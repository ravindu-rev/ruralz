// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import "strings"

// detector recognizes one credential shape (01 J row 013): match reports
// whether s holds a match of the shape's pattern anywhere, reporting the
// bytes it reads to read.
type detector struct {
	name  string
	match func(s string, read func(int)) bool
}

// DetectorVersion versions the credential detector list (01 risk 35): a
// change to the list or a pattern changes it.
const DetectorVersion = 1

// byteClass is a set of bytes.
type byteClass [256]bool

// classOf returns the byte class of the ranges ("a-z") and single bytes
// in spec.
func classOf(spec string) *byteClass {
	var c byteClass
	// bounded: a constant class spec.
	for i := 0; i < len(spec); i++ {
		if i+2 < len(spec) && spec[i+1] == '-' {
			// bounded: a byte range of a constant class.
			for b := int(spec[i]); b <= int(spec[i+2]); b++ {
				c[b] = true
			}
			i += 2
			continue
		}
		c[spec[i]] = true
	}
	return &c
}

// not returns the bytes outside c.
func (c *byteClass) not() *byteClass {
	var out byteClass
	// bounded: the 256 bytes.
	for b := range out {
		out[b] = !c[b]
	}
	return &out
}

// runEnd returns the end of the run of bytes of class c in s that starts
// at from, reading at most limit bytes when limit is positive, reporting
// what it reads to read in steps of at most textStep bytes.
func runEnd(s string, from int, c *byteClass, limit int, read func(int)) int {
	stop := len(s)
	if limit > 0 {
		stop = min(stop, from+limit)
	}
	i := from
	for i < stop {
		end := min(stop, i+textStep)
		j := i
		// bounded: one window of at most textStep bytes, reported after it.
		for j < end && c[s[j]] {
			j++
		}
		if j < end {
			read(j - i + 1)
			return j
		}
		read(end - i)
		i = end
	}
	return i
}

// newDetectors returns the versioned credential detector list of the 01
// J row for RZ-CFG-013 (spec 01 risk 35). Each matcher finds exactly the
// strings its pattern of the row matches somewhere (TestDetectorsMatchSpec
// and FuzzDetectors compare each with the pattern as a Go regular
// expression), in time linear in the string: a regular expression run
// through the whole string could not report what it reads as it runs,
// and a repeat {n,} is decided by its first n bytes, a run by its end. The
// messages name the detector, never the value.
func newDetectors() []detector {
	upperSpace := classOf("A-Z ")
	letter := classOf("a-zA-Z")
	scheme := classOf("a-zA-Z0-9+.-")
	user := classOf("/@:\t\n\f\r ").not()
	password := classOf("/@\t\n\f\r ").not()
	jwtChar := classOf("A-Za-z0-9_-")
	upperDigit := classOf("0-9A-Z")
	alnum := classOf("A-Za-z0-9")
	alnumUnderscore := classOf("A-Za-z0-9_")
	alnumDash := classOf("A-Za-z0-9-")
	token := classOf("A-Za-z0-9._~+/-")
	github, slack := classOf("pousr"), classOf("abposr")
	return []detector{
		// -----BEGIN [A-Z ]*PRIVATE KEY-----: the run of [A-Z ] after the
		// BEGIN ends where "-----" starts, so it ends with PRIVATE KEY.
		{name: "PEM private key", match: func(s string, read func(int)) bool {
			const begin, key = "-----BEGIN ", "PRIVATE KEY"
			for p := 0; ; {
				i := indexFrom(s, begin, p, read)
				if i < 0 {
					return false
				}
				start := i + len(begin)
				e := runEnd(s, start, upperSpace, 0, read)
				if e-start >= len(key) && s[e-len(key):e] == key && strings.HasPrefix(s[e:], "-----") {
					return true
				}
				p = max(e, i+1)
			}
		}},
		// ^[a-zA-Z][a-zA-Z0-9+.-]*://[^/@:\s]+:[^/@\s]+@: each run ends
		// where the byte after it must stand, which its class excludes.
		{name: "URL with a password", match: func(s string, read func(int)) bool {
			if s == "" || !letter[s[0]] {
				return false
			}
			e1 := runEnd(s, 1, scheme, 0, read)
			if !strings.HasPrefix(s[e1:], "://") {
				return false
			}
			u := e1 + 3
			e2 := runEnd(s, u, user, 0, read)
			if e2 == u || e2 >= len(s) || s[e2] != ':' {
				return false
			}
			e3 := runEnd(s, e2+1, password, 0, read)
			return e3 > e2+1 && e3 < len(s) && s[e3] == '@'
		}},
		// eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}:
		// the first eyJ of a run gives its longest first part, and every
		// later eyJ of the run fails as it does, so the search goes on
		// from the run's end.
		{name: "JSON Web Token", match: func(s string, read func(int)) bool {
			for p := 0; ; {
				i := indexFrom(s, "eyJ", p, read)
				if i < 0 {
					return false
				}
				e1 := runEnd(s, i+3, jwtChar, 0, read)
				if e1-(i+3) >= 8 && e1 < len(s) && s[e1] == '.' && strings.HasPrefix(s[e1+1:], "eyJ") {
					e2 := runEnd(s, e1+4, jwtChar, 0, read)
					if e2-(e1+4) >= 8 && e2 < len(s) && s[e2] == '.' && runEnd(s, e2+1, jwtChar, 8, read)-(e2+1) >= 8 {
						return true
					}
				}
				p = e1
			}
		}},
		{name: "AWS access key ID", match: func(s string, read func(int)) bool {
			return prefixRun(s, "AKIA", upperDigit, 16, read) || prefixRun(s, "ASIA", upperDigit, 16, read)
		}},
		{name: "GitHub token", match: func(s string, read func(int)) bool {
			return pickRun(s, "gh", github, '_', alnum, 36, read)
		}},
		{name: "GitHub fine-grained token", match: func(s string, read func(int)) bool {
			return prefixRun(s, "github_pat_", alnumUnderscore, 22, read)
		}},
		{name: "Slack token", match: func(s string, read func(int)) bool {
			return pickRun(s, "xox", slack, '-', alnumDash, 10, read)
		}},
		// sk-(ant-)?[A-Za-z0-9_-]{20,}: ant- is in the class, so the
		// optional group never decides a match.
		{name: "API secret key", match: func(s string, read func(int)) bool {
			return prefixRun(s, "sk-", jwtChar, 20, read)
		}},
		{name: "Authorization header credentials", match: func(s string, read func(int)) bool {
			return prefixRun(s, "Bearer ", token, 16, read) || prefixRun(s, "Basic ", token, 16, read)
		}},
	}
}

// prefixRun reports whether s holds prefix followed by at least n bytes
// of class c: the pattern prefix c{n,}, decided by its first n bytes.
func prefixRun(s, prefix string, c *byteClass, n int, read func(int)) bool {
	for p := 0; ; {
		i := indexFrom(s, prefix, p, read)
		if i < 0 {
			return false
		}
		start := i + len(prefix)
		if runEnd(s, start, c, n, read)-start >= n {
			return true
		}
		p = i + 1
	}
}

// pickRun reports whether s holds prefix, a byte of pick, sep and at
// least n bytes of class c: the pattern prefix[pick]sep c{n,}.
func pickRun(s, prefix string, pick *byteClass, sep byte, c *byteClass, n int, read func(int)) bool {
	for p := 0; ; {
		i := indexFrom(s, prefix, p, read)
		if i < 0 {
			return false
		}
		k := i + len(prefix)
		if k+1 < len(s) && pick[s[k]] && s[k+1] == sep && runEnd(s, k+2, c, n, read)-(k+2) >= n {
			return true
		}
		p = i + 1
	}
}

// detect returns the name of the first detector s matches, or "".
func detect(ds []detector, s string, read func(int)) string {
	// bounded: the detector list.
	for i := range ds {
		if ds[i].match(s, read) {
			return ds[i].name
		}
	}
	return ""
}

// credentialHeader reports a header name whose literal headers set[]
// value is credential-shaped by name (01 J row 013).
func credentialHeader(name string) bool {
	if len(name) > len("proxy-authorization") {
		return false
	}
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "x-api-key":
		return true
	default:
		return false
	}
}
