// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

const baseDir = "testdata/base"

// kindName names a secret.Kind for the want files.
func kindName(k secret.Kind) string {
	switch k {
	case secret.KindOpaque:
		return "opaque"
	case secret.KindPEMCertificate:
		return "pem-certificate"
	case secret.KindPEMPrivateKey:
		return "pem-private-key"
	case secret.KindPEMCertPool:
		return "pem-cert-pool"
	case secret.KindPEMCRL:
		return "pem-crl"
	case secret.KindAPIKey:
		return "api-key"
	case secret.KindStateStoreURL:
		return "state-store-url"
	default:
		return fmt.Sprintf("kind-%d", k)
	}
}

// secretsText returns one line per secret use: resource, path, reference,
// kind, destination and location.
func secretsText(uses []hub.SecretUse) string {
	var b strings.Builder
	for _, u := range uses {
		fmt.Fprintf(&b, "%s/%s %s %s kind=%s dest=%s at %s:%d:%d\n",
			u.Resource.Kind, u.Resource.Name, u.Path, secret.RefOf(u.Ref), kindName(u.Kind), u.Destination,
			u.Loc.File, u.Loc.Line, u.Loc.Column)
	}
	return b.String()
}

// TestBaseIsClean: the base Bundle every fixture edits passes stage H, so
// a fixture's findings come from its own edit; its secret uses carry their
// kinds and destinations.
func TestBaseIsClean(t *testing.T) {
	b, files := loadDirs(t, baseDir)
	res, ds := stageH(t, newValidator(t, nil), b, files, 2)
	if len(ds) > 0 {
		t.Fatalf("base Bundle has findings:\n%s", diagText(t, ds))
	}
	golden(t, "testdata/base.secrets.txt", secretsText(res.Secrets))
}

// TestExampleBundle: the example Bundle of the Configuration model, read
// from its golden corpus entry (loadShop), passes stage H, and its secret
// uses carry the kinds and destinations of "Secret-to-destination
// binding": the clientSecret to its tokenUrl origin, the State Store URL
// to the State Store, everything else local. The secret uses are this
// package's own form, which no pipeline output holds, so their golden file
// stays here.
func TestExampleBundle(t *testing.T) {
	b, files := loadShop(t)
	for _, workers := range []int{1, 4} {
		res, ds := stageH(t, newValidator(t, nil), b, files, workers)
		if len(ds) > 0 {
			t.Fatalf("workers %d: example Bundle has findings:\n%s", workers, diagText(t, ds))
		}
		golden(t, "testdata/shop-bundle.secrets.txt", secretsText(res.Secrets))
	}
}
