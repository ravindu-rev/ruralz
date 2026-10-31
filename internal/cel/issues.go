// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"slices"
	"strings"
	"unicode/utf8"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Codes of the findings this package reports
// (docs/architecture/02-configuration-model.md "Limits").
const (
	// codeInvalid is a syntax, type or unavailable-variable error.
	codeInvalid = "RZ-CFG-014"
	// codeCost is a static cost above the ceiling.
	codeCost = "RZ-CFG-015"
)

// issueAt returns an Issue of code at the 1-based line and 0-based
// code-point column cel-go reports; a line below 1 is not positional.
func issueAt(code string, se siteEnv, src string, line, col int, msg, hint string) expr.Issue {
	is := expr.Issue{
		Code:    code,
		Offset:  -1,
		Message: "CEL " + string(se.place.ID) + ": " + msg,
		Hint:    hint,
	}
	if off, ok := byteOffset(src, line, col); ok {
		is.Offset, is.Line, is.Column = off, line, col+1
	}
	return is
}

// wholeIssue returns an Issue about the whole expression, not positional.
func wholeIssue(code string, se siteEnv, msg, hint string) expr.Issue {
	return issueAt(code, se, "", 0, 0, msg, hint)
}

// byteOffset returns the byte offset in src of the 1-based line and the
// 0-based code-point column within it; false when outside src.
func byteOffset(src string, line, col int) (int, bool) {
	if line < 1 || col < 0 {
		return 0, false
	}
	off := 0
	for l := 1; l < line; l++ {
		i := strings.IndexByte(src[off:], '\n')
		if i < 0 {
			return 0, false
		}
		off += i + 1
	}
	for range col {
		if off >= len(src) || src[off] == '\n' {
			return 0, false
		}
		_, size := utf8.DecodeRuneInString(src[off:])
		off += size
	}
	return off, true
}

// celIssues converts cel-go's parse and check errors, in cel-go's order,
// to RZ-CFG-014 Issues. An undeclared reference to a Ruralz variable gets
// a hint naming the variables the site has.
func celIssues(se siteEnv, src string, iss *celgo.Issues) []expr.Issue {
	errs := iss.Errors()
	out := make([]expr.Issue, 0, len(errs))
	for _, e := range errs {
		msg, _, _ := strings.Cut(e.Message, "\n")
		// Ruralz declares no container, so cel-go's container note says
		// nothing.
		msg = strings.TrimSuffix(msg, " (in container '')")
		hint := ""
		if name, ok := undeclaredVar(msg); ok && se.vars&name == 0 {
			hint = string(se.place.ID) + " has " + strings.Join(se.vars.Names(), ", ")
		}
		line, col := -1, -1
		if e.Location != nil {
			line, col = e.Location.Line(), e.Location.Column()
		}
		out = append(out, issueAt(codeInvalid, se, src, line, col, msg, hint))
	}
	return out
}

// undeclaredVar returns the Ruralz variable an "undeclared reference"
// message names.
func undeclaredVar(msg string) (expr.Var, bool) {
	rest, ok := strings.CutPrefix(msg, "undeclared reference to '")
	if !ok {
		return 0, false
	}
	name, _, ok := strings.Cut(rest, "'")
	if !ok {
		return 0, false
	}
	return varBit(name)
}

// findingIssues converts the site findings to RZ-CFG-014 Issues ordered by
// position, then message.
func findingIssues(se siteEnv, src string, a *ast.AST, fs []finding) []expr.Issue {
	out := make([]expr.Issue, 0, len(fs))
	for _, f := range fs {
		loc := a.SourceInfo().GetStartLocation(f.id)
		out = append(out, issueAt(codeInvalid, se, src, loc.Line(), loc.Column(), f.msg, f.hint))
	}
	slices.SortStableFunc(out, func(x, y expr.Issue) int {
		if x.Offset != y.Offset {
			return x.Offset - y.Offset
		}
		return strings.Compare(x.Message, y.Message)
	})
	return out
}
