// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// TestGoldenShopExample checks both output forms of the configuration
// model's diff example against the golden corpus entry that pins them
// (diff/prod.prod.txt and .json): TO is the example Bundle rendered for
// prod; FROM is the earlier state that listed the secondary candidate
// first, granted no response.send, had a 5s timeout and a legacy-orders
// Upstream, and lacked the ratelimit-orders Policy. Every worker count
// writes the same bytes.
func TestGoldenShopExample(t *testing.T) {
	from := render(t, shop(t, corpus.FromDir))
	to := render(t, shop(t, corpus.ToDir))
	var text, js []byte
	for _, workers := range []int{1, 2, 8} {
		d, err := engine(t, workers).Compare(context.Background(),
			Side{State: from, Environment: diffEnv},
			Side{State: to, Origin: OriginBundle, Location: "./shop-bundle", Environment: diffEnv})
		if err != nil {
			t.Fatalf("Compare() error = %v", err)
		}
		gotText, gotJSON := d.AppendText(nil), d.AppendJSON(nil)
		if text == nil {
			text, js = gotText, gotJSON
			continue
		}
		if !bytes.Equal(gotText, text) || !bytes.Equal(gotJSON, js) {
			t.Errorf("workers %d changed the output", workers)
		}
	}
	sameAsCorpus(t, "diff/"+diffEnv+"."+diffEnv+".txt", text)
	sameAsCorpus(t, "diff/"+diffEnv+"."+diffEnv+".json", js)
}

// TestGoldenNoChanges pins both forms of a diff of a state with its own
// /config/dump, a drift check against a Node that finds none: the
// example Bundle rendered for prod, whose digest the golden corpus pins,
// names its Bundle and Environment on the FROM side and the Node's admin
// address on the TO side, and no resource, chain or impact.
func TestGoldenNoChanges(t *testing.T) {
	to := render(t, shop(t, corpus.ToDir))
	var exp struct {
		Digests map[string]string `json:"digests"`
	}
	if err := json.Unmarshal(diffLevel(t).Expected, &exp); err != nil {
		t.Fatalf("%s %s: %v", diffEntry, corpus.ExpectedFile, err)
	}
	digest, err := revision.Parse(exp.Digests[corpus.ToDir+"/"+diffEnv])
	if err != nil {
		t.Fatalf("%s %s: the digest of to/ rendered for %s: %v", diffEntry, corpus.ExpectedFile, diffEnv, err)
	}
	if to.Revision.Digest != digest {
		t.Fatalf("the example Bundle rendered for %s has digest %s, the golden corpus pins %s", diffEnv, to.Revision.Digest, digest)
	}
	d, err := engine(t, 1).Compare(context.Background(),
		Side{State: to, Origin: OriginBundle, Location: "./shop-bundle", Environment: diffEnv},
		Side{State: dumped(t, to), Origin: OriginAdmin, Location: "https://node-a.shop.example:9901"})
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	if d.Changed() {
		t.Fatalf("Changed() = true for a state and its own /config/dump:\n%s", d.AppendText(nil))
	}
	r := strings.NewReplacer("REV", digest.Short(), "DIGEST", digest.String(), "ENV", diffEnv)
	wantText := r.Replace("ruralz bundle diff: REV -> REV (environment: ENV)\n\n" +
		"0 resources changed: 0 added, 0 removed, 0 modified; 0 Route chains changed; impact: none\n")
	wantJSON := r.Replace(`{
  "format": "ruralz.diff.v1",
  "from": {
    "revision": "REV",
    "bundle": "./shop-bundle",
    "digest": "DIGEST"
  },
  "to": {
    "revision": "REV",
    "admin": "https://node-a.shop.example:9901",
    "digest": "DIGEST"
  },
  "environment": "ENV",
  "resources": [],
  "effective": [],
  "summary": {
    "changed": 0,
    "added": 0,
    "removed": 0,
    "modified": 0,
    "routeChainsChanged": 0,
    "impact": []
  }
}
`)
	if got := string(d.AppendText(nil)); got != wantText {
		t.Errorf("text form:\n%s\nwant\n%s", got, wantText)
	}
	if got := string(d.AppendJSON(nil)); got != wantJSON {
		t.Errorf("JSON form:\n%s\nwant\n%s", got, wantJSON)
	}
}
