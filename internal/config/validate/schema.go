// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"encoding/json"
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// gatewayDefaults reads, from the hub schema, the defaults of the Gateway
// fields stage H compares against: admin.port, limits.maxCompositionSteps
// and limits.maxResponseBodyBytes. The schema is their only source, so a
// changed default needs no code change here.
func gatewayDefaults(idx *schemaidx.Index) (schemaDefaults, error) {
	var d schemaDefaults
	spec, ok := idx.Spec(string(v1alpha1.KindGateway))
	if !ok {
		return d, fmt.Errorf("the hub schema has no %s", v1alpha1.KindGateway)
	}
	var err error
	if d.adminPort, err = defaultInt(spec, "admin", "port"); err != nil {
		return d, err
	}
	if d.maxSteps, err = defaultInt(spec, "limits", "maxCompositionSteps"); err != nil {
		return d, err
	}
	if d.maxResponse, err = defaultInt(spec, "limits", "maxResponseBodyBytes"); err != nil {
		return d, err
	}
	return d, nil
}

// defaultInt returns the integer default of the member path below n: a
// number, or a byte size quantity string.
func defaultInt(n *schemaidx.Node, path ...string) (int64, error) {
	cur := n
	for _, name := range path {
		next, ok := cur.Property(name)
		if !ok {
			return 0, fmt.Errorf("the hub schema has no Gateway spec member %v", path)
		}
		cur = next
	}
	v, ok := cur.Default()
	if !ok {
		return 0, fmt.Errorf("the hub schema gives Gateway spec member %v no default", path)
	}
	switch t := v.(type) {
	case json.Number:
		i, err := t.Int64()
		if err != nil {
			return 0, fmt.Errorf("the default of Gateway spec member %v is not an integer: %w", path, err)
		}
		return i, nil
	case string:
		b, err := v1alpha1.ParseByteSize(t)
		if err != nil {
			return 0, fmt.Errorf("the default of Gateway spec member %v: %w", path, err)
		}
		return int64(b), nil
	default:
		return 0, fmt.Errorf("the default of Gateway spec member %v is %T, not a number", path, v)
	}
}
