// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"crypto/sha1" //nolint:gosec // G505: Redis names scripts by SHA-1
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// update rewrites the golden files: go test ./internal/statestore/redis -update.
var update = flag.Bool("update", false, "rewrite testdata golden files")

// TestScriptLayoutGolden pins the library by its SHA-1. The script is State
// Store layout version 1, and Nodes of two releases run side by side
// (docs/engineering/04-release-versioning-and-compatibility.md "State
// Store layout changes"), so changing it is a reviewed layout change: a
// new golden, made with -update.
func TestScriptLayoutGolden(t *testing.T) {
	const path = "testdata/ruralz_v1.sha1"
	got := scriptSHA() + "\n"
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Fatalf("ruralz_v1.lua changed: SHA-1 %s, golden %s; a layout change must keep N-1 Nodes working", strings.TrimSpace(got), strings.TrimSpace(string(want)))
	}
}

// TestScriptSHA checks that EVALSHA names the embedded library by the
// SHA-1 Redis computes for SCRIPT LOAD.
func TestScriptSHA(t *testing.T) {
	sum := sha1.Sum([]byte(script)) //nolint:gosec // G401: the script's name
	if got, want := scriptSHA(), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("scriptSHA() = %s, want %s", got, want)
	}
	if !strings.HasPrefix(script, "-- Copyright") {
		t.Fatal("the embedded library is not ruralz_v1.lua")
	}
}

// TestScriptLoadedOnEveryConnection checks that the dialer runs SCRIPT
// LOAD on every new connection before the client uses it: connect.
func TestScriptLoadedOnEveryConnection(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.consume(c)
	if c.Err != nil || !c.Done {
		t.Fatalf("first call: Done %v Err %v", c.Done, c.Err)
	}
	// A connection the maintenance goroutine is opening may be accepted
	// before its SCRIPT LOAD arrives.
	waitFor(t, "one SCRIPT LOAD per connection", func() bool {
		loads, conns := f.srv.count("SCRIPT LOAD"), f.srv.connections()
		return conns > 0 && loads >= conns
	})
	if f.srv.count("EVAL") != 0 {
		t.Fatal("the driver sent EVAL")
	}
}

// warmConns runs calls until both pipelined connections are open, so a
// test's next call meets a connection that already loaded the library.
func (f *fixture) warmConns() {
	f.t.Helper()
	for i := 0; f.d.conns.len() < 2; i++ {
		if i == 200 {
			f.t.Fatal("the second pipelined connection never opened")
		}
		f.consume(gcraCall(statestore.DigestOf("warm"+fmt.Sprint(i)), oneLimit()))
	}
}

// TestScriptReloadedOnReconnect checks failover: the server loses its
// scripts and every connection, and the reconnect's SCRIPT LOAD lets the
// first call after it succeed without NOSCRIPT.
func TestScriptReloadedOnReconnect(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	f.consume(gcraCall(statestore.DigestOf("k"), oneLimit()))
	before := f.srv.count("SCRIPT LOAD")
	f.srv.flushScripts()
	f.srv.dropConns()
	waitFor(t, "the dropped connections to close", func() bool { return f.d.conns.len() == 0 })
	c := gcraCall(statestore.DigestOf("k2"), oneLimit())
	f.consume(c)
	if c.Err != nil || !c.Done {
		t.Fatalf("call after the reconnect: Done %v Err %v", c.Done, c.Err)
	}
	if f.srv.count("SCRIPT LOAD") <= before {
		t.Fatal("the reconnect loaded no script")
	}
}

// TestNoScriptFailsOnceAndReloads checks the NOSCRIPT path of pack 8.7
// rule 1: the call fails with RZ-STS-002 after one EVALSHA, never retried
// with EVAL or a second EVALSHA, and a reload off the request path makes
// the next call succeed.
func TestNoScriptFailsOnceAndReloads(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	f.warmConns()
	evals, loads := f.srv.count("EVALSHA"), f.srv.count("SCRIPT LOAD")
	f.srv.flushScripts()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.consume(c)
	if c.Err != statestore.ErrFailed || c.Done {
		t.Fatalf("call meeting NOSCRIPT: Done %v Err %v, want RZ-STS-002", c.Done, c.Err)
	}
	if got := f.srv.count("EVALSHA") - evals; got != 1 {
		t.Fatalf("%d EVALSHA for one call, want 1", got)
	}
	if f.srv.count("EVAL") != 0 {
		t.Fatal("NOSCRIPT was retried with EVAL")
	}
	waitFor(t, "the reload", func() bool { return f.srv.count("SCRIPT LOAD") > loads && !f.d.single.loading.Load() })
	c = gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil || !c.Done {
		t.Fatalf("call after the reload: Done %v Err %v", c.Done, c.Err)
	}
}

// TestScriptsRefusedAreUnsupported checks RZ-STS-005: a server answering
// SCRIPT as an unknown command lacks the script command set, so script
// calls and writes are skipped without a call while reads and lease takes
// still run, until a later load succeeds.
func TestScriptsRefusedAreUnsupported(t *testing.T) {
	for _, refusal := range []string{
		"ERR unknown command 'SCRIPT', with args beginning with: 'LOAD'",
		"NOPERM User ruralz has no permissions to run the 'script|load' command",
	} {
		t.Run(strings.Fields(refusal)[0], func(t *testing.T) {
			f := newFixture(t, fixtureOptions{fake: fakeOptions{scriptsOff: refusal}})
			f.ready()
			waitFor(t, "the refusal", func() bool { return f.d.single.noScripts.Load() })
			if f.d.Supports(statestore.CapScripts) {
				t.Fatal("Supports(CapScripts) with SCRIPT refused")
			}
			evals := f.srv.count("EVALSHA")
			c := gcraCall(statestore.DigestOf("k"), oneLimit())
			f.consume(c)
			if c.Err != statestore.ErrUnsupported {
				t.Fatalf("GCRA call: Err %v, want RZ-STS-005", c.Err)
			}
			if f.m.call(emit.StateOpGCRA, statestore.ResultSkipped) != 1 {
				t.Fatal("the unsupported call is not counted skipped")
			}
			w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTO, Invalidate: statestore.DigestOf("u")}
			take := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Lease: statestore.CacheLease{
				Key: statestore.CacheKey{URI: statestore.DigestOf("u")}, Mode: statestore.LeaseTake, Token: 1,
			}}
			f.write(w, take)
			if w.Err != statestore.ErrUnsupported || take.Err != nil || !take.Applied {
				t.Fatalf("invalidation Err %v; lease take Err %v Applied %v", w.Err, take.Err, take.Applied)
			}
			if f.srv.count("EVALSHA") != evals {
				t.Fatal("a script call reached the server")
			}
			l := lookupCall(statestore.CacheKey{URI: statestore.DigestOf("u")}, statestore.DigestOf("v"))
			if f.read(l); l.Err != nil || !l.Done {
				t.Fatalf("lookup: Done %v Err %v", l.Done, l.Err)
			}
			if got := f.logs.count("state store refuses scripts; script calls fail with RZ-STS-005"); got != 1 {
				t.Fatalf("%d refusal logs, want 1", got)
			}
			f.srv.refuseScripts("")
			f.advanceWhile("the retried load", func() bool { return f.d.Supports(statestore.CapScripts) }, time.Second)
			c = gcraCall(statestore.DigestOf("k"), oneLimit())
			if f.consume(c); c.Err != nil || !c.Done {
				t.Fatalf("call after the load: Done %v Err %v", c.Done, c.Err)
			}
		})
	}
}

// TestNeverEval checks that no operation sends EVAL, whose NOSCRIPT retry
// would be a second round trip (ADR-0008).
func TestNeverEval(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	d := statestore.DigestOf("k")
	f.consume(gcraCall(d, oneLimit()), quotaCall(time.Hour, 5, d))
	k := statestore.CacheKey{URI: d, Partition: d}
	f.read(lookupCall(k, d))
	f.write(&statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Cache: statestore.CacheStore{
		Key: k, Names: "n", Variant: d, TTL: time.Hour, Entry: []byte("e"), Token: 1,
	}}, &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTO, Invalidate: d})
	if f.srv.count("EVAL") != 0 {
		t.Fatal("the driver sent EVAL")
	}
}

// luaToken is one token of the library's source.
type luaToken struct {
	kind byte // n name, s string, d number, o operator
	text string
	line int
}

// luaLex tokenizes the Lua subset the library uses.
func luaLex(src string) ([]luaToken, error) {
	var out []luaToken
	line := 1
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case strings.HasPrefix(src[i:], "--[["):
			end := strings.Index(src[i:], "]]")
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated comment", line)
			}
			line += strings.Count(src[i:i+end], "\n")
			i += end + 2
		case strings.HasPrefix(src[i:], "--"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				if j < len(src) && src[j] == '\n' {
					return nil, fmt.Errorf("line %d: newline in string", line)
				}
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("line %d: unterminated string", line)
			}
			out = append(out, luaToken{'s', src[i+1 : j], line})
			i = j + 1
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E') {
				j++
			}
			out = append(out, luaToken{'d', src[i:j], line})
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			out = append(out, luaToken{'n', src[i:j], line})
			i = j
		default:
			op := string(c)
			for _, two := range []string{"...", "==", "~=", "<=", ">=", ".."} {
				if strings.HasPrefix(src[i:], two) {
					op = two
					break
				}
			}
			if !strings.ContainsAny(op[:1], "+-*/%^#<>=(){}[];:,.~") || op == "~" {
				return nil, fmt.Errorf("line %d: unexpected %q", line, op)
			}
			out = append(out, luaToken{'o', op, line})
			i += len(op)
		}
	}
	return out, nil
}

// luaKeywords are Lua 5.1's reserved words.
func luaKeywords() []string {
	return []string{
		"and", "break", "do", "else", "elseif", "end", "false", "for", "function", "if", "in",
		"local", "nil", "not", "or", "repeat", "return", "then", "true", "until", "while",
	}
}

// luaGlobals are the globals a Redis script may read.
func luaGlobals() []string {
	return []string{"redis", "KEYS", "ARGV", "tonumber", "tostring", "string", "math", "table", "type"}
}

// luaCommands are the commands the library may call.
func luaCommands() []string {
	return []string{
		"TIME", "GET", "SET", "INCRBY", "DECRBY", "PEXPIREAT", "PEXPIRE", "PTTL", "EXISTS", "DEL",
		"HGET", "HSET", "HDEL", "HEXISTS",
	}
}

// lintLua checks the library without running it: balanced blocks and
// brackets, every name a declared local or a script global (Redis refuses
// global writes, and an undeclared read is nil), every redis.call a known
// command, and every helper called with a known function.
func lintLua(src string) []string {
	toks, err := luaLex(src)
	if err != nil {
		return []string{err.Error()}
	}
	var errs []string
	type block struct {
		kind  string
		names []string
	}
	blocks := []block{{kind: "chunk"}}
	declare := func(n string) { blocks[len(blocks)-1].names = append(blocks[len(blocks)-1].names, n) }
	declared := func(n string) bool {
		for i := len(blocks) - 1; i >= 0; i-- {
			if slices.Contains(blocks[i].names, n) {
				return true
			}
		}
		return slices.Contains(luaGlobals(), n)
	}
	var brackets []string
	pendingDo := 0
	for i := 0; i < len(toks); i++ {
		tk := toks[i]
		prev := luaToken{}
		if i > 0 {
			prev = toks[i-1]
		}
		next := luaToken{}
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		switch {
		case tk.kind == 'o' && len(tk.text) == 1 && strings.ContainsAny(tk.text, "({["):
			brackets = append(brackets, tk.text)
		case tk.kind == 'o' && len(tk.text) == 1 && strings.ContainsAny(tk.text, ")}]"):
			want := map[string]string{")": "(", "}": "{", "]": "["}[tk.text]
			if len(brackets) == 0 || brackets[len(brackets)-1] != want {
				errs = append(errs, fmt.Sprintf("line %d: unbalanced %q", tk.line, tk.text))
				continue
			}
			brackets = brackets[:len(brackets)-1]
		case tk.kind != 'n':
		case tk.text == "function":
			blocks = append(blocks, block{kind: "function"})
			// The parameters are locals of the function.
			j := i + 1
			for j < len(toks) && toks[j].text != "(" {
				j++
			}
			for j++; j < len(toks) && toks[j].text != ")"; j++ {
				if toks[j].kind == 'n' {
					declare(toks[j].text)
				}
			}
			i = j
		case tk.text == "if":
			blocks = append(blocks, block{kind: "if"})
		case tk.text == "elseif" || tk.text == "else":
			if blocks[len(blocks)-1].kind != "if" {
				errs = append(errs, fmt.Sprintf("line %d: %s outside if", tk.line, tk.text))
				continue
			}
			blocks[len(blocks)-1].names = nil
		case tk.text == "for" || tk.text == "while":
			blocks = append(blocks, block{kind: "loop"})
			pendingDo++
			if tk.text == "for" {
				for j := i + 1; j < len(toks) && toks[j].text != "=" && toks[j].text != "in"; j++ {
					if toks[j].kind == 'n' {
						declare(toks[j].text)
					}
				}
			}
		case tk.text == "do":
			if pendingDo > 0 {
				pendingDo--
			} else {
				blocks = append(blocks, block{kind: "do"})
			}
		case tk.text == "repeat":
			blocks = append(blocks, block{kind: "repeat"})
		case tk.text == "end" || tk.text == "until":
			want := tk.text == "until"
			if len(blocks) == 1 || (blocks[len(blocks)-1].kind == "repeat") != want {
				errs = append(errs, fmt.Sprintf("line %d: unbalanced %s", tk.line, tk.text))
				continue
			}
			blocks = blocks[:len(blocks)-1]
		case tk.text == "local":
			j := i + 1
			if j < len(toks) && toks[j].text == "function" {
				declare(toks[j+1].text)
				continue
			}
			for ; j < len(toks) && toks[j].kind == 'n'; j += 2 {
				declare(toks[j].text)
				if j+1 >= len(toks) || toks[j+1].text != "," {
					break
				}
			}
			i = j
		case slices.Contains(luaKeywords(), tk.text):
		case prev.text == "." || prev.text == ":":
			// A field.
		case len(brackets) > 0 && brackets[len(brackets)-1] == "{" && next.text == "=":
			// A table constructor's key.
		case !declared(tk.text):
			errs = append(errs, fmt.Sprintf("line %d: %s is not a declared local", tk.line, tk.text))
		}
		if tk.text == "call" && prev.text == "." && i >= 2 && toks[i-2].text == "redis" {
			if i+2 >= len(toks) || toks[i+2].kind != 's' || !slices.Contains(luaCommands(), toks[i+2].text) {
				errs = append(errs, fmt.Sprintf("line %d: redis.call of an unknown command", tk.line))
			}
		}
	}
	if len(blocks) != 1 {
		errs = append(errs, fmt.Sprintf("%d blocks left open", len(blocks)-1))
	}
	if len(brackets) != 0 {
		errs = append(errs, fmt.Sprintf("%d brackets left open", len(brackets)))
	}
	if pendingDo != 0 {
		errs = append(errs, "a loop without do")
	}
	return errs
}

// TestLuaLibraryLint checks ruralz_v1.lua statically: the tests have no
// Lua runtime (ADR-0019), and the fake server mirrors the library in Go,
// so this lint and the integration suite against real servers guard the
// script itself.
func TestLuaLibraryLint(t *testing.T) {
	if errs := lintLua(script); len(errs) != 0 {
		t.Fatalf("ruralz_v1.lua:\n%s", strings.Join(errs, "\n"))
	}
}

// TestLuaLintCatchesMistakes checks that the lint finds the mistakes it
// guards against.
func TestLuaLintCatchesMistakes(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"missing end", "if x then\nlocal y = 1\n"},
		{"undeclared name", "local a = 1\nreturn b"},
		{"global write", "count = 1"},
		{"unknown command", "return redis.call('HSETT', KEYS[1])"},
		{"unbalanced paren", "local a = (1"},
		{"stray end", "local a = 1\nend"},
	} {
		src := tc.src
		if tc.name == "missing end" {
			src = "local x = true\n" + src
		}
		if len(lintLua(src)) == 0 {
			t.Errorf("lintLua(%s) found nothing", tc.name)
		}
	}
	if errs := lintLua("local function f(a, b)\nreturn a .. b\nend\nlocal t = {1, f('x', 'y')}\nfor i = 1, #t do\nt[i] = tonumber(t[i])\nend\nreturn t"); len(errs) != 0 {
		t.Fatalf("lintLua flags valid code: %v", errs)
	}
}

// TestScriptReloadIsSingleFlight checks that reload requests arriving
// while a load runs make it run once more, never in parallel.
func TestScriptReloadIsSingleFlight(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	hold := f.srv.inject(&fault{cmd: "SCRIPT LOAD", hold: make(chan struct{})})
	loads := f.srv.count("SCRIPT LOAD")
	f.d.requestScripts(context.Background(), f.d.single)
	waitFor(t, "the held load", func() bool { return f.srv.count("SCRIPT LOAD") == loads+1 })
	for range 4 {
		f.d.requestScripts(context.Background(), f.d.single)
	}
	if !f.d.single.loading.Load() || !f.d.single.again.Load() {
		t.Fatal("requests during a load did not wait for it")
	}
	close(hold.hold)
	f.srv.clearFaults()
	waitFor(t, "the loads to finish", func() bool { return !f.d.single.loading.Load() })
	if got := f.srv.count("SCRIPT LOAD") - loads; got != 2 {
		t.Fatalf("%d loads for 5 requests during one, want 2", got)
	}
	if f.d.single.pending.Load() {
		t.Fatal("a load is still pending after success")
	}
}
