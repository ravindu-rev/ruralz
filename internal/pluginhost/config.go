// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"fmt"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/pluginhost/configschema"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// configured is a pool's configured state: the rz_configure input, the
// host-held keys its handles name, and the resolved secret values guest
// logs redact.
type configured struct {
	input   []byte
	keys    []*hostKey
	secrets [][]byte
}

// keyOpsOf returns the operations of a key field's declaration.
func keyOpsOf(ops []string) keyOp {
	var k keyOp
	for _, o := range ops {
		switch o {
		case configschema.OpSign:
			k |= opSign
		case configschema.OpVerify:
			k |= opVerify
		case configschema.OpHMAC:
			k |= opHMAC
		}
	}
	return k
}

// configureInput builds the rz_configure input of a Policy config: the
// schema-checked config as RFC 8785 canonical JSON, with each secret
// field's Node-resolved value as a string in place of its reference and
// each key field's handle, numbered from 1 in path order, in place of its
// reference (docs/architecture/05-wasm-plugin-system.md "Contract"). A
// literal in a marked field, or a reference the store does not resolve,
// fails; so does key material that does not fit its declaration.
func configureInput(fields []configschema.Field, cfg map[string]any, store secret.Store) (*configured, error) {
	doc := jsonval.Clone(cfg)
	out := &configured{}
	for _, r := range configschema.Refs(fields, cfg) {
		at := configschema.PathText(r.At)
		if r.Literal {
			return nil, fmt.Errorf("%s holds a value that is not a {secretRef: ...}", at)
		}
		if store == nil {
			return nil, fmt.Errorf("%s: no resolved secret table", at)
		}
		val, ok := store.Get(secret.RefOf(r.Ref))
		if !ok {
			return nil, fmt.Errorf("%s: secretRef %s is not resolved on this Node", at, secret.RefOf(r.Ref))
		}
		b := val.Reveal()
		var repl any
		if r.Field.Key {
			k, err := parseKey(r.Field.Algorithm, keyOpsOf(r.Field.Operations), b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
			out.keys = append(out.keys, k)
			repl = jsonNumber(len(out.keys))
		} else {
			repl = string(b)
			out.secrets = append(out.secrets, b)
		}
		doc = replaceAt(doc, r.At, repl)
	}
	input, err := jsonval.AppendCanonical(nil, doc)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	out.input = input
	return out, nil
}

// replaceAt replaces the value at the instance path at of doc.
func replaceAt(doc any, at []string, v any) any {
	if len(at) == 0 {
		return v
	}
	switch d := doc.(type) {
	case map[string]any:
		d[at[0]] = replaceAt(d[at[0]], at[1:], v)
		return d
	case []any:
		if i, err := strconv.Atoi(at[0]); err == nil && i >= 0 && i < len(d) {
			d[i] = replaceAt(d[i], at[1:], v)
		}
		return d
	default:
		return doc
	}
}
