// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/test/compose/mock"
)

// sinkCheckName is the name the air-gapped project's probe looks up
// through the DNS sink itself, which proves the sink answers and records.
const sinkCheckName = "rz-sink-check.invalid."

// resolvConf names the DNS sink, on loopback in the network namespace the
// air-gapped Node shares with the mock Upstream, as the Node's only
// resolver.
const resolvConf = "nameserver 127.0.0.1\noptions ndots:1 timeout:1 attempts:1\n"

// dnsLogProblems checks a DNS sink's record from the air-gapped start: it
// holds the probe's own check, so the sink answered and recorded, and no
// other name but those of the network; it dropped nothing and read no
// message it could not name.
func dnsLogProblems(log mock.DNSLog, network []string) []string {
	var problems []string
	if log.Dropped != 0 || log.Malformed != 0 {
		problems = append(problems, fmt.Sprintf("the DNS sink dropped %d questions and read %d malformed messages, want none", log.Dropped, log.Malformed))
	}
	checked := false
	for _, q := range log.Queries {
		switch name := strings.TrimSuffix(q.Name, "."); {
		case q.Name == sinkCheckName:
			checked = true
		case slices.Contains(network, name):
		default:
			problems = append(problems, fmt.Sprintf("the Node looked up %s (%s over %s), a name outside the network", q.Name, q.Type, q.Transport))
		}
	}
	if !checked {
		problems = append(problems, fmt.Sprintf("the DNS sink's log %+v lacks the probe's own check %s", log.Queries, sinkCheckName))
	}
	return problems
}

func TestDNSLogProblems(t *testing.T) {
	network := []string{svcUpstream, svcRuralzd, svcProbe}
	check := mock.DNSQuery{Name: sinkCheckName, Type: "A", Transport: "udp"}
	tests := []struct {
		name string
		log  mock.DNSLog
		want string
	}{
		{"only the probe's check", mock.DNSLog{Queries: []mock.DNSQuery{check, {Name: sinkCheckName, Type: "AAAA"}}}, ""},
		{"a name of the network", mock.DNSLog{Queries: []mock.DNSQuery{check, {Name: "upstream.", Type: "A"}}}, ""},
		{"a lookup outside the network", mock.DNSLog{Queries: []mock.DNSQuery{check, {Name: "updates.example.", Type: "A", Transport: "udp"}}}, "looked up updates.example."},
		{"no check: the sink was not reached", mock.DNSLog{}, "lacks the probe's own check"},
		{"a dropped question", mock.DNSLog{Queries: []mock.DNSQuery{check}, Dropped: 1}, "dropped 1 questions"},
		{"a malformed message", mock.DNSLog{Queries: []mock.DNSQuery{check}, Malformed: 1}, "1 malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(dnsLogProblems(tt.log, network), "; ")
			switch {
			case tt.want == "" && got != "":
				t.Errorf("dnsLogProblems = %q, want none", got)
			case tt.want != "" && !strings.Contains(got, tt.want):
				t.Errorf("dnsLogProblems = %q, want %q", got, tt.want)
			}
		})
	}
}
