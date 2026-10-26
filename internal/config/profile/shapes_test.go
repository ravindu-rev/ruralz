// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// The hostile shapes, which the hostile-input table, the fuzz seeds and
// the parser tests share. Each targets a cost a YAML parser can let grow
// faster than its input: expansion, nesting past the depth bound, copying
// a long key into every node below it, or work repeated for every entry.

// billionLaughs is the alias-expansion shape, sized to size bytes.
func billionLaughs(size int) string {
	var b strings.Builder
	b.WriteString("a: &a [\"lol\",\"lol\",\"lol\",\"lol\",\"lol\",\"lol\",\"lol\",\"lol\",\"lol\"]\n")
	prev := "a"
	for i := 0; b.Len() < size; i++ {
		name := "l" + strconv.Itoa(i)
		b.WriteString(name + ": &" + name + " [*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + "]\n")
		prev = name
	}
	return b.String()
}

// taggedLadder is cols columns in turn of rows lines "kN: !!map", whose
// tag ends the line before a sibling key at the same column. A parser that
// nests each key under the tagged node before it grows the tree one level
// per line; YAML 1.2 makes each such node an empty !!map, which the
// profile refuses at its tag. A last line "end: 1" gives the last tagged
// node a value. Keys, then a last comment, pad the input to size bytes;
// size 0 adds no padding.
func taggedLadder(cols, rows, size int) string {
	gen := func(pad string) string {
		var b strings.Builder
		for c := range cols {
			for r := range rows {
				b.WriteString(strings.Repeat(" ", c) + "k" + pad + itoa(c*rows+r) + ": !!map\n")
			}
		}
		// The last tagged node gets a value too.
		b.WriteString(strings.Repeat(" ", max(cols-1, 0)) + "end: 1\n")
		return b.String()
	}
	src := gen("")
	if n := cols * rows; size > len(src) && n > 0 {
		src = gen(strings.Repeat("x", (size-len(src))/n))
	}
	if size > len(src)+1 {
		src += "#" + strings.Repeat("x", size-len(src)-2) + "\n"
	}
	return src
}

// keyLadder is "a:\n", then reps repetitions of two lines: at column c,
// the given number of "- " indicators and an empty quoted scalar, then ":"
// at column 1. c starts at 3 and grows by 2*dashes each time. A parser
// that reads each quoted scalar and the ':' under it as a multi-line
// implicit key nests one sequence deeper per repetition; YAML 1.2 reads
// each ':' as an entry with an empty key whose value is the sequence
// below it, so the entries are duplicate keys.
func keyLadder(dashes, reps int) string {
	var b strings.Builder
	b.WriteString("a:\n")
	for r := range reps {
		c := 3 + 2*dashes*r
		b.WriteString(strings.Repeat(" ", c-1) + strings.Repeat("- ", dashes) + "\"\"\n:\n")
	}
	return b.String()
}

// explicitKeyLadder is reps repetitions of three lines: `? "a"` at column
// c, then at column c+2 the given number of "- " indicators and `"b"`,
// then ":" at column c. c starts at 1 and grows by 2*dashes+4 each time;
// dashes < 0 gives repetition r 63-r dashes. A parser that groups each
// ':' with the "b" before it nests a multi-line implicit key at its own
// column per repetition; YAML 1.2 refuses the line indented past the
// explicit key.
func explicitKeyLadder(dashes, reps int) string {
	var b strings.Builder
	c := 1
	for r := range reps {
		k := dashes
		if k < 0 {
			k = 63 - r
		}
		b.WriteString(strings.Repeat(" ", c-1) + "? \"a\"\n" + strings.Repeat(" ", c+1) + strings.Repeat("- ", k) + "\"b\"\n" + strings.Repeat(" ", c-1) + ":\n")
		c += 2*k + 4
	}
	return b.String()
}

// longKeyFlow is a key of keyLen bytes over a flow sequence of items+1
// entries: a parser that copies a node's path from its parent's copies the
// key into every entry's path. An implicit key is at most 1,024
// characters, so a longer one is refused at the key.
func longKeyFlow(keyLen, items int) string {
	return strings.Repeat("k", keyLen) + ": [" + strings.Repeat("1,", items) + "1]"
}

// longKeysNested is the same cost in valid YAML 1.2, every implicit key
// under 1,024 bytes: levels nested block mappings, each with a key of
// keyLen bytes, then a flow sequence of items+1 entries.
func longKeysNested(levels, keyLen, items int) string {
	var b strings.Builder
	for d := range levels {
		b.WriteString(strings.Repeat(" ", d) + strings.Repeat("k", keyLen) + ":\n")
	}
	b.WriteString(strings.Repeat(" ", levels) + "v: [" + strings.Repeat("1,", items) + "1]\n")
	return b.String()
}

// longKeysInFlow nests the same shape in flow mappings: levels flow
// mappings with keys of keyLen bytes around a flow sequence of items+1
// entries.
func longKeysInFlow(levels, keyLen, items int) string {
	return "a: " + strings.Repeat("{"+strings.Repeat("k", keyLen)+": ", levels) + "[" + strings.Repeat("1,", items) + "1]" + strings.Repeat("}", levels) + "\n"
}

// keysWithoutValues is a flow mapping of n+1 entries entry, such as "a"
// or "k:", that have no value: a parser that inserts each implicit null
// into a token slice moves every later token, quadratic work.
func keysWithoutValues(entry string, n int) string {
	return "a: {" + strings.Repeat(entry+",", n) + entry + "}"
}

// taggedEmpties is a flow sequence of n tagged empty nodes ("[!!str ,
// ...]"), or with keys a flow mapping of n entries whose values are tagged
// empty nodes: the same insertion cost for each tag's empty value.
func taggedEmpties(n int, keys bool) string {
	if !keys {
		return "a: [" + strings.Repeat("!!str ,", n) + "x]\n"
	}
	var b strings.Builder
	b.WriteString("a: {")
	for i := range n {
		b.WriteString("k" + itoa(i) + ": !!str ,")
	}
	b.WriteString("z: x}\n")
	return b.String()
}

// dedented is body under "a:", indented by two spaces, then " c: 2" at
// column 2, which returns to no enclosing collection: a parser that reads
// the whole value again to place that line does work quadratic in the
// body.
func dedented(body string) string {
	return "a:\n  " + strings.ReplaceAll(strings.TrimSuffix(body, "\n"), "\n", "\n  ") + "\n c: 2\n"
}

// treeDepth returns the nesting depth of a tree: a collection is one more
// than its deepest child, so a document's root mapping is depth 1, as
// Options.MaxDepth counts it.
func treeDepth(n *tree.Node) int {
	if n == nil {
		return 0
	}
	d := 0
	for _, m := range n.Members {
		d = max(d, treeDepth(m.Value))
	}
	for _, it := range n.Items {
		d = max(d, treeDepth(it))
	}
	if n.Kind == tree.KindMap || n.Kind == tree.KindList {
		d++
	}
	return d
}

// flowLadder is "x: ", then levels levels, each "[\n" and rows lines of
// "key:", the line of row r indented by r spaces (the first key is "k",
// the others keyLen bytes), then " " before the next level; then "v" and
// the closing brackets. A parser that reads the lines by columns nests one
// level per line; YAML 1.2 refuses the first line not indented past the
// mapping holding the flow sequence.
func flowLadder(levels, rows, keyLen int) string {
	var b strings.Builder
	b.WriteString("x: ")
	for range levels {
		b.WriteString("[\n")
		for r := range rows {
			key := "k"
			if r > 0 {
				key = strings.Repeat("k", keyLen)
			}
			b.WriteString(strings.Repeat(" ", r) + key + ":")
			if r < rows-1 {
				b.WriteByte('\n')
			}
		}
		b.WriteByte(' ')
	}
	b.WriteString("v" + strings.Repeat("]", levels) + "\n")
	return b.String()
}

// commaLess is "x: [\n", n lines that entry(i) gives, then last and "]\n":
// flow entries without ',' that a parser can read as one block mapping
// inside the sequence, recursing once per entry.
func commaLess(n int, entry func(i int) string, last string) string {
	var b strings.Builder
	b.WriteString("x: [\n")
	for i := range n {
		b.WriteString(entry(i) + "\n")
	}
	b.WriteString(last + "]\n")
	return b.String()
}

// longTaggedScalar is longBlockScalar followed by one more entry.
func longTaggedScalar(prefix, header string, n, m int) string {
	if prefix == "- " {
		return longBlockScalar(prefix, header, n, m, "- z\n")
	}
	return longBlockScalar(prefix, header, n, m, "b: z\n")
}

// longBlockScalar is a block scalar of n bytes of content followed by m
// empty lines, under the given header and line prefix ("a: " or "- "), then
// tail: a parser that clips the empty lines once per line of content does
// O(m·n) work.
func longBlockScalar(prefix, header string, n, m int, tail string) string {
	line := strings.Repeat("x", 63)
	var b strings.Builder
	b.WriteString(prefix + header + "\n")
	for b.Len() < n {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString(strings.Repeat("\n", m))
	b.WriteString(tail)
	return b.String()
}
