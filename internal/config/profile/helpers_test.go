// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml/scanner"
	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// parseYAML parses YAML text with default options.
func parseYAML(t testing.TB, src string) ([]Document, diag.List) {
	t.Helper()
	docs, diags, err := Parse(context.Background(), []byte(src), 0, "f.yaml", FormatYAML, Options{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return docs, diags
}

// parseJSON parses JSON text with default options.
func parseJSON(t testing.TB, src string) ([]Document, diag.List) {
	t.Helper()
	docs, diags, err := Parse(context.Background(), []byte(src), 0, "f.json", FormatJSON, Options{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return docs, diags
}

// show renders a tree compactly: {k:v,...}, [a,...], n, b:true, i:7,
// f:0.5, s:"x". With pos, every key and value carries @line:column.
func show(n *tree.Node, pos bool) string {
	var b strings.Builder
	shower{pos: pos}.write(&b, n)
	return b.String()
}

// showShifted renders a tree like show with positions, its lines moved
// down by shift.
func showShifted(n *tree.Node, shift int) string {
	var b strings.Builder
	shower{pos: true, shift: shift}.write(&b, n)
	return b.String()
}

// showValues renders a tree like show without positions, writing every
// number as its exact value (num:1/2), so 1, 1.0 and 1e0 render alike.
func showValues(n *tree.Node) string {
	var b strings.Builder
	shower{values: true}.write(&b, n)
	return b.String()
}

// shower renders trees for show, showShifted and showValues.
type shower struct {
	pos, values bool
	shift       int
}

func (sh shower) write(b *strings.Builder, n *tree.Node) {
	if n == nil {
		b.WriteString("<nil>")
		return
	}
	at := func(p tree.Pos) {
		if sh.pos {
			fmt.Fprintf(b, "@%d:%d", int(p.Line)+sh.shift, p.Column)
		}
	}
	switch n.Kind {
	case tree.KindMap:
		b.WriteByte('{')
		for i, m := range n.Members {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(m.Key)
			at(m.KeyPos)
			b.WriteByte(':')
			sh.write(b, m.Value)
		}
		b.WriteByte('}')
	case tree.KindList:
		b.WriteByte('[')
		for i, it := range n.Items {
			if i > 0 {
				b.WriteByte(',')
			}
			sh.write(b, it)
		}
		b.WriteByte(']')
	case tree.KindNull:
		b.WriteString("n")
	case tree.KindBool:
		b.WriteString("b:" + strconv.FormatBool(n.Bool))
	case tree.KindInt, tree.KindFloat:
		if r, ok := new(big.Rat).SetString(n.Text); sh.values && ok {
			b.WriteString("num:" + r.RatString())
		} else if n.Kind == tree.KindInt {
			b.WriteString("i:" + n.Text)
		} else {
			b.WriteString("f:" + n.Text)
		}
	case tree.KindString:
		b.WriteString("s:" + strconv.Quote(n.Text))
	}
	if n.Kind != tree.KindMap && n.Kind != tree.KindList {
		at(n.Pos)
	}
}

// codes lists the diagnostics as "code@line:column".
func codes(l diag.List) string {
	var parts []string
	for _, d := range l {
		parts = append(parts, fmt.Sprintf("%s@%d:%d", d.Code, d.Line, d.Column))
	}
	return strings.Join(parts, " ")
}

// wantOne asserts exactly one diagnostic with code at line:column and
// returns it.
func wantOne(t *testing.T, l diag.List, code string, line, col int) diag.Diagnostic {
	t.Helper()
	if len(l) != 1 || l[0].Code != code || l[0].Line != line || l[0].Column != col {
		t.Fatalf("diagnostics = %q, want one %s@%d:%d", codes(l), code, line, col)
	}
	if l[0].Severity != diag.SeverityError || l[0].File == "" {
		t.Fatalf("diagnostic %+v: want an error with a file", l[0])
	}
	return l[0]
}

// suiteDataDir holds the vendored YAML Test Suite cases.
const suiteDataDir = "../../../test/fixtures/yaml-test-suite/data"

// suiteInputs returns the in.yaml text of every YAML Test Suite case.
func suiteInputs(t testing.TB) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(suiteDataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "in.yaml" {
			data, err := os.ReadFile(path) //nolint:gosec // G304: a file under the vendored suite directory
			if err != nil {
				return err
			}
			out = append(out, string(data))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the YAML Test Suite: %v", err)
	}
	return out
}

// allocBytes returns the bytes allocated by the process so far.
func allocBytes() uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

func itoa(i int) string { return strconv.Itoa(i) }

// peakHeap runs f and returns the largest growth of live heap objects and
// goroutine stacks over their size before f, sampling
// /memory/classes/heap/objects:bytes (the measure of 11 req 17) and
// /memory/classes/heap/stacks:bytes every millisecond: a parser that
// recurses once per token spends its memory on the stack, not in heap
// objects.
func peakHeap(f func()) uint64 {
	read := func() uint64 {
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/heap/stacks:bytes"}}
		metrics.Read(s)
		return s[0].Value.Uint64() + s[1].Value.Uint64()
	}
	runtime.GC()
	base := read()
	peak := base
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				peak = max(peak, read())
			}
		}
	}()
	f()
	close(stop)
	<-done
	peak = max(peak, read())
	return peak - base
}

func itoa64(i int64) string { return strconv.FormatInt(i, 10) }

// scanString returns goccy's scanner tokens of src, up to its first
// error.
func scanString(src string) (token.Tokens, error) {
	var s scanner.Scanner
	s.Init(src)
	var tks token.Tokens
	for {
		sub, err := s.Scan()
		tks.Add(sub...)
		switch {
		case errors.Is(err, io.EOF), err == nil && len(sub) == 0:
			return tks, nil
		case err != nil:
			return tks, err
		}
	}
}
