// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var _ hub.PolicyCheck = Check

// Window rules of a limits[] entry (registered in the Configuration
// model): a window of at least 1ms in whole milliseconds, whose emission
// interval window / requests is at least 1µs, above the TAT resolution.
const (
	minWindow   = time.Millisecond
	minInterval = time.Microsecond
)

// problem is one config rule a ratelimit Policy breaks, at a path under
// spec.config.
type problem struct {
	path    diag.Path
	message string
	hint    string
}

// configPath returns spec.config followed by elems.
func configPath(elems ...diag.PathElem) diag.Path {
	return append(diag.Path{diag.Field("spec"), diag.Field("config")}, elems...)
}

// limitPath returns spec.config.limits[i] followed by elems.
func limitPath(i int, elems ...diag.PathElem) diag.Path {
	return configPath(append([]diag.PathElem{diag.Field("limits"), diag.Index(i)}, elems...)...)
}

// problems returns the RZ-CFG-005 rules cfg breaks, in config order: more
// than 8 limits, a requests below 1, a window below 1ms or not in whole
// milliseconds, window / requests below 1µs, a perNodeCeiling outside 1 to
// requests, a burst outside 0 to requests, and two limits with equal
// requests and window, which would share one GCRA key (reported at the
// later one).
func problems(cfg *v1alpha1.RateLimitConfig) []problem {
	var out []problem
	if len(cfg.Limits) == 0 {
		out = append(out, problem{configPath(diag.Field("limits")), "limits is empty", "declare at least one limit, such as {requests: 100, window: 1s}"})
	}
	if len(cfg.Limits) > MaxLimits {
		out = append(out, problem{
			limitPath(MaxLimits), "more than " + strconv.Itoa(MaxLimits) + " limits",
			"keep at most " + strconv.Itoa(MaxLimits) + " limits per Policy",
		})
	}
	type rate struct {
		requests int64
		window   time.Duration
	}
	first := map[rate]int{}
	for i, l := range cfg.Limits {
		w := time.Duration(l.Window)
		switch {
		case l.Requests < 1:
			out = append(out, problem{limitPath(i, diag.Field("requests")), "requests is below 1", ""})
		case w < minWindow || w%time.Millisecond != 0:
			out = append(out, problem{limitPath(i, diag.Field("window")), "window " + w.String() + " is below 1ms or not in whole milliseconds", "write a window such as 1s or 250ms"})
		case w/time.Duration(l.Requests) < minInterval:
			out = append(out, problem{limitPath(i, diag.Field("window")), "window / requests is below 1µs", "lengthen the window or lower requests"})
		}
		if p := l.PerNodeCeiling; p != nil && (*p < 1 || *p > l.Requests) {
			out = append(out, problem{limitPath(i, diag.Field("perNodeCeiling")), "perNodeCeiling " + strconv.FormatInt(*p, 10) + " is outside 1 to requests", ""})
		}
		if b := l.Burst; b != nil && (*b < 0 || *b > l.Requests) {
			out = append(out, problem{limitPath(i, diag.Field("burst")), "burst " + strconv.FormatInt(*b, 10) + " is outside 0 to requests", ""})
		}
		k := rate{l.Requests, w}
		if j, dup := first[k]; dup {
			out = append(out, problem{
				limitPath(i), "limits " + strconv.Itoa(j) + " and " + strconv.Itoa(i) + " have equal requests and window",
				"two such limits would share one GCRA key; remove one",
			})
			continue
		}
		first[k] = i
	}
	return out
}

// Check is the offline check of a ratelimit Policy (a hub.PolicyCheck run
// at stage H by every binary): each rule problems lists is one RZ-CFG-005
// error at its path, so validation refuses what Build refuses. A resource
// whose Config is not a decoded RateLimitConfig yields nothing; the
// schema reports a missing limits.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.RateLimitConfig)
	if !ok || cfg == nil {
		return
	}
	for _, p := range problems(cfg) {
		var l diag.Location
		if loc != nil {
			l = loc(p.path)
		}
		add(diag.Diagnostic{
			Code: CodeConfig, Severity: diag.SeverityError, Location: l,
			Resource: r.ResourceID(), Path: p.path, Message: p.message, Hint: p.hint,
		})
	}
}
