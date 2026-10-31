// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"errors"
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// DecodeJSON decodes one JSON document for CEL: a body or JWT claims. The
// decode is strict: invalid UTF-8 and repeated member names at any depth
// are errors (docs/architecture/06-ai-llm-gateway.md "Strict JSON"). built
// is the decoded size charged to limits.maxBufferedBytes, by jsonval's cost
// model; past budget the error wraps expr.ErrTooLarge, and a budget of 0
// or less means none (docs/architecture/02-configuration-model.md "Body
// buffering and limits"). On any error v reads as a body that did not
// decode, so an expression that selects it fails with KindBody.
func (c *Compiler) DecodeJSON(data []byte, budget int64) (v expr.Value, built int64, err error) {
	tree, cost, err := jsonval.Decode(data, jsonval.Options{MaxCost: budget, MapObjects: true})
	if err != nil {
		if errors.Is(err, jsonval.ErrTooLarge) {
			err = fmt.Errorf("%w: %w", expr.ErrTooLarge, err)
		} else {
			err = fmt.Errorf("cel: decode JSON: %w", err)
		}
		return celtypes.ErrorValue(err), cost, err
	}
	return celtypes.FromNative(tree), cost, nil
}
