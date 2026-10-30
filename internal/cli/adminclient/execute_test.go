// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// End-to-end tests through command.Execute of a command that reads
// /config/dump with the admin flags: exit codes, the one-line problem on
// standard error and the document on standard output under --output json
// (docs/reference/01-cli-and-api-surface.md "Admin API conventions" and
// "Exit codes").

// dumpCommand reads /config/dump of the Node --admin names and writes it
// to standard output.
type dumpCommand struct {
	admin  Flags
	output string
	clk    *clocktest.Fake
}

func (d *dumpCommand) Flags(f *command.Flags) {
	d.admin.Bind(f, FlagOptions{URL: true, Timeout: true})
	f.Output(&d.output, "text", "text", "json")
}

func (d *dumpCommand) Run(ctx context.Context, sio command.IO, _ []string) error {
	c, err := New(d.admin.Config(sio), Options{Stderr: sio.Stderr, Clock: d.clk, Version: testVersion})
	if err != nil {
		return err
	}
	defer c.Close()
	doc, err := c.ConfigDump(ctx)
	if err != nil {
		return err
	}
	return command.WriteData(sio, "", doc)
}

// executeDump runs `ruralz node dump args...` on clk with environ.
func executeDump(ctx context.Context, clk *clocktest.Fake, environ []string, args ...string) (code int, stdout, stderr string) {
	spec := command.Spec{
		Path: []string{"node", "dump"}, Summary: "Save /config/dump, secrets omitted",
		New: func() command.Command { return &dumpCommand{clk: clk} },
	}
	var out, errOut bytes.Buffer
	sio := command.NewIO(&out, &errOut, environ, epoch, "linux")
	code = command.Execute(ctx, sio, "ruralz", []command.Spec{spec}, append([]string{"node", "dump"}, args...))
	return code, out.String(), errOut.String()
}

// dumpBody is a /config/dump document; its bytes reach standard output
// unchanged.
const dumpBody = `{"content":{"apiVersion":"ruralz/v1alpha1"},"digest":"sha256:68f7","lastKnownGood":null,"revision":"rev-68f782530463"}`

// TestExecuteDump covers the results of the command and their streams.
func TestExecuteDump(t *testing.T) {
	var rec recorder
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		switch r.URL.Path {
		case "/ok/config/dump":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, dumpBody)
		default:
			writeProblem(w, r, "RZ-RT-024")
		}
	})
	tok := tokenFile(t, testToken)
	short := writeFile(t, t.TempDir(), "short", "short-token")
	problemLine := func() string {
		id, _ := traceIDOf(rec.all()[len(rec.all())-1].header.Get("Traceparent"))
		return "ruralz node dump: 503 RZ-RT-024 No active Revision (requestId " + id + ")\n"
	}
	problemDoc := func() string {
		id, _ := traceIDOf(rec.all()[len(rec.all())-1].header.Get("Traceparent"))
		return string(problem.Append(nil, problem.New("RZ-RT-024", id))) + "\n"
	}

	t.Run("dump", func(t *testing.T) {
		code, out, errOut := executeDump(t.Context(), clocktest.New(epoch()), nil, "--admin", srv.URL+"/ok")
		if code != 0 || out != dumpBody || errOut != "" {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
	t.Run("problem document", func(t *testing.T) {
		code, out, errOut := executeDump(t.Context(), clocktest.New(epoch()), nil, "--admin", srv.URL)
		if code != 2 || out != "" || errOut != problemLine() {
			t.Errorf("exit %d, stdout %q, stderr %q, want %q", code, out, errOut, problemLine())
		}
	})
	t.Run("problem document with --output json", func(t *testing.T) {
		code, out, errOut := executeDump(t.Context(), clocktest.New(epoch()), nil, "--admin", srv.URL, "--output", "json")
		if code != 2 || out != problemDoc() || errOut != problemLine() {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
	t.Run("environment", func(t *testing.T) {
		environ := []string{"RURALZ_ADMIN_URL=" + srv.URL + "/ok", "RURALZ_ADMIN_TOKEN_FILE=" + tok}
		code, out, errOut := executeDump(t.Context(), clocktest.New(epoch()), environ)
		last := rec.all()[len(rec.all())-1]
		if code != 0 || out != dumpBody || errOut != "" || last.header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("exit %d, stdout %q, stderr %q, Authorization %q", code, out, errOut, last.header.Get("Authorization"))
		}
	})
	t.Run("invalid URL", func(t *testing.T) {
		code, out, errOut := executeDump(t.Context(), clocktest.New(epoch()), nil, "--admin", "ftp://node-a")
		want := "ruralz node dump: --admin \"ftp://node-a\": want an http or https URL, such as http://127.0.0.1:9901\n" +
			"Run 'ruralz node dump --help' for usage.\n"
		if code != 2 || out != "" || errOut != want {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
	t.Run("invalid URL from the environment", func(t *testing.T) {
		code, _, errOut := executeDump(t.Context(), clocktest.New(epoch()), []string{"RURALZ_ADMIN_URL=http://node-a/?x=1"})
		if code != 2 || !strings.HasPrefix(errOut, `ruralz node dump: RURALZ_ADMIN_URL "http://node-a/?x=1": an admin URL takes no query`) {
			t.Errorf("exit %d, stderr %q", code, errOut)
		}
	})
	t.Run("invalid token file", func(t *testing.T) {
		n := len(rec.all())
		code, out, errOut := executeDump(t.Context(), clocktest.New(epoch()), nil, "--admin", srv.URL, "--admin-token-file", short)
		want := "ruralz node dump: --admin-token-file " + short + ": adminauth: the admin token is shorter than 22 bytes\n"
		if code != 2 || out != "" || errOut != want || len(rec.all()) != n {
			t.Errorf("exit %d, stdout %q, stderr %q, want %q", code, out, errOut, want)
		}
	})
	t.Run("negative timeout", func(t *testing.T) {
		code, _, errOut := executeDump(t.Context(), clocktest.New(epoch()), nil, "--timeout", "-1s")
		if code != 2 || !strings.HasPrefix(errOut, `ruralz node dump: invalid value "-1s" for --timeout: must be zero or greater (0 means none)`) {
			t.Errorf("exit %d, stderr %q", code, errOut)
		}
	})
}

// TestExecuteDeadline checks that an expired --timeout exits 2 naming it.
func TestExecuteDeadline(t *testing.T) {
	s := newStallingServer(t, partialBody, nil)
	clk := clocktest.New(epoch())
	type outcome struct {
		code        int
		out, errOut string
	}
	ch := make(chan outcome, 1)
	go func() {
		code, out, errOut := executeDump(t.Context(), clk, nil, "--admin", s.url, "--timeout", "90s")
		ch <- outcome{code, out, errOut}
	}()
	within(t, "the partial body", s.entered)
	waitFor(t, "the call deadline alone", func() bool { return clk.Pending() == 1 })
	clk.Advance(90 * time.Second)
	select {
	case o := <-ch:
		want := "ruralz node dump: GET " + s.url + "/config/dump: not complete within 1m30s (--timeout)\n"
		if o.code != 2 || o.out != "" || o.errOut != want {
			t.Errorf("exit %d, stdout %q, stderr %q, want %q", o.code, o.out, o.errOut, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not end")
	}
}

// TestExecuteInterrupt checks that Ctrl-C ends the command with 130 and
// prints nothing.
func TestExecuteInterrupt(t *testing.T) {
	s := newStallingServer(t, nil, nil)
	ctx, interrupt := interruptible(t)
	type outcome struct {
		code        int
		out, errOut string
	}
	ch := make(chan outcome, 1)
	go func() {
		code, out, errOut := executeDump(ctx, clocktest.New(epoch()), nil, "--admin", s.url)
		ch <- outcome{code, out, errOut}
	}()
	within(t, "the request", s.entered)
	interrupt()
	select {
	case o := <-ch:
		if o.code != command.ExitInterrupted || o.out != "" || o.errOut != "" {
			t.Errorf("exit %d, stdout %q, stderr %q", o.code, o.out, o.errOut)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not end")
	}
}

// TestExecuteHelp checks the help of the admin flags.
func TestExecuteHelp(t *testing.T) {
	code, out, errOut := executeDump(t.Context(), clocktest.New(epoch()), nil, "--help")
	want := `Usage: ruralz node dump [FLAGS]

Save /config/dump, secrets omitted.

Flags:
  --admin URL              Admin base URL of the Node (default $RURALZ_ADMIN_URL, else http://127.0.0.1:9901)
  --admin-token-file PATH  Operator admin token file (default $RURALZ_ADMIN_TOKEN_FILE)
  --ca-file PATH           Extra trust anchors (PEM), added to the system roots
  --client-cert PATH       Client certificate (PEM) for admin mTLS over https
  --client-key PATH        Unencrypted PEM key of --client-cert
  --help                   Print this help
  --output text|json       Output format (text or json, default text)
  --timeout DURATION       Deadline of each admin call that does not stream (default 5m; 0 means none)
`
	if code != 0 || out != want || errOut != "" {
		t.Errorf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
}
