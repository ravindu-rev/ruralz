// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"cmp"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// The work of a call is counted while it is done, so Yield runs every 256
// work units inside a resource, spread over the work and not in bursts
// before or after it (01 req 53; review finding: a time-based Yield
// collapses a burst into one Gosched). Every loop of the walk over the
// input counts each element it reads, text is read in steps of at most
// textStep bytes (16 work units), and every library call whose cost
// grows with the input goes through a counted helper of this file, of
// the scanner (scan.go), the detectors (detect.go) or the hint search
// (hint.go) (TestLookupsMetered):
//
//   - tree.Node.Get is a counted scan (get);
//   - schemaidx.Node.Select looks each member an if/then rule tests up by
//     a scan of the instance, once per rule, which no caller can
//     interrupt, so it reads a proxy of the members a rule may test
//     (selectOf), and DispatchValues runs once per counted call
//     (dispatchTested);
//   - schemaidx.ItemElem reads a keyed entry's key by a counted scan and
//     names a set element by its canonical JSON only when it is small
//     (itemElem);
//   - substring searches, copies into a builder, letter tests and case
//     changes read their text in windows (contains, appendText, hasAlnum,
//     lower);
//   - the credential detectors are linear scanners that read their text
//     in windows, in place of the regular expressions of the 01 J row
//     (detect.go);
//   - input text a diagnostic carries is bounded where the walk builds
//     the diagnostic: a message quotes it clipped (clip), a path element
//     clips it (boundElem) and the resource identity shows it clipped and
//     quoted when it is not graphic (shown).
//
// What remains uncounted are single runtime operations on one value at
// memory speed: hashing a name for a map lookup (a variable's Lookup in
// the Source, the hint memo), comparing two strings, allocating one
// string or slice, and the RFC 8785 form of one number literal of a small
// set element. Each reads one value once, which the source limit bounds
// (01 req 6).

// textStep is the most bytes one counted step reads, copies or compares:
// 16 work units of byteUnit bytes.
const textStep = 16 * byteUnit

// maxShown bounds, in bytes, the input text one diagnostic carries in each
// place, as config/canonical bounds it: a name quoted in its message, the
// resource kind and name of its identity, and a member name, a list key, a
// string set element or the canonical JSON of a composite set element in
// its path. The text form of a diagnostic thus stays short however large
// the content, so the diagnostics of a resource print in space linear in
// their count (review finding: up to 10,001 diagnostics each repeated one
// long key in full).
const maxShown = 64

// probe records how one call spreads its work between Yield calls, for
// tests (TestYieldSpread).
type probe struct {
	// since counts the work units charged since the last Yield.
	since int
	// gap is the most work units charged between two Yield calls, or
	// before the first.
	gap int
	// read is the most bytes one counted read reported.
	read int
	// selectWidth is the most members of an object schemaidx Select read.
	selectWidth int
}

// last returns the most work units charged between two Yield calls, the
// work after the last Yield included.
func (p *probe) last() int { return max(p.gap, p.since) }

// tick counts one work unit.
func (w *walker) tick() { w.charge(1) }

// charge counts k work units, at most textStep/byteUnit at a time (read),
// calling Yield each time yieldEvery more have passed.
func (w *walker) charge(k int) {
	if k <= 0 {
		return
	}
	if p := w.probe; p != nil {
		p.since += k
	}
	w.units += k
	// bounded: a charge is at most a few units, so this runs at most once.
	for w.units >= yieldEvery {
		w.units -= yieldEvery
		if p := w.probe; p != nil {
			p.gap = max(p.gap, p.since)
			p.since = 0
		}
		if w.yield != nil {
			w.yield()
		}
	}
}

// read counts n bytes read, written or compared: one work unit per
// byteUnit bytes. Callers read at most textStep bytes at a time.
func (w *walker) read(n int) {
	if n <= 0 {
		return
	}
	if p := w.probe; p != nil {
		p.read = max(p.read, n)
	}
	w.bytes += n
	if w.bytes >= byteUnit {
		w.charge(w.bytes / byteUnit)
		w.bytes %= byteUnit
	}
}

// selected records that schemaidx Select read an object of n members.
func (w *walker) selected(n int) {
	if w.probe != nil {
		w.probe.selectWidth = max(w.probe.selectWidth, n)
	}
}

// get returns the value of the first member key of object n, as
// tree.Node.Get does, counting each member it reads.
func (w *walker) get(n *tree.Node, key string) (*tree.Node, bool) {
	if n == nil || n.Kind != tree.KindMap {
		return nil, false
	}
	for i := range n.Members {
		w.tick()
		if n.Members[i].Key == key {
			return n.Members[i].Value, true
		}
	}
	return nil, false
}

// selectOf returns static.Select(inst), the schema of object inst with
// the if/then dispatch of static applied. schemaidx Select finds each
// member a rule tests by a scan of inst, once per rule and round (cases
// times members), which no caller can count as it runs, so selectOf
// passes it a proxy: inst itself when it has at most linearScan members,
// else the first member of each name that the schema nodes the dispatch
// reaches declare, collected by a counted pass over inst. Select reads
// the proxy as it reads inst, because every member a rule reads is a
// declared property of the object whose rule it is
// (TestDispatchReadsDeclared over the committed schema) and a dispatch
// result declares every property of the nodes it passed through: when
// the result declares a property no node read so far declared, another
// pass collects it and Select runs again, which ends within the rounds
// Select takes. A contract change request asks schemaidx to export the
// members its rules read, which makes the proxy exact for every schema.
func (w *walker) selectOf(static *schemaidx.Node, inst *tree.Node) *schemaidx.Node {
	w.tick()
	if inst == nil || inst.Kind != tree.KindMap || len(inst.Members) <= linearScan {
		if inst != nil {
			w.selected(len(inst.Members))
		}
		return static.Select(inst)
	}
	var buf [linearScan]tree.Member
	proxy := &tree.Node{Kind: tree.KindMap, Members: buf[:0]}
	read := []*schemaidx.Node{static}
	w.collect(proxy, inst, static)
	for pass := 1; ; pass++ {
		sel := static.Select(proxy)
		w.selected(len(proxy.Members))
		if pass == maxSelectPasses || !w.declaresMore(sel, read) {
			return sel
		}
		read = append(read, sel)
		if !w.collect(proxy, inst, sel) {
			return sel
		}
	}
}

// maxSelectPasses bounds the passes of selectOf: one per dispatch round
// schemaidx Select takes (at most 8) and one more.
const maxSelectPasses = 9

// collect appends to proxy the first member of inst of each name that
// node declares and proxy lacks, and reports whether it added one,
// counting each member of inst.
func (w *walker) collect(proxy, inst *tree.Node, node *schemaidx.Node) bool {
	added := false
	for _, m := range inst.Members {
		w.tick()
		if _, ok := node.Property(m.Key); !ok {
			continue
		}
		if _, dup := w.get(proxy, m.Key); !dup {
			proxy.Members = append(proxy.Members, m)
			added = true
		}
	}
	return added
}

// declaresMore reports whether sel declares a property that none of the
// nodes in read declares, counting each property it compares.
func (w *walker) declaresMore(sel *schemaidx.Node, read []*schemaidx.Node) bool {
	// bounded: the nodes of at most maxSelectPasses dispatch results.
	for _, r := range read {
		if r == sel {
			return false
		}
	}
	// bounded: the declared properties of one schema node; each comparison is counted.
	for name := range sel.Properties() {
		known := false
		for _, r := range read {
			w.tick()
			if _, ok := r.Property(name); ok {
				known = true
				break
			}
		}
		if !known {
			return true
		}
	}
	return false
}

// dispatchTested reports whether an if/then rule of static tests member
// name against a string constant (schemaidx DispatchValues), one counted
// call: its cost is bounded by the rules of one schema node.
func (w *walker) dispatchTested(static *schemaidx.Node, name string) bool {
	w.tick()
	return len(static.DispatchValues(name)) > 0
}

// itemElem returns the key-aware path element of element i of a list
// whose schema is list, as schemaidx.ItemElem does (02 req 17), counting
// what it reads: a keyed entry's key member is found by a counted scan,
// a string set element is named by its text, and a composite set element
// by its canonical JSON only when that is at most maxShown bytes, else by
// its index, as config/canonical names it. The key and the string stay
// whole in walker.path, which the walk compares; a diagnostic's copy of
// the path clips them (boundElem). Whether an element is small is
// decided by a counted walk that stops past maxShown bytes of its text
// (keys, strings and number literals as written), so only a small element
// is encoded. The walk passes string, object and list elements only.
func (w *walker) itemElem(list *schemaidx.Node, item *tree.Node, i int) diag.PathElem {
	lt, key := list.List()
	switch {
	case lt.Keyed():
		if k, ok := w.get(item, key); ok && (k.Kind == tree.KindString || k.Kind == tree.KindInt || k.Kind == tree.KindFloat) {
			return diag.Keyed(key, k.Text)
		}
	case lt == schemaidx.ListSet && item != nil:
		if item.Kind == tree.KindString {
			return diag.Item(item.Text)
		}
		if w.textSize(item, maxShown) > maxShown {
			return diag.Index(i)
		}
		e := schemaidx.ItemElem(list, item, i)
		if raw, ok := e.Item.(json.RawMessage); ok && len(raw) > maxShown {
			return diag.Index(i)
		}
		return e
	default:
	}
	return diag.Index(i)
}

// textSize returns the bytes of n's text, each node, key, string and
// number literal counted with its length plus one, stopping once the sum
// passes limit (it then returns a sum past limit), counting each node.
func (w *walker) textSize(n *tree.Node, limit int) int {
	w.tick()
	size := 1 + len(n.Text)
	for _, m := range n.Members {
		if size > limit {
			return size
		}
		size += 1 + len(m.Key) + w.textSize(m.Value, limit-size)
	}
	for _, it := range n.Items {
		if size > limit {
			return size
		}
		size += w.textSize(it, limit-size)
	}
	return size
}

// contains reports whether s holds sub, reading s in windows of textStep
// bytes (indexFrom).
func (w *walker) contains(s, sub string) bool { return indexFrom(s, sub, 0, w.read) >= 0 }

// indexFrom returns the index of the first sub at or after from in s, or
// -1, reading s in windows of textStep bytes, each reported to read (01
// req 53): a window overlaps the next by len(sub)-1 bytes, so no match
// across a window boundary is missed.
func indexFrom(s, sub string, from int, read func(int)) int {
	for q := from; q < len(s); q += textStep {
		end := min(len(s), q+textStep+len(sub)-1)
		if i := strings.Index(s[q:end], sub); i >= 0 {
			read(i + len(sub))
			return q + i
		}
		read(end - q)
	}
	return -1
}

// runeCut returns the end of the window of s that starts at from and
// holds at most textStep bytes, cut before a rune: at the last rune start
// within utf8.UTFMax-1 bytes before the window's end, or at that end, so
// decoding the windows one by one decodes s as a whole does (a valid rune
// is at most utf8.UTFMax bytes long, and an invalid byte decodes alone).
func runeCut(s string, from int) int {
	end := min(len(s), from+textStep)
	if end == len(s) {
		return end
	}
	// bounded: at most utf8.UTFMax-1 steps back to a rune start.
	for q := end; q > end-utf8.UTFMax && q > from; q-- {
		if utf8.RuneStart(s[q]) {
			return q
		}
	}
	return end
}

// appendText writes s to b in windows of textStep bytes, each reported to
// read.
func appendText(b *strings.Builder, s string, read func(int)) {
	for len(s) > 0 {
		k := min(len(s), textStep)
		b.WriteString(s[:k])
		read(k)
		s = s[k:]
	}
}

// hasAlnum reports a letter or digit in s, as
// strings.IndexFunc(s, unicode.IsLetter or unicode.IsDigit) does, reading
// s in windows cut before a rune.
func hasAlnum(s string, read func(int)) bool {
	for from := 0; from < len(s); {
		end := runeCut(s, from)
		read(end - from)
		if strings.IndexFunc(s[from:end], isAlnum) >= 0 {
			return true
		}
		from = end
	}
	return false
}

// lower returns strings.ToLower(s), mapping s in windows cut before a
// rune, each reported to read: ToLower maps rune by rune, and so maps
// the windows of s as it maps s.
func lower(s string, read func(int)) string {
	if len(s) <= textStep {
		read(len(s))
		return strings.ToLower(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for from := 0; from < len(s); {
		end := runeCut(s, from)
		read(end - from)
		b.WriteString(strings.ToLower(s[from:end]))
		from = end
	}
	return b.String()
}

// clip shortens s, input text a diagnostic carries, to its first maxShown
// bytes cut before a rune, marked with "...". The result is always a
// copy, as config/canonical's is: s may be a substring of the parser's
// copy of the source, which a diagnostic holding it would keep alive.
func clip(s string) string {
	if len(s) <= maxShown {
		return strings.Clone(s)
	}
	cut := maxShown
	// bounded: at most utf8.UTFMax-1 steps back to a rune start.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// boundElem returns path element e with the input text it names bounded,
// as config/canonical's boundElem bounds it: a member name, a keyed
// entry's key and a string set element are clipped (diag quotes them as
// JSON). itemElem already names a composite or number set element longer
// than maxShown by its index.
func boundElem(e diag.PathElem) diag.PathElem {
	switch e.Kind {
	case diag.ElemField:
		e.Name = clip(e.Name)
	case diag.ElemKeyed:
		e.Key = clip(e.Key)
	case diag.ElemItem:
		if s, ok := e.Item.(string); ok {
			e.Item = clip(s)
		}
	case diag.ElemIndex:
	}
	return e
}

// shown returns a resource kind or name from the input as a diagnostic's
// identity shows it, as config/canonical shows it: clipped (a copy),
// Go-quoted (strconv.QuoteToGraphic) when it holds a character that is
// not graphic (a newline, a control or format character), a quote, a
// backslash or invalid UTF-8, and `""` when it is empty. diag.AppendText
// prints an identity as it is, so the one-line text form (01 req 49) then
// carries no newline and no terminal control sequence.
func shown(s string) string {
	if s == "" {
		return `""`
	}
	c := clip(s)
	if needsQuote(c) {
		return strconv.QuoteToGraphic(c)
	}
	return c
}

// needsQuote reports whether strconv.QuoteToGraphic would escape a
// character of s: one that is not graphic, a quote, a backslash or a byte
// of invalid UTF-8.
func needsQuote(s string) bool {
	// bounded: a clipped text of at most maxShown+3 bytes.
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c < 0x20 || c == 0x7f || c == '"' || c == '\\' {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || !strconv.IsGraphic(r) {
			return true
		}
		i += size
	}
	return false
}

// resourceID returns the identity a diagnostic carries for the resource
// id (shown).
func resourceID(id tree.ID) *diag.ResourceID {
	return &diag.ResourceID{Kind: shown(string(id.Kind)), Name: shown(id.Name)}
}

// sortStrings sorts s and removes repeated strings, as
// slices.Compact(slices.Sorted(...)) does: a bottom-up merge sort that
// reports a work unit per string it moves and the bytes each comparison
// may read, in steps (compareText), so no step of the sort runs
// uncounted.
func sortStrings(s []string, read func(int)) []string {
	n := len(s)
	if n > 1 {
		src, dst := s, make([]string, n)
		// bounded: log2(n) passes, each counting the n strings it moves.
		for width := 1; width < n; width *= 2 {
			// bounded: the runs of one pass; the loop inside counts each string it moves.
			for lo := 0; lo < n; lo += 2 * width {
				mid, hi := min(lo+width, n), min(lo+2*width, n)
				i, j := lo, mid
				for k := lo; k < hi; k++ {
					read(byteUnit)
					if j >= hi || (i < mid && compareText(src[j], src[i], read) >= 0) {
						dst[k] = src[i]
						i++
					} else {
						dst[k] = src[j]
						j++
					}
				}
			}
			src, dst = dst, src
		}
		s = src
	}
	kept := s[:0]
	for i, v := range s {
		read(byteUnit)
		if i == 0 || compareText(v, kept[len(kept)-1], read) != 0 {
			kept = append(kept, v)
		}
	}
	return kept
}

// compareText returns strings.Compare(a, b), comparing the strings in
// windows of textStep bytes, each reported to read.
func compareText(a, b string, read func(int)) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i += textStep {
		end := min(n, i+textStep)
		read(end - i)
		if c := strings.Compare(a[i:end], b[i:end]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

// trimScheme removes a leading "Bearer " or "Basic " authentication
// scheme, in any case, from s.
func trimScheme(s string) string {
	for _, scheme := range [...]string{"Bearer ", "Basic "} {
		if len(s) >= len(scheme) && strings.EqualFold(s[:len(scheme)], scheme) {
			return s[len(scheme):]
		}
	}
	return s
}

// isAlnum reports a letter or digit.
func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
