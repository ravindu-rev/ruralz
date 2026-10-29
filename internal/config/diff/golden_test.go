// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"bytes"
	"context"
	"testing"
)

// TestGoldenShopExample pins both output forms of the configuration
// model's diff example: TO is the example Bundle rendered for prod; FROM is
// the earlier state that listed the secondary candidate first, granted no
// response.send, had a 5s timeout and a legacy-orders Upstream, and lacked
// the ratelimit-orders Policy. Every worker count writes the same bytes.
func TestGoldenShopExample(t *testing.T) {
	from := render(t, shop(t, "from"))
	to := render(t, shop(t, "to"))
	var text, js []byte
	for _, workers := range []int{1, 2, 8} {
		d, err := engine(t, workers).Compare(context.Background(),
			Side{State: from, Environment: "prod"},
			Side{State: to, Origin: OriginBundle, Location: "./shop-bundle", Environment: "prod"})
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
	golden(t, "shop.txt", text)
	golden(t, "shop.json", js)
}

// TestGoldenNoChanges pins both forms of a diff of a state with itself.
func TestGoldenNoChanges(t *testing.T) {
	to := render(t, shop(t, "to"))
	d, err := engine(t, 1).Compare(context.Background(),
		Side{State: to, Origin: OriginBundle, Location: "./shop-bundle", Environment: "prod"},
		Side{State: dumped(t, to), Origin: OriginAdmin, Location: "https://node-a.shop.example:9901"})
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	if d.Changed() {
		t.Fatalf("Changed() = true for a state and its own /config/dump:\n%s", d.AppendText(nil))
	}
	golden(t, "nochange.txt", d.AppendText(nil))
	golden(t, "nochange.json", d.AppendJSON(nil))
}
