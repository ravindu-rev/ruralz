// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"regexp"
	"strings"
)

// detector recognizes one credential shape (01 J row 013). needles are
// substrings at least one of which every match contains; the regular
// expression runs only when one occurs, so ordinary strings cost a few
// substring searches.
type detector struct {
	name    string
	needles []string
	re      *regexp.Regexp
}

// DetectorVersion versions the credential detector list (01 risk 35): a
// change to the list or a pattern changes it.
const DetectorVersion = 1

// newDetectors compiles the versioned credential detector list of the
// 01 J row for RZ-CFG-013 (spec 01 risk 35). The messages name the
// detector, never the value.
func newDetectors() []detector {
	return []detector{
		{
			name: "PEM private key", needles: []string{"PRIVATE KEY-----"},
			re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		},
		{
			name: "URL with a password", needles: []string{"://"},
			re: regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://[^/@:\s]+:[^/@\s]+@`),
		},
		{
			name: "JSON Web Token", needles: []string{"eyJ"},
			re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
		},
		{
			name: "AWS access key ID", needles: []string{"AKIA", "ASIA"},
			re: regexp.MustCompile(`(AKIA|ASIA)[0-9A-Z]{16}`),
		},
		{
			name: "GitHub token", needles: []string{"gh"},
			re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),
		},
		{
			name: "GitHub fine-grained token", needles: []string{"github_pat_"},
			re: regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`),
		},
		{
			name: "Slack token", needles: []string{"xox"},
			re: regexp.MustCompile(`xox[abposr]-[A-Za-z0-9-]{10,}`),
		},
		{
			name: "API secret key", needles: []string{"sk-"},
			re: regexp.MustCompile(`sk-(ant-)?[A-Za-z0-9_-]{20,}`),
		},
		{
			name: "Authorization header credentials", needles: []string{"Bearer ", "Basic "},
			re: regexp.MustCompile(`(Bearer|Basic) [A-Za-z0-9._~+/-]{16,}=*`),
		},
	}
}

// detect returns the name of the first detector s matches, or "".
func detect(ds []detector, s string) string {
	for i := range ds {
		d := &ds[i]
		for _, n := range d.needles {
			if strings.Contains(s, n) {
				if d.re.MatchString(s) {
					return d.name
				}
				break
			}
		}
	}
	return ""
}

// credentialHeader reports a header name whose literal headers set[]
// value is credential-shaped by name (01 J row 013).
func credentialHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "x-api-key":
		return true
	default:
		return false
	}
}
