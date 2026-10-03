// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bufio"
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Tests for spec 08 section 2.6 (reqs 47 to 50) and the key rows of the
// 08 section 6.1 unit table: prefixes, the golden key layout, escaping,
// canonical durations and allocation-free builders.

// update rewrites the golden files: go test ./internal/statestore/keys -update.
var update = flag.Bool("update", false, "rewrite testdata golden files")

// fixedStart is a 720h window start: 30-day windows align to the Unix epoch.
func fixedStart() time.Time {
	w := int64(720 * time.Hour / time.Millisecond)
	ms := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC).UnixMilli()
	return time.UnixMilli(ms - ms%w).UTC()
}

// goldenKeys are the layout cases of 08 section 6.1 ("Keys").
func goldenKeys() [][2]string {
	user := statestore.DigestOf("user-42")
	u := statestore.DigestOf("https\x00example.com\x00/v1/catalog\x00")
	p := statestore.DigestOf("gold")
	v := statestore.DigestOf("accept-encoding=gzip")
	ck := statestore.CacheKey{URI: u, Partition: p}
	day := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	s := func(b []byte) string { return string(b) }
	return [][2]string{
		{"ratelimit-1s", s(AppendRateLimit(nil, "ratelimit-gold", statestore.GCRALimit{Requests: 100, Window: time.Second, Burst: 100}, user))},
		{"ratelimit-1m", s(AppendRateLimit(nil, "ratelimit-gold", statestore.GCRALimit{Requests: 5000, Window: time.Minute}, user))},
		{"ratelimit-24h", s(AppendRateLimit(nil, "ratelimit-daily", statestore.GCRALimit{Requests: 1, Window: 24 * time.Hour}, user))},
		{"ratelimit-1500ms", s(AppendRateLimit(nil, "ratelimit-edge", statestore.GCRALimit{Requests: 3, Window: 1500 * time.Millisecond}, user))},
		{"ratelimit-empty-digest", s(AppendRateLimit(nil, "ratelimit-gold", statestore.GCRALimit{Requests: 10, Window: time.Second}, statestore.DigestOf("")))},
		{"quota-720h", s(AppendQuota(nil, "monthly-requests", 720*time.Hour, fixedStart(), user))},
		{"quota-24h", s(AppendQuota(nil, "daily-requests", 24*time.Hour, day, user))},
		{"quota-escape-braces", s(AppendQuota(nil, "a{b}", time.Hour, day, user))},
		{"quota-escape-colon", s(AppendQuota(nil, "a:b", time.Hour, day, user))},
		{"quota-escape-percent", s(AppendQuota(nil, "%", time.Hour, day, user))},
		{"quota-escape-utf8", s(AppendQuota(nil, "é", time.Hour, day, user))},
		{"cache-generation", s(AppendCacheGeneration(nil, u))},
		{"cache-partition", s(AppendCachePartition(nil, ck))},
		{"cache-lease", s(AppendCacheLease(nil, ck))},
		{"field-names", FieldNames},
		{"field-order", FieldOrder},
		{"field-generation", s(AppendField(nil, FieldGeneration, v))},
		{"field-time", s(AppendField(nil, FieldTime, v))},
		{"field-entry", s(AppendField(nil, FieldEntry, v))},
		{"digest-empty", s(AppendHex(nil, statestore.DigestOf("")))},
	}
}

func TestKeyLayoutGolden(t *testing.T) {
	// 08 reqs 47 and 48: exact formats are a compatibility surface.
	const path = "testdata/keys.golden"
	var got bytes.Buffer
	for _, kv := range goldenKeys() {
		got.WriteString(kv[0] + "\t" + kv[1] + "\n")
	}
	if *update {
		if err := os.WriteFile(path, got.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("key layout changed:\ngot:\n%s\nwant:\n%s", got.Bytes(), want)
	}
	// Every golden line names a case exactly once.
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(want))
	for sc.Scan() {
		name, _, _ := strings.Cut(sc.Text(), "\t")
		if seen[name] {
			t.Fatalf("golden case %q repeated", name)
		}
		seen[name] = true
	}
}

func TestKeyLayoutShapes(t *testing.T) {
	// 08 req 48: explicit expectations independent of the golden file.
	user := statestore.DigestOf("user-42")
	hexUser := string(AppendHex(nil, user))
	cases := []struct {
		name, got, want string
	}{
		{"gcra 1s", string(AppendRateLimit(nil, "ratelimit-gold", statestore.GCRALimit{Requests: 100, Window: time.Second}, user)), "rz:rl:ratelimit-gold:100/1s:{" + hexUser + "}"},
		{"gcra 1m0s", string(AppendRateLimit(nil, "ratelimit-gold", statestore.GCRALimit{Requests: 5000, Window: time.Minute}, user)), "rz:rl:ratelimit-gold:5000/1m0s:{" + hexUser + "}"},
		{"quota 720h0m0s", string(AppendQuota(nil, "monthly-requests", 720*time.Hour, time.UnixMilli(1757376000000), user)), "rz:qt:monthly-requests:720h0m0s:1757376000000:{" + hexUser + "}"},
		{"quota escaped", string(AppendQuota(nil, "a{b}:c%é", time.Hour, time.UnixMilli(0), user)), "rz:qt:a%7Bb%7D%3Ac%25%C3%A9:1h0m0s:0:{" + hexUser + "}"},
		{"empty digest", string(AppendHex(nil, statestore.DigestOf(""))), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
	ck := statestore.CacheKey{URI: statestore.DigestOf("u"), Partition: statestore.DigestOf("p")}
	hu, hp := string(AppendHex(nil, ck.URI)), string(AppendHex(nil, ck.Partition))
	if got, want := string(AppendCacheGeneration(nil, ck.URI)), "rz:rc:{"+hu+"}:gen"; got != want {
		t.Errorf("generation = %q, want %q", got, want)
	}
	if got, want := string(AppendCachePartition(nil, ck)), "rz:rc:{"+hu+":"+hp+"}"; got != want {
		t.Errorf("partition = %q, want %q", got, want)
	}
	if got, want := string(AppendCacheLease(nil, ck)), "rz:rc:{"+hu+":"+hp+"}:lease"; got != want {
		t.Errorf("lease = %q, want %q", got, want)
	}
	if n := len(AppendCacheGeneration(nil, ck.URI)); n != CacheGenerationLen {
		t.Errorf("generation length %d, want CacheGenerationLen %d", n, CacheGenerationLen)
	}
	if n := len(AppendCachePartition(nil, ck)); n != CachePartitionLen {
		t.Errorf("partition length %d, want CachePartitionLen %d", n, CachePartitionLen)
	}
	if n := len(AppendCacheLease(nil, ck)); n != CacheLeaseLen {
		t.Errorf("lease length %d, want CacheLeaseLen %d", n, CacheLeaseLen)
	}
	if n := len(AppendField(nil, FieldEntry, ck.URI)); n != FieldLen {
		t.Errorf("field length %d, want FieldLen %d", n, FieldLen)
	}
}

func TestPrefixes(t *testing.T) {
	// 08 req 47: three M1 prefixes plus rzplg: reserved (OQ-scalability-
	// and-distributed-state-11 (a)).
	want := []string{"rz:rl:", "rz:qt:", "rz:rc:", "rzplg:"}
	got := Prefixes()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Prefixes() = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if Prefixes()[0] != "rz:rl:" {
		t.Fatal("Prefixes returns shared state")
	}
	for _, c := range []struct {
		key  string
		want bool
	}{
		{"rzplg:prod:7:policy:x", true},
		{"rz:rl:p:1/1s:{x}", false},
		{"rz:qt:q:1h0m0s:0:{x}", false},
		{"rz:rc:{x}:gen", false},
		{"rzplg", false},
	} {
		if got := Reserved(c.key); got != c.want {
			t.Errorf("Reserved(%q) = %v, want %v", c.key, got, c.want)
		}
	}
	// M1 builders never produce a reserved key.
	for _, kv := range goldenKeys() {
		if Reserved(kv[1]) {
			t.Errorf("%s: builder produced reserved key %q", kv[0], kv[1])
		}
	}
}

func TestEscape(t *testing.T) {
	// 08 req 48: every byte outside [A-Za-z0-9._-] becomes %XX.
	cases := []struct{ in, want string }{
		{"", ""},
		{"monthly-requests", "monthly-requests"},
		{"A.Z_09-", "A.Z_09-"},
		{"a{b}", "a%7Bb%7D"},
		{"a:b", "a%3Ab"},
		{"%", "%25"},
		{"é", "%C3%A9"},
		{" /\x00\xff", "%20%2F%00%FF"},
	}
	for _, c := range cases {
		got := string(AppendEscaped(nil, c.in))
		if got != c.want {
			t.Errorf("AppendEscaped(%q) = %q, want %q", c.in, got, c.want)
		}
		back, ok := Unescape(got)
		if !ok || back != c.in {
			t.Errorf("Unescape(%q) = %q, %v; want %q", got, back, ok, c.in)
		}
	}
	for _, bad := range []string{"{", "a:b", "%", "%2", "%2g", "%7b", "%41", "%zz", "é"} {
		if _, ok := Unescape(bad); ok {
			t.Errorf("Unescape(%q) accepted text AppendEscaped never produces", bad)
		}
	}
}

func TestAppendDuration(t *testing.T) {
	// 08 req 48: <window> is the canonical Go duration string.
	for _, d := range []time.Duration{
		0, 1, 999, time.Microsecond, 1500, time.Millisecond, 1500 * time.Microsecond,
		100 * time.Millisecond, time.Second, 1500 * time.Millisecond, time.Minute,
		90 * time.Second, time.Hour, 24 * time.Hour, 720 * time.Hour, -time.Second,
		-1, 1<<63 - 1, -1 << 63, 3*time.Hour + 2*time.Second + 1,
	} {
		if got, want := string(AppendDuration(nil, d)), d.String(); got != want {
			t.Errorf("AppendDuration(%d) = %q, want %q", int64(d), got, want)
		}
	}
	if got := string(AppendDuration([]byte("x="), time.Minute)); got != "x=1m0s" {
		t.Fatalf("AppendDuration does not append: %q", got)
	}
}

func FuzzAppendDuration(f *testing.F) {
	for _, d := range []int64{0, 1, 1000, 1500000, 1e9, 60e9, 86400e9, -1, 1<<63 - 1, -1 << 63} {
		f.Add(d)
	}
	f.Fuzz(func(t *testing.T, n int64) {
		d := time.Duration(n)
		if got, want := string(AppendDuration(nil, d)), d.String(); got != want {
			t.Fatalf("AppendDuration(%d) = %q, want %q", n, got, want)
		}
	})
}

func FuzzEscape(f *testing.F) {
	// 08 section 6.3: key escaping is injective and never emits '{', '}'
	// or ':'.
	for _, s := range []string{"", "monthly-requests", "a{b}", "a:b", "%", "é", "%25", "\x00\xff"} {
		f.Add(s, s+"x")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		ea := string(AppendEscaped(nil, a))
		if strings.ContainsAny(ea, "{}:") {
			t.Fatalf("AppendEscaped(%q) = %q contains a reserved byte", a, ea)
		}
		if !utf8.ValidString(ea) {
			t.Fatalf("AppendEscaped(%q) = %q is not ASCII", a, ea)
		}
		back, ok := Unescape(ea)
		if !ok || back != a {
			t.Fatalf("Unescape(AppendEscaped(%q)) = %q, %v", a, back, ok)
		}
		if eb := string(AppendEscaped(nil, b)); a != b && ea == eb {
			t.Fatalf("AppendEscaped not injective: %q and %q both give %q", a, b, ea)
		}
		// Keys built from distinct names differ too.
		d := statestore.DigestOf("k")
		ka := AppendQuota(nil, a, time.Hour, time.UnixMilli(0), d)
		kb := AppendQuota(nil, b, time.Hour, time.UnixMilli(0), d)
		if a != b && bytes.Equal(ka, kb) {
			t.Fatalf("quota keys collide for %q and %q", a, b)
		}
		if tag := HashTag(string(ka)); tag != string(AppendHex(nil, d)) {
			t.Fatalf("quota key %q hashes %q, not its digest tag", ka, tag)
		}
	})
}

func TestBuildersDoNotAllocate(t *testing.T) {
	// 08 section 3.1: key builders never allocate when dst has capacity.
	d := statestore.DigestOf("user-42")
	ck := statestore.CacheKey{URI: d, Partition: d}
	l := statestore.GCRALimit{Requests: 5000, Window: time.Minute}
	start := time.UnixMilli(1757376000000)
	buf := make([]byte, 0, 256)
	cases := map[string]func(){
		"AppendRateLimit":       func() { buf = AppendRateLimit(buf[:0], "ratelimit-gold", l, d) },
		"AppendQuota":           func() { buf = AppendQuota(buf[:0], "monthly-requests", 720*time.Hour, start, d) },
		"AppendCacheGeneration": func() { buf = AppendCacheGeneration(buf[:0], d) },
		"AppendCachePartition":  func() { buf = AppendCachePartition(buf[:0], ck) },
		"AppendCacheLease":      func() { buf = AppendCacheLease(buf[:0], ck) },
		"AppendField":           func() { buf = AppendField(buf[:0], FieldEntry, d) },
		"AppendDuration":        func() { buf = AppendDuration(buf[:0], 1500*time.Millisecond) },
		"AppendEscaped":         func() { buf = AppendEscaped(buf[:0], "a{b}:é") },
	}
	for name, fn := range cases {
		if n := testing.AllocsPerRun(100, fn); n != 0 {
			t.Errorf("%s allocates %v times per call", name, n)
		}
	}
}

func BenchmarkAppendRateLimit(b *testing.B) {
	d := statestore.DigestOf("user-42")
	l := statestore.GCRALimit{Requests: 5000, Window: time.Minute}
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		buf = AppendRateLimit(buf[:0], "ratelimit-gold", l, d)
	}
}

func BenchmarkAppendQuota(b *testing.B) {
	d := statestore.DigestOf("user-42")
	start := time.UnixMilli(1757376000000)
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		buf = AppendQuota(buf[:0], "monthly-requests", 720*time.Hour, start, d)
	}
}
