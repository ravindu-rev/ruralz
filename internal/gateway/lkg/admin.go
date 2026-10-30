// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lkg

import (
	"bytes"
	"encoding/json"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// AdminPort returns Gateway.spec.admin.port of ruralz.canonical.v1
// content, which holds every default, so a Gateway without one there is
// not in canonical form; false when the content does not decode, has no
// Gateway or no port in 1 to 65535. A Node binds it before its first
// activation when the Bundle it boots does not parse
// (docs/architecture/03-data-plane.md "Admin endpoints"). It does not
// verify the content's digest; the activation does.
func AdminPort(content []byte) (int, bool) {
	var doc struct {
		Resources []struct {
			Kind string `json:"kind"`
			Spec struct {
				Admin struct {
					Port *json.Number `json:"port"`
				} `json:"admin"`
			} `json:"spec"`
		} `json:"resources"`
	}
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return 0, false
	}
	for _, r := range doc.Resources {
		if r.Kind != string(v1alpha1.KindGateway) {
			continue
		}
		if r.Spec.Admin.Port == nil {
			return 0, false
		}
		p, err := r.Spec.Admin.Port.Int64()
		if err != nil || p < 1 || p > 65535 {
			return 0, false
		}
		return int(p), true
	}
	return 0, false
}

// AdminPort returns the admin port of the entry's content (AdminPort).
func (e Entry) AdminPort() (int, bool) { return AdminPort(e.Content) }
