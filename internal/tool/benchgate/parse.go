// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// sample is one benchmark result line.
type sample struct {
	allocs float64 // allocs/op
	bytes  float64 // B/op
}

var errNoBenchmem = errors.New("benchmark line has no allocs/op or B/op: run with -test.benchmem")

// parseBench reads `go test -bench -benchmem` output and appends one sample
// per result line to out, keyed by benchmark name without the "-<cpu>"
// suffix the testing package adds when cpu > 1. Lines that are not results
// (headers, logs, a name printed before its result) are skipped.
func parseBench(data []byte, cpu int, out map[string][]sample) error {
	suffix := "-" + strconv.Itoa(cpu)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "Benchmark") {
			continue
		}
		if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
			continue
		}
		name := fields[0]
		if cpu > 1 {
			name = strings.TrimSuffix(name, suffix)
		}
		var s sample
		var haveAllocs, haveBytes bool
		for i := 2; i+1 < len(fields); i += 2 {
			v, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				return fmt.Errorf("%s: value %q: %w", name, fields[i], err)
			}
			switch fields[i+1] {
			case "allocs/op":
				s.allocs, haveAllocs = v, true
			case "B/op":
				s.bytes, haveBytes = v, true
			}
		}
		if !haveAllocs || !haveBytes {
			return fmt.Errorf("%s: %w", name, errNoBenchmem)
		}
		out[name] = append(out[name], s)
	}
	return sc.Err()
}

// median returns the median of xs: the middle value for an odd count and
// the mean of the two middle values for an even count. ok is false for no
// values.
func median(xs []float64) (m float64, ok bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2], true
	}
	return (s[n/2-1] + s[n/2]) / 2, true
}
