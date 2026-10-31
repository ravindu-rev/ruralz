// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// ConstantsAPIKey is the constants runs' API key: the constants Bundle's
// Consumer bench holds its SHA-256 hash, and the Quota denial cache run
// sends it as x-api-key.
const ConstantsAPIKey = "ruralz-bench-constants-api-key" //nolint:gosec // G101: the benchmark Bundle's published test key, not a credential

// Constants the runs verify (PBB "Constants the suite verifies"), targets.
const (
	// PreRoutingDeadline is the pre-routing write deadline: 5 s.
	PreRoutingDeadline = 5 * time.Second
	// ReadHeaderTimeout is the Node's http.Server header deadline, 10 s,
	// which ends the slow-header flood.
	ReadHeaderTimeout = 10 * time.Second
	// TimeoutSlack is how late past its value an expiry may come: 2 s.
	TimeoutSlack = 2 * time.Second
	// ConnectionMemory is the per-connection memory hypothesis: about
	// 592 KiB before a handler runs.
	ConnectionMemory = 592 << 10
	// HeaderCeiling is the Node's header block ceiling, 256 KiB; the
	// slow-header flood holds blocks just under it.
	HeaderCeiling = 256 << 10
)

// Paths of the constants Bundle.
const (
	pathNoRoute = "/no-route"
	pathFlood   = "/flood"
	pathCache   = "/cache/flood"
	pathQuota   = "/quota"
)

// Codes the constants runs read.
const (
	codeQuotaDenied = "RZ-RL-003"
)

// RunConstants runs the constants runs of slot 4 (PBB "Constants the suite
// verifies"): the pre-routing writes, the body flood, K = 2 under
// activations, the span and access-log queues at both trace caps, the
// Quota denial cache, the cache key flood, the ring-hash Upstream ladder
// and the per-connection memory flood. Rows a CE experiment verifies run
// in the Chaos job, and the frame limits and the timeout cases are the
// protocol suite's.
func RunConstants(ctx context.Context, e Env, s Scale) (*Result, error) {
	if err := e.needFiles(ItemConstants, max(s.Constants.FloodConnections, s.Constants.PreRoutingClients)); err != nil {
		return nil, err
	}
	res := &Result{}
	runs := []func(context.Context, Scale, *Result) error{
		e.preRouting, e.bodyFlood, e.activations, e.traceCaps, e.quotaFlood, e.cacheFlood, e.ringLadder, e.connMemory,
	}
	for _, run := range runs {
		if err := run(ctx, s, res); err != nil {
			return nil, err
		}
	}
	return res.finish(), nil
}

// constantsScenario is a constants run over the constants Bundle, offering
// S1's workload to bench-1k at rate with loadgen.
func constantsScenario(id string, rate float64) (harness.Scenario, error) {
	s1, err := catalog("S1")
	if err != nil {
		return harness.Scenario{}, err
	}
	w := s1.Workload
	w.Tool, w.Rate = harness.ToolLoadgen, rate
	return scenario(id, DirConstants, w, s1.Saturation), nil
}

// action is what a constants run does to its Node while background
// traffic runs; it returns the run's criteria.
type action func(ctx context.Context, st *harness.Stage) func(*harness.RunOutcome) []benchrecord.BudgetResult

// backgroundRun prepares sc's stage and runs act while S1's workload
// offers background traffic for dur, measured at once; the record holds
// the background run's measures, act's criteria and that no background
// request failed.
func (e Env) backgroundRun(ctx context.Context, sc harness.Scenario, dur time.Duration, act action, res *Result) error {
	if err := e.check(ctx, sc); err != nil {
		return err
	}
	rev, err := harness.Render(ctx, e.Head.Ruralz, e.S.RepoRoot, sc, e.S.Profile)
	if err != nil {
		return err
	}
	return e.backgroundOn(ctx, sc, rev, harness.NodeSpec{}, dur, act, res)
}

// backgroundOn is backgroundRun on a rendered Revision and Node settings.
func (e Env) backgroundOn(ctx context.Context, sc harness.Scenario, rev harness.Revision, ns harness.NodeSpec, dur time.Duration, act action, res *Result) error {
	spec := harness.RunSpec{Scenario: sc, Build: e.Head, Revision: rev, Name: "run-1", Warmup: -1, Duration: dur, NoSettle: true, Sample: sampleEvery, Node: ns}
	st, err := e.S.Prepare(ctx, spec)
	if err != nil {
		return err
	}
	defer st.Close()
	done := make(chan func(*harness.RunOutcome) []benchrecord.BudgetResult, 1)
	go func() { done <- act(ctx, st) }()
	spec.Criteria = func(o *harness.RunOutcome) []benchrecord.BudgetResult {
		crit := <-done
		out := []benchrecord.BudgetResult{judge(sc.ID+"-background", "background requests all answered meanwhile", failedRequests(o.Load) == 0,
			fmt.Sprintf("%d of %d failed", failedRequests(o.Load), offered(o.Load)))}
		return append(out, crit(o)...)
	}
	out, err := e.S.Measure(ctx, st, spec)
	if err != nil {
		return err
	}
	res.add(sc.ID, out.Path, criteriaOf(out.Record))
	if !out.Valid {
		res.Invalid = append(res.Invalid, sc.ID+": "+strings.Join(out.Record.Validity.Reasons, "; "))
	}
	return nil
}

// fixed returns criteria that ignore the run outcome.
func fixed(rs ...benchrecord.BudgetResult) func(*harness.RunOutcome) []benchrecord.BudgetResult {
	return func(*harness.RunOutcome) []benchrecord.BudgetResult { return rs }
}

// failedAction returns an action's single failed criterion for err.
func failedAction(id, value string, err error) func(*harness.RunOutcome) []benchrecord.BudgetResult {
	return fixed(judge(id, value, false, err.Error()))
}

// listenerAddr returns the gateway host's address of a scenario port.
func (e Env) listenerAddr(port int) string {
	return net.JoinHostPort(gatewayHost(e.S), strconv.Itoa(port))
}

// preRouting runs the pre-routing writes: PreRoutingClients h2c clients
// each ask for a path no Route matches with a zero stream window, so the
// Node's RZ-RT-001 write blocks; past 2,000 such writes at once the extra
// handlers abort without writing, and each blocked write expires at the
// 5 s pre-routing write deadline.
func (e Env) preRouting(ctx context.Context, s Scale, res *Result) error {
	sc, err := constantsScenario("constants-pre-routing", s.Constants.BackgroundRate)
	if err != nil {
		return err
	}
	n := s.Constants.PreRoutingClients
	return e.backgroundRun(ctx, sc, 3*PreRoutingDeadline, func(ctx context.Context, st *harness.Stage) func(*harness.RunOutcome) []benchrecord.BudgetResult {
		const abortID, abortValue = "constants-pre-routing-abort", "past 2,000 pre-routing writes at once the extra handlers abort (target)"
		cc := lab.ConnsConfig{Kind: lab.ConnsStallH2C, Target: e.listenerAddr(portHTTP), Count: n, Host: harness.NameGateway, Path: pathNoRoute}
		p, err := st.Lab().StartConns(ctx, "conns-pre-routing", cc)
		if err != nil {
			return failedAction(abortID, abortValue, err)
		}
		if _, err := waitConns(ctx, p, connsWait(cc)); err != nil {
			return failedAction(abortID, abortValue, err)
		}
		if err := sleepCtx(ctx, PreRoutingDeadline+TimeoutSlack+time.Second); err != nil {
			return failedAction(abortID, abortValue, err)
		}
		cs, err := connsStats(p)
		if err != nil {
			return failedAction(abortID, abortValue, err)
		}
		return fixed(
			judge(abortID, abortValue, cs.Aborted > 0 && cs.Responded <= PreRoutingWrites && cs.Responded+cs.Aborted == int64(n),
				fmt.Sprintf("%d of %d clients got RZ-RT-001's header, %d were aborted without a write", cs.Responded, n, cs.Aborted)),
			window("constants-pre-routing-deadline", "each blocked pre-routing write expires within 5s to 7s (target)", cs.ExpiredAfter, cs.Expired, PreRoutingDeadline),
		)
	}, res)
}

// window judges an expiry histogram: count expiries, each between value
// and value plus the slack.
func window(id, value, hist string, count int64, d time.Duration) benchrecord.BudgetResult {
	h, err := lab.DecodeHistogram(hist)
	if err != nil {
		return judge(id, value, false, err.Error())
	}
	if count == 0 || h.Count() == 0 {
		return judge(id, value, false, "no expiry observed")
	}
	// The histogram keeps three significant digits: a value read back may
	// sit up to one part in a thousand below the one recorded.
	lo, hi := h.Min(), h.Max()
	return judge(id, value, lo >= d-d/1000 && hi <= d+TimeoutSlack, fmt.Sprintf("%d expiries from %s to %s", h.Count(), lo, hi))
}

// heldWait bounds how long the body flood keeps its bodies paused until a
// scrape shows the bytes they hold.
const heldWait = 10 * time.Second

// bodyFlood runs the body flood: Bodies POSTs of BodyBytes each into the
// gated flood Route at once, together above the 512 MiB buffer budget, each
// paused at 90% of its body until every one has reached that point or been
// refused and a scrape has shown the bytes the Node holds for them:
// reservations past the budget get 503 RZ-RT-004, the buffered bytes never
// pass 512 MiB and the Node stays up. /metrics reuses one collection for
// up to a second, longer than the pause lasts on a fast host, so bodies
// released at once could leave every scrape before or after the flood.
func (e Env) bodyFlood(ctx context.Context, s Scale, res *Result) error {
	sc, err := constantsScenario("constants-body-flood", s.Constants.BackgroundRate)
	if err != nil {
		return err
	}
	return e.backgroundRun(ctx, sc, 30*time.Second, func(ctx context.Context, st *harness.Stage) func(*harness.RunOutcome) []benchrecord.BudgetResult {
		node := st.Node()
		stop := make(chan struct{})
		peak := make(chan float64, 1)
		// seen closes at the first scrape that shows buffered bytes.
		seen := make(chan struct{})
		go func() {
			var hi float64
			defer func() { peak <- hi }()
			for {
				if m, err := node.Scrape(ctx); err == nil {
					v := m.Sum(metricBuffered, nil)
					if v > 0 && hi == 0 {
						close(seen)
					}
					hi = max(hi, v)
				}
				select {
				case <-stop:
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
		}()
		tr := &http.Transport{MaxConnsPerHost: s.Constants.Bodies, MaxIdleConnsPerHost: s.Constants.Bodies, DisableCompression: true}
		codes, statuses, err := flood(ctx, tr, "http://"+e.listenerAddr(portHTTP)+pathFlood, s.Constants.Bodies, s.Constants.BodyBytes, func() {
			t := time.NewTimer(heldWait)
			defer t.Stop()
			select {
			case <-seen:
			case <-t.C:
			case <-ctx.Done():
			}
		})
		close(stop)
		hi := <-peak
		if err != nil {
			return failedAction("constants-body-flood-rejects", "a reservation past the buffer budget gets 503 RZ-RT-004 (target)", err)
		}
		alive := !node.Exited()
		return fixed(
			judge("constants-body-flood-rejects", "a reservation past the buffer budget gets 503 RZ-RT-004 (target)", codes[codeBufferFull] > 0,
				fmt.Sprintf("%d of %d bodies refused with RZ-RT-004; statuses %v", codes[codeBufferFull], s.Constants.Bodies, statuses)),
			judge("constants-body-flood-budget", "buffered bytes never above limits.maxBufferedBytes, 512 MiB (target)", hi <= BufferedBytes && hi > 0,
				"highest ruralz_node_buffered_bytes "+mib(int64(hi))),
			judge("constants-body-flood-alive", "the Node keeps running through the flood, no OOM", alive, fmt.Sprintf("running %v", alive)),
		)
	}, res)
}

// flood sends n POSTs of size bytes each to url through rt at once, each
// paused at 90% of its body. Once every body has paused or its request
// ended, held runs, and the bodies go on only after it returns. flood
// returns the RZ codes and statuses of the answers.
func flood(ctx context.Context, rt http.RoundTripper, url string, n int, size int64, held func()) (map[string]int, map[int]int, error) {
	release := make(chan struct{})
	var reached sync.WaitGroup
	reached.Add(n)
	client := &http.Client{Transport: rt, Timeout: 2 * time.Minute}
	defer client.CloseIdleConnections()
	type answer struct {
		status int
		code   string
		err    error
	}
	answers := make(chan answer, n)
	for range n {
		go func() {
			body := &pausedBody{size: size, pauseAt: size * 9 / 10, reached: reached.Done, release: release}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
			if err != nil {
				body.reach()
				answers <- answer{err: err}
				return
			}
			req.ContentLength = size
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			body.reach()
			if err != nil {
				answers <- answer{err: err}
				return
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			answers <- answer{status: resp.StatusCode, code: problemCode(b)}
		}()
	}
	waited := make(chan struct{})
	go func() { reached.Wait(); close(waited) }()
	select {
	case <-waited:
		held()
	case <-time.After(time.Minute):
	case <-ctx.Done():
	}
	close(release)
	codes, statuses := map[string]int{}, map[int]int{}
	var errs []error
	for range n {
		a := <-answers
		if a.err != nil {
			errs = append(errs, a.err)
			continue
		}
		statuses[a.status]++
		if a.code != "" {
			codes[a.code]++
		}
	}
	if len(errs) == n {
		return nil, nil, errors.Join(errs...)
	}
	return codes, statuses, nil
}

// pausedBody is a JSON object body {"a":"aaa…"} of size bytes that blocks
// at pauseAt until release closes, calling reached once when it blocks or
// its request ends.
type pausedBody struct {
	size, pauseAt, sent int64
	reached             func()
	once                sync.Once
	release             <-chan struct{}
}

func (b *pausedBody) reach() { b.once.Do(b.reached) }

// Read fills p with the body's next bytes.
func (b *pausedBody) Read(p []byte) (int, error) {
	if b.sent >= b.size {
		return 0, io.EOF
	}
	if b.sent >= b.pauseAt {
		b.reach()
		<-b.release
	}
	n := int64(len(p))
	if limit := b.pauseAt - b.sent; b.sent < b.pauseAt && n > limit {
		n = limit
	}
	n = min(n, b.size-b.sent)
	for i := range n {
		p[i] = b.byteAt(b.sent + i)
	}
	b.sent += n
	return int(n), nil
}

// byteAt returns the body's byte at offset i.
func (b *pausedBody) byteAt(i int64) byte {
	const head = `{"a":"`
	switch {
	case i < int64(len(head)):
		return head[i]
	case i == b.size-2:
		return '"'
	case i == b.size-1:
		return '}'
	default:
		return 'a'
	}
}

// Close is a no-op.
func (*pausedBody) Close() error { return nil }

// problemCode returns a problem document's code member, "" for another
// body.
func problemCode(b []byte) string {
	const key = `"code":"`
	i := strings.Index(string(b), key)
	if i < 0 {
		return ""
	}
	rest := string(b[i+len(key):])
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// activations runs K = 2 under activations: S2's load while the Node's
// source alternates between two Revisions, each placed and loaded with
// SIGHUP ReloadRate times per second. No unary request fails, at most K
// retired snapshots plus one closing or ending are kept, and on RH-1 the
// peak configuration memory stays within (K + 2) times the snapshot.
func (e Env) activations(ctx context.Context, s Scale, res *Result) error {
	s2, err := catalog("S2")
	if err != nil {
		return err
	}
	c := s.Constants
	w := s2.Workload
	w.Rate = c.ReloadLoad
	sc := scenario("constants-activations", "s2", w, s2.Saturation)
	if err := e.check(ctx, sc); err != nil {
		return err
	}
	rev, err := harness.Render(ctx, e.Head.Ruralz, e.S.RepoRoot, sc, e.S.Profile)
	if err != nil {
		return err
	}
	dir := filepath.Join(e.S.Work, "activations")
	vs, err := s2Variants(ctx, e.Head.Ruralz, rev, dir)
	if err != nil {
		return err
	}
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o750); err != nil {
		return err
	}
	spec := harness.RunSpec{Scenario: sc, Build: e.Head, Revision: rev, Name: "run-1", Warmup: -1, Duration: c.ReloadDuration, NoSettle: true, Sample: 200 * time.Millisecond}
	st, err := e.S.Prepare(ctx, spec)
	if err != nil {
		return err
	}
	defer st.Close()
	node, err := st.StartNode(ctx, harness.NodeSpec{Rendered: cfg, NoRevision: true})
	if err != nil {
		return err
	}
	heap0, err := node.HeapAlloc(ctx)
	if err != nil {
		return err
	}
	if _, err := activate(ctx, node, cfg, vs[0]); err != nil {
		return err
	}
	heap1, err := node.HeapAlloc(ctx)
	if err != nil {
		return err
	}
	snapshot := heap1 - heap0
	rssBefore, err := node.RSS()
	if err != nil {
		return err
	}
	if err := node.ResetPeak(); err != nil {
		return err
	}
	before, err := node.Scrape(ctx)
	if err != nil {
		return err
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var placed int
	var placeErr error
	wg.Go(func() {
		t := time.NewTicker(time.Duration(float64(time.Second) / c.ReloadRate))
		defer t.Stop()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := place(cfg, vs[i%2].file); err != nil {
				placeErr = err
				return
			}
			if err := node.Signal(syscall.SIGHUP); err != nil {
				placeErr = err
				return
			}
			placed++
		}
	})
	spec.Criteria = func(o *harness.RunOutcome) []benchrecord.BudgetResult {
		close(stop)
		wg.Wait()
		after, err := node.Scrape(ctx)
		activated := promtext.Delta(before, after, metricActivations, promtext.L("result", "activated"))
		if err != nil {
			activated = 0
		}
		peak, perr := node.Peak()
		win := inWindow(o.Samples, o.MeasureStart, o.MeasureEnd)
		retired, n := gaugeMax(win, metricRetired, nil)
		out := []benchrecord.BudgetResult{
			judge("constants-activations-failures", "zero failed unary requests under ten activations per second (target)",
				failedRequests(o.Load) == 0 && placeErr == nil,
				fmt.Sprintf("%d of %d requests failed; %d sources placed, %s activated%s", failedRequests(o.Load), offered(o.Load), placed, fmtCount(activated), errSuffix(placeErr))),
			judge("constants-retired-snapshots", "at most K = 2 retired snapshots plus one closing or ending (target)",
				n > 0 && retired <= RetiredSnapshots+1, fmt.Sprintf("highest ruralz_config_retired_snapshots %s over %d samples", fmtCount(retired), n)),
			judge("constants-activations-activated", "the activations take effect under load", activated >= 1, fmtCount(activated)+" activations"),
		}
		peakResult := judge("constants-activations-peak", "peak configuration memory within (K + 2) x the snapshot (target)", false, "no peak reading")
		if perr == nil {
			peakResult = bytesBound("constants-activations-peak", "peak configuration memory within (K + 2) x the snapshot (target)",
				peak-rssBefore, int64(RetiredSnapshots+2)*snapshot)
		}
		return append(out, s.hardware(peakResult))
	}
	out, err := e.S.Measure(ctx, st, spec)
	if err != nil {
		return err
	}
	res.add(sc.ID, out.Path, criteriaOf(out.Record))
	if !out.Valid {
		res.Invalid = append(res.Invalid, sc.ID+": "+strings.Join(out.Record.Validity.Reasons, "; "))
	}
	return nil
}

// s2Variants returns the two Revisions the activations run alternates:
// S2's render, and the same with bench-1k's timeout at 6 s instead of
// 5 s, each with its digest.
func s2Variants(ctx context.Context, ruralz string, rev harness.Revision, dir string) ([2]r1Variant, error) {
	var vs [2]r1Variant
	b, err := os.ReadFile(rev.Rendered)
	if err != nil {
		return vs, err
	}
	from, to, err := routeTimeout(b)
	if err != nil {
		return vs, err
	}
	for i, body := range [][]byte{b, []byte(strings.Replace(string(b), from, to, 1))} {
		src := filepath.Join(dir, "variant-"+strconv.Itoa(i))
		if err := os.MkdirAll(src, 0o750); err != nil {
			return vs, err
		}
		if err := os.WriteFile(filepath.Join(src, "ruralz.yaml"), body, 0o600); err != nil { //nolint:gosec // G703: S2's render, edited, under the run's directory
			return vs, err
		}
		r, err := renderBundle(ctx, ruralz, src, filepath.Join(dir, "rendered-"+strconv.Itoa(i)+".yaml"))
		if err != nil {
			return vs, err
		}
		vs[i] = r1Variant{rev: r, file: r.Rendered}
	}
	if vs[0].rev.Digest != rev.Digest {
		return vs, fmt.Errorf("scenarios: S2's render rebuilt as %s, want %s", vs[0].rev.Digest, rev.Digest)
	}
	return vs, nil
}

// trafficStart is the requests the Node answers before a run that times
// an action from its load's start counts the load as started.
const trafficStart = 100

// waitTraffic waits until the Node has answered n requests more than in
// before, polling past the metrics reuse window, or fails after limit.
func waitTraffic(ctx context.Context, node *harness.Node, before harness.NodeSample, n float64, limit time.Duration) error {
	if before.Err != nil {
		return before.Err
	}
	base := before.Metrics.Sum(metricRequests, nil)
	deadline := time.Now().Add(limit)
	prev := before
	for {
		cur, err := sampleAfter(ctx, node, prev)
		if err != nil {
			return err
		}
		if cur.Err == nil && cur.Metrics.Sum(metricRequests, nil)-base >= n {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("scenarios: the Node answered fewer than %v requests of the load within %v", n, limit)
		}
		prev = cur
	}
}

// routeTimeout returns the field of bench-1k's 5 s timeout in S2's render
// and its 6 s replacement: ruralz bundle render writes the duration
// quoted, a hand-written Bundle may leave it bare, and the render must
// hold exactly one.
func routeTimeout(render []byte) (from, to string, err error) {
	for _, q := range []string{`"`, ""} {
		f := "timeout: " + q + "5s" + q
		if n := strings.Count(string(render), f); n == 1 {
			return f, "timeout: " + q + "6s" + q, nil
		} else if n > 1 {
			return "", "", fmt.Errorf("scenarios: S2's render holds %d %s, want one", n, f)
		}
	}
	return "", "", errors.New(`scenarios: S2's render holds no 5 s timeout, want bench-1k's "timeout: 5s"`)
}

// traceCaps runs the span and access-log queues at both trace caps: S2 on
// the s2-trace-caps Bundle, traceSampling 1, half the load carrying a
// sampled traceparent, both above the root and parent caps, with the OTLP
// sink stalled for the middle third of the run. Both caps bind, the full
// queues drop spans and access records with reason queue_full, and no
// request fails.
func (e Env) traceCaps(ctx context.Context, s Scale, res *Result) error {
	s2, err := catalog("S2")
	if err != nil {
		return err
	}
	_, dur, _ := e.lengths(s)
	if dur == 0 {
		dur = harness.Measured
	}
	w := s2.Workload
	w.Rate = s.Constants.TraceRate / 2
	sc := scenario("constants-trace-caps", DirS2TraceCaps, w, s2.Saturation)
	if err := e.check(ctx, sc); err != nil {
		return err
	}
	rev, err := harness.Render(ctx, e.Head.Ruralz, e.S.RepoRoot, sc, e.S.Profile)
	if err != nil {
		return err
	}
	spec := harness.RunSpec{Scenario: sc, Build: e.Head, Revision: rev, Name: "run-1"}
	st, err := e.S.Prepare(ctx, spec)
	if err != nil {
		return err
	}
	defer st.Close()
	node := st.Node()
	parented := w
	parented.Header = map[string][]string{"traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}
	before := sample(ctx, node)
	started := time.Now().UTC().Truncate(time.Millisecond)
	type loaded struct {
		r   *lab.LoadResult
		err error
	}
	results := make(chan loaded, 2)
	for _, lw := range []harness.Workload{w, parented} {
		go func() {
			ls, err := st.Lab().LoadSpec(lw, gatewayHost(e.S), 0, dur, false)
			if err != nil {
				results <- loaded{err: err}
				return
			}
			r, err := st.Lab().RunLoad(ctx, ls)
			results <- loaded{r: r, err: err}
		}()
	}
	// The sink stalls a third of the load long, from a quarter into it: the
	// stall starts once the Node answers the load, since the generator
	// mints S2's bearer tokens before its first request, so a stall timed
	// from the launch could end before any traffic queued.
	stallErr := func() error {
		if err := waitTraffic(ctx, node, before, trafficStart, dur); err != nil {
			return err
		}
		if err := sleepCtx(ctx, dur/4); err != nil {
			return err
		}
		if err := st.Lab().SinkMode(lab.SinkStall); err != nil {
			return err
		}
		stalled := sleepCtx(ctx, dur/3)
		return errors.Join(stalled, st.Lab().SinkMode(lab.SinkAccept))
	}()
	var loads []*lab.LoadResult
	var errs []error
	for range 2 {
		l := <-results
		if l.err != nil {
			errs = append(errs, l.err)
			continue
		}
		loads = append(loads, l.r)
	}
	if err := errors.Join(append(errs, stallErr)...); err != nil {
		return err
	}
	after := sample(ctx, node)
	delta := func(name string, match promtext.Labels) float64 {
		if before.Err != nil || after.Err != nil {
			return 0
		}
		return promtext.Delta(before.Metrics, after.Metrics, name, match)
	}
	var failed, total int64
	for _, l := range loads {
		failed += failedRequests(l)
		total += offered(l)
	}
	budgets := []benchrecord.BudgetResult{
		judge("constants-trace-root-cap", "the root cap of 1,000 sampled traces per second binds (target)", delta(metricUnsampled, promtext.L("reason", "rate_cap_root")) > 0,
			fmtCount(delta(metricUnsampled, promtext.L("reason", "rate_cap_root")))+" traces unsampled by the root cap"),
		judge("constants-trace-parent-cap", "the parent cap of 500 sampled traces per second binds (target)", delta(metricUnsampled, promtext.L("reason", "rate_cap_parent")) > 0,
			fmtCount(delta(metricUnsampled, promtext.L("reason", "rate_cap_parent")))+" traces unsampled by the parent cap"),
		judge("constants-span-queue", "the span queue holds 8,192 spans and drops the excess with queue_full (target)", delta(metricSpansDropped, promtext.L("reason", "queue_full")) > 0,
			fmtCount(delta(metricSpansDropped, promtext.L("reason", "queue_full")))+" spans dropped with queue_full"),
		judge("constants-log-queue", "the access-log queue holds 8,192 records and 4 MiB and drops the excess with queue_full (target)",
			delta(metricLogsDropped, promtext.L("stream", "access", "sink", "otlp", "reason", "queue_full")) > 0,
			fmtCount(delta(metricLogsDropped, promtext.L("stream", "access", "sink", "otlp", "reason", "queue_full")))+" access records dropped with queue_full"),
		judge("constants-trace-failures", "no request fails while the queues drop", failed == 0, fmt.Sprintf("%d of %d failed", failed, total)),
	}
	rec := e.S.BaseRecord(sc, spec, started, benchrecord.KindMacro)
	rec.Load = benchrecord.Load{
		Tool: string(harness.ToolLoadgen), Version: e.S.ToolVersions[harness.ToolLoadgen], CommandLine: "two loadgen runs of S2's workload, one with a sampled traceparent",
		OfferedRate: s.Constants.TraceRate, AchievedRate: achievedRate(loads), Connections: 2 * w.Connections,
		KeyCardinality: harness.BenchKeyCardinality, KeyDistribution: "cycle", InjectedDelay: "none",
	}
	if after.Err == nil {
		rec.Resources.RSSLoadBytes = after.RSS
	}
	path, err := e.writeRecord(rec, "run-1", budgets, nil)
	if err != nil {
		return err
	}
	res.add(sc.ID, path, budgets)
	return nil
}

// achievedRate sums the loads' achieved rates.
func achievedRate(loads []*lab.LoadResult) float64 {
	var r float64
	for _, l := range loads {
		r += l.AchievedRate
	}
	return r
}

// quotaFlood runs the Quota denial cache run: QuotaKeys keys each sent
// twice, as Consumer bench with its one-request quota, so every key's
// second request is a denied Quota key, beyond the denial cache's 65,536
// entries: each first request is admitted and each second one denied with
// RZ-RL-003, however many keys are denied.
func (e Env) quotaFlood(ctx context.Context, s Scale, res *Result) error {
	c := s.Constants
	sc, err := constantsScenario("constants-quota", c.QuotaRate)
	if err != nil {
		return err
	}
	sc.Workload.Path, sc.Workload.KeyHeader = pathQuota, "x-bench-key"
	sc.Workload.Keys = &lab.KeySpec{Kind: lab.KeysCycle, Prefix: "quota-", N: c.QuotaKeys}
	sc.Workload.Header = map[string][]string{"x-api-key": {ConstantsAPIKey}}
	sc.Workload.Count = []string{codeQuotaDenied}
	dur := time.Duration(float64(2*c.QuotaKeys) / c.QuotaRate * float64(time.Second))
	err = e.run(ctx, sc, harness.RunSpec{
		Warmup: -1, Duration: dur, NoSettle: true, Sample: sampleEvery,
		Criteria: func(o *harness.RunOutcome) []benchrecord.BudgetResult {
			r := o.Load
			if r == nil {
				return []benchrecord.BudgetResult{judge("constants-quota-denials", "every denied Quota key stays denied (target)", false, "no load result")}
			}
			first := min(r.Offered, int64(c.QuotaKeys))
			ok := r.Statuses[http.StatusOK]
			denied := r.Codes[codeQuotaDenied]
			return []benchrecord.BudgetResult{judge("constants-quota-denials",
				fmt.Sprintf("each of %d keys admitted once and denied after, beyond the 65,536-entry denial cache (target)", c.QuotaKeys),
				ok == first && denied == r.Offered-first,
				fmt.Sprintf("%d admitted, %d denied with RZ-RL-003 of %d", ok, denied, r.Offered))}
		},
	}, res)
	return err
}

// cacheFlood runs the cache key flood: each of CacheKeys keys read twice in
// a row at CacheRate, the mock's responses cacheable and varying on the
// key, so every key is its own Response Cache entry and the Node's
// hot-entry layer fills and evicts; every request is answered.
func (e Env) cacheFlood(ctx context.Context, s Scale, res *Result) error {
	c := s.Constants
	sc, err := constantsScenario("constants-cache", c.CacheRate)
	if err != nil {
		return err
	}
	sc.Workload.Path, sc.Workload.KeyHeader, sc.Workload.BodySize = pathCache, "x-bench-key", c.CacheBody
	sc.Workload.Keys = &lab.KeySpec{Kind: lab.KeysRotate, Prefix: "entry-", PerKey: 2}
	sc.Workload.MockHeader = []string{"Cache-Control: public, max-age=60", "Vary: X-Bench-Key"}
	dur := time.Duration(float64(2*c.CacheKeys) / c.CacheRate * float64(time.Second))
	err = e.run(ctx, sc, harness.RunSpec{
		Warmup: -1, Duration: dur, NoSettle: true, Sample: sampleEvery,
		Criteria: func(o *harness.RunOutcome) []benchrecord.BudgetResult {
			win := good(o.Samples)
			hits := counterDelta(win, metricCache, promtext.L("result", "hit"))
			misses := counterDelta(win, metricCache, promtext.L("result", "miss"))
			return []benchrecord.BudgetResult{judge("constants-cache-flood", "every request answered while the hot-entry layer evicts (target)",
				o.Load != nil && failedRequests(o.Load) == 0,
				fmt.Sprintf("%d of %d failed; %s hits, %s misses", failedRequests(o.Load), offered(o.Load), fmtCount(hits), fmtCount(misses)))}
		},
	}, res)
	return err
}

// ringLadder runs the ring-hash Upstream ladder: for each rung, a Node on
// a Bundle of that many ring-hash Upstreams of RingEndpoints Endpoints,
// whose balancer structures stay within the 256 MiB budget, every ring
// built when the minimum of 64 virtual nodes per Endpoint fits and some
// falling back with balancer_budget when it does not.
func (e Env) ringLadder(ctx context.Context, s Scale, res *Result) error {
	c := s.Constants
	for _, rings := range c.RingSizes {
		id := "constants-ring-" + strconv.Itoa(rings)
		dir := filepath.Join(e.S.Work, "ring", strconv.Itoa(rings))
		if err := ringBundle(filepath.Join(dir, "bundle"), labOf(e.S.Profile), rings, c.RingEndpoints); err != nil {
			return err
		}
		rev, err := renderBundle(ctx, e.Head.Ruralz, filepath.Join(dir, "bundle"), filepath.Join(dir, "rendered.yaml"))
		if err != nil {
			return err
		}
		sc, err := constantsScenario(id, c.BackgroundRate)
		if err != nil {
			return err
		}
		sc.Bundle = filepath.Join(dir, "bundle")
		if err := e.check(ctx, sc); err != nil {
			return err
		}
		err = e.backgroundOn(ctx, sc, rev, harness.NodeSpec{}, 5*time.Second, func(ctx context.Context, st *harness.Stage) func(*harness.RunOutcome) []benchrecord.BudgetResult {
			rs, err := readRings(ctx, st.Node())
			m, merr := st.Node().Scrape(ctx)
			degraded := merr == nil && m.Sum(metricUpDegraded, promtext.L("reason", "balancer_budget")) > 0
			return fixed(ringCriteria(rings, c.RingEndpoints, rs, degraded, errors.Join(err, merr))...)
		}, res)
		if err != nil {
			return err
		}
	}
	return nil
}

// connMemory runs the per-connection memory flood: FloodConnections
// slow-header connections each holding a header block just under the
// 256 KiB ceiling, opened within 4 s, measured for the memory each costs
// against the 592 KiB hypothesis (reported on RH-1), then closed by the
// Node at its ReadHeaderTimeout, each between 10 s and 12 s.
func (e Env) connMemory(ctx context.Context, s Scale, res *Result) error {
	c := s.Constants
	sc, err := constantsScenario("constants-conn-memory", c.BackgroundRate)
	if err != nil {
		return err
	}
	return e.backgroundRun(ctx, sc, ReadHeaderTimeout+TimeoutSlack+10*time.Second, func(ctx context.Context, st *harness.Stage) func(*harness.RunOutcome) []benchrecord.BudgetResult {
		const heldID, heldValue = "constants-conn-memory", "the slow-header flood held, about 592 KiB per connection (hypothesis)"
		node := st.Node()
		before, err := node.RSS()
		if err != nil {
			return failedAction(heldID, heldValue, err)
		}
		n := c.FloodConnections
		cc := lab.ConnsConfig{
			Kind: lab.ConnsSlowHeader, Target: e.listenerAddr(portHTTP), Count: n, Rate: float64(n) / 4,
			Host: harness.NameGateway, Path: benchPath, HeaderBytes: HeaderCeiling - 4<<10,
		}
		p, err := st.Lab().StartConns(ctx, "conns-flood", cc)
		if err != nil {
			return failedAction(heldID, heldValue, err)
		}
		cs, err := waitConns(ctx, p, connsWait(cc))
		if err != nil {
			return failedAction(heldID, heldValue, err)
		}
		after, err := node.RSS()
		if err != nil {
			return failedAction(heldID, heldValue, err)
		}
		if err := sleepCtx(ctx, ReadHeaderTimeout+TimeoutSlack+time.Second); err != nil {
			return failedAction(heldID, heldValue, err)
		}
		closed, err := connsStats(p)
		if err != nil {
			return failedAction(heldID, heldValue, err)
		}
		per := int64(0)
		if n > 0 {
			per = (after - before) / int64(n)
		}
		heldResult := judge(heldID, heldValue, cs.Opened == int64(n) && !node.Exited(),
			fmt.Sprintf("%d of %d connections set up, %s per connection", cs.Opened, n, kib(per)))
		return fixed(heldResult,
			s.hardware(bytesBound("constants-conn-memory-per-connection", "about 592 KiB per connection before a handler runs (hypothesis)", per, ConnectionMemory)),
			window("constants-read-header-timeout", "each slow header block expires at ReadHeaderTimeout within 10s to 12s (target)", closed.ClosedAfter, closed.ClosedByPeer, ReadHeaderTimeout),
			judge("constants-read-header-closed", "the Node closes every slow-header connection", closed.ClosedByPeer == int64(n),
				fmt.Sprintf("%d of %d closed by the Node", closed.ClosedByPeer, n)),
		)
	}, res)
}
