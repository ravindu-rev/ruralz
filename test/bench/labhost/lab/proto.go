// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// MaxLine bounds one protocol line (a LoadSpec with its keys, a
// LoadResult with its interval log).
const MaxLine = 64 << 20

// Operations of a Request.
const (
	// OpStats answers the service's counters and its CPU share since the
	// last reset.
	OpStats = "stats"
	// OpReset starts a new measurement: counters, histograms and the CPU
	// share start over.
	OpReset = "reset"
	// OpWindow declares a sink fold window [Start, End) named Name.
	OpWindow = "window"
	// OpResult answers what the sink folded into window Name.
	OpResult = "result"
	// OpRemove drops sink window Name.
	OpRemove = "remove"
	// OpMode switches a service's mode: the sink's answer to exports, Name
	// "stall" holding every export until the next "accept", so the Node's
	// telemetry queues fill (the constants runs' queue bounds); the fault
	// proxy between "pass" and its configured "delay" (O2).
	OpMode = "mode"
)

// Request is one command line on a service's standard input.
type Request struct {
	Op    string    `json:"op"`
	Name  string    `json:"name,omitempty"`
	Start time.Time `json:"start,omitzero"`
	End   time.Time `json:"end,omitzero"`
	// Where, on a sink window, folds only the access records whose
	// attribute of that name is present and not empty (failure_modes).
	Where string `json:"where,omitempty"`
}

// Ready is the first line a service writes: what it serves on.
type Ready struct {
	Service string `json:"service"`
	// Addr is the service's listen address (host:port).
	Addr string `json:"addr"`
	// PID is the service process.
	PID int `json:"pid"`
	// CPUs is the CPU count the process may run on (its cpuset).
	CPUs int `json:"cpus"`
}

// Response answers one Request, or ends the session (the last line after
// standard input ended).
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Process is the service's CPU since the last reset.
	Process ProcessStats `json:"process"`
	Mock    *MockStats   `json:"mock,omitempty"`
	Sink    *SinkStats   `json:"sink,omitempty"`
	Window  *Window      `json:"window,omitempty"`
	Proxy   *ProxyStats  `json:"proxy,omitempty"`
	Conns   *ConnsStats  `json:"conns,omitempty"`
}

// ProcessStats is a process's CPU over a wall-clock interval.
type ProcessStats struct {
	// CPUSeconds is the user and system CPU time of the interval.
	CPUSeconds float64 `json:"cpuSeconds"`
	// WallSeconds is the interval's length.
	WallSeconds float64 `json:"wallSeconds"`
	// CPUs is the CPU count of the process's cpuset.
	CPUs int `json:"cpus"`
}

// Share returns the CPU share of the cpuset, 0 to 1 (more when the
// process used more cores than its cpuset holds, which a cpuset forbids).
func (p ProcessStats) Share() float64 {
	if p.WallSeconds <= 0 || p.CPUs <= 0 {
		return 0
	}
	return p.CPUSeconds / (p.WallSeconds * float64(p.CPUs))
}

// cpuMeter measures the own process's CPU since its last reset.
type cpuMeter struct {
	mu    sync.Mutex
	cpu   time.Duration
	start time.Time
}

// selfCPU reads the process's own user and system CPU time; 0 where /proc
// is missing.
func selfCPU() time.Duration {
	u, err := proc.SampleProc("/proc", os.Getpid())
	if err != nil {
		return 0
	}
	return u.UserCPU + u.SysCPU
}

func newCPUMeter() *cpuMeter { return &cpuMeter{cpu: selfCPU(), start: time.Now()} }

// reset starts a new interval.
func (m *cpuMeter) reset() {
	m.mu.Lock()
	m.cpu, m.start = selfCPU(), time.Now()
	m.mu.Unlock()
}

// stats returns the interval so far.
func (m *cpuMeter) stats() ProcessStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ProcessStats{
		CPUSeconds:  (selfCPU() - m.cpu).Seconds(),
		WallSeconds: time.Since(m.start).Seconds(),
		CPUs:        runtime.NumCPU(),
	}
}

// Service is one long-running lab service.
type Service interface {
	// Ready describes what the service serves on.
	Ready() Ready
	// Handle answers one Request.
	Handle(ctx context.Context, r Request) Response
	// Close stops the service and waits for its goroutines.
	Close() error
}

// Serve runs the protocol of svc on in and out: the Ready line, then one
// Response line per Request line, until in ends or ctx ends; then it
// closes svc and writes a final Response with the last counters. Lines
// that are not Requests get an error Response.
func Serve(ctx context.Context, svc Service, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	if err := enc.Encode(svc.Ready()); err != nil {
		_ = svc.Close()
		return fmt.Errorf("lab: write the ready line: %w", err)
	}
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	stop := make(chan struct{})
	var reader sync.WaitGroup
	reader.Go(func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64<<10), MaxLine)
		for sc.Scan() {
			select {
			case lines <- append([]byte(nil), sc.Bytes()...):
			case <-stop:
				return
			}
		}
		readErr <- sc.Err()
	})
	defer func() {
		close(stop)
		// The reader ends at the end of in, which the caller owns: a
		// closed pipe or the end of the ssh session.
	}()
	for {
		select {
		case <-ctx.Done():
			return finish(svc, enc)
		case err := <-readErr:
			if ferr := finish(svc, enc); ferr != nil {
				return ferr
			}
			return err
		case line := <-lines:
			var r Request
			resp := Response{}
			if err := json.Unmarshal(line, &r); err != nil {
				resp.Error = "lab: not a request: " + err.Error()
			} else {
				resp = svc.Handle(ctx, r)
			}
			if err := enc.Encode(resp); err != nil {
				_ = svc.Close()
				return fmt.Errorf("lab: write a response: %w", err)
			}
		}
	}
}

// finish closes svc and writes the session's last counters.
func finish(svc Service, enc *json.Encoder) error {
	last := svc.Handle(context.Background(), Request{Op: OpStats})
	err := svc.Close()
	if err != nil {
		last.OK, last.Error = false, err.Error()
	}
	return errors.Join(err, enc.Encode(last))
}

// Client drives a Service over its standard input and output.
type Client struct {
	mu  sync.Mutex
	in  io.Writer
	out *bufio.Scanner
	// Ready is the service's Ready line.
	Ready Ready
}

// NewClient reads the Ready line from out and returns the Client writing
// Requests to in.
func NewClient(in io.Writer, out io.Reader) (*Client, error) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLine)
	c := &Client{in: in, out: sc}
	if !sc.Scan() {
		return nil, fmt.Errorf("lab: no ready line: %w", errors.Join(sc.Err(), io.ErrUnexpectedEOF))
	}
	if err := json.Unmarshal(sc.Bytes(), &c.Ready); err != nil || c.Ready.Service == "" {
		return nil, fmt.Errorf("lab: bad ready line %q: %v", sc.Text(), err)
	}
	return c, nil
}

// Do sends r and returns the Response; a Response that is not OK is an
// error.
func (c *Client) Do(r Request) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.Marshal(r)
	if err != nil {
		return Response{}, err
	}
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return Response{}, fmt.Errorf("lab: send %s: %w", r.Op, err)
	}
	if !c.out.Scan() {
		return Response{}, fmt.Errorf("lab: no response to %s: %w", r.Op, errors.Join(c.out.Err(), io.ErrUnexpectedEOF))
	}
	var resp Response
	if err := json.Unmarshal(c.out.Bytes(), &resp); err != nil {
		return Response{}, fmt.Errorf("lab: bad response to %s: %w", r.Op, err)
	}
	if !resp.OK {
		return resp, fmt.Errorf("lab: %s: %s", r.Op, resp.Error)
	}
	return resp, nil
}

// Last reads the final Response a service writes once its input ended.
func (c *Client) Last() (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.out.Scan() {
		return Response{}, fmt.Errorf("lab: no final response: %w", errors.Join(c.out.Err(), io.ErrUnexpectedEOF))
	}
	var resp Response
	if err := json.Unmarshal(c.out.Bytes(), &resp); err != nil {
		return Response{}, fmt.Errorf("lab: bad final response: %w", err)
	}
	return resp, nil
}

// EncodeHistogram returns h in the compressed HdrHistogram V2 encoding,
// base64; "" for nil.
func EncodeHistogram(h *loadgen.Histogram) string {
	if h == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(h.Encode())
}

// DecodeHistogram decodes EncodeHistogram's output; "" is an empty
// histogram at the run settings.
func DecodeHistogram(s string) (*loadgen.Histogram, error) {
	if s == "" {
		return loadgen.NewHistogram(loadgen.DefaultLowest, loadgen.DefaultHighest, loadgen.DefaultDigits)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("lab: histogram: %w", err)
	}
	return loadgen.DecodeHistogram(b)
}
