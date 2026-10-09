// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrRemoteReference marks a schema that references a resource outside the
// compiled document: the loader refuses every URL, so compiling never
// reads a file or the network (01 req 32, 01 risk 30).
var ErrRemoteReference = errors.New("schemaview: remote references are not allowed")

// vocabularyURL identifies the Ruralz vocabulary. It is a name, never
// fetched: the rendered view does not declare $vocabulary, and
// AssertVocabs activates the vocabulary for every schema.
const vocabularyURL = "urn:ruralz:vocabulary:x-ruralz"

// vocabularyMeta is the meta-schema of the seven x-ruralz-* keywords
// (docs/architecture/02-configuration-model.md "Schema keywords that drive
// tooling"). With AssertVocabs every schema object of a compiled document
// is validated against it, so a malformed keyword fails compilation, and
// the vocabulary is closed: an eighth x-ruralz-* keyword is rejected. The
// shapes are the documented ones, which the generator's markers produce:
// x-ruralz-ref names one of the ten kinds (foundation pack section 3),
// x-ruralz-secret is true, x-ruralz-impact lists distinct classes and
// x-ruralz-since is at least 1 (omitted for level 0). They are stricter
// than what internal/config/schemaidx accepts, never looser.
const vocabularyMeta = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "propertyNames": {
    "anyOf": [
      {"not": {"pattern": "^x-ruralz-"}},
      {"enum": ["x-ruralz-ref", "x-ruralz-secret", "x-ruralz-cel", "x-ruralz-list",
                "x-ruralz-impact", "x-ruralz-since", "x-ruralz-validations"]}
    ]
  },
  "properties": {
    "x-ruralz-ref": {"enum": ["Gateway", "Route", "Upstream", "Policy", "Plugin", "Consumer",
                              "AIProvider", "AIModel", "Environment", "Cluster"]},
    "x-ruralz-secret": {"const": true},
    "x-ruralz-cel": {
      "type": "object",
      "required": ["variables", "result"],
      "additionalProperties": false,
      "properties": {
        "variables": {"type": "array", "minItems": 1, "items": {"type": "string"}},
        "result": {"enum": ["bool", "string", "dyn"]}
      }
    },
    "x-ruralz-list": {
      "type": "object",
      "required": ["type"],
      "additionalProperties": false,
      "properties": {
        "type": {"enum": ["map", "orderedMap", "set", "atomic"]},
        "key": {"type": "string", "minLength": 1}
      },
      "if": {"properties": {"type": {"enum": ["map", "orderedMap"]}}},
      "then": {"required": ["key"]},
      "else": {"not": {"required": ["key"]}}
    },
    "x-ruralz-impact": {
      "type": "array",
      "minItems": 1,
      "uniqueItems": true,
      "items": {"enum": ["ai", "metadata", "plugin", "routing", "security", "traffic"]}
    },
    "x-ruralz-since": {"type": "integer", "minimum": 1},
    "x-ruralz-validations": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["rule"],
        "additionalProperties": false,
        "properties": {
          "rule": {"type": "string", "minLength": 1},
          "message": {"type": "string"}
        }
      }
    }
  }
}`

// refusingLoader refuses every URL. Resources added to a compiler with
// AddResource are served without a load, so only references that leave
// the compiled documents reach it.
type refusingLoader struct{}

// Load always fails with ErrRemoteReference.
func (refusingLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("%w: %s", ErrRemoteReference, url)
}

// keywords is the parsed form of the x-ruralz-* keywords of one schema
// object, kept in jsonschema.Schema.Extensions (01 req 32). Navigation
// reads the same keywords through internal/config/schemaidx (architecture
// R-3); the vocabulary only parses them, asserts their shapes and lets the
// meta test cross-check both parsers.
type keywords struct {
	// ref is x-ruralz-ref.
	ref string
	// secret is x-ruralz-secret.
	secret bool
	// celVariables and celResult are x-ruralz-cel.
	celVariables []string
	celResult    string
	// listType and listKey are x-ruralz-list.
	listType string
	listKey  string
	// impact is x-ruralz-impact.
	impact []string
	// since is x-ruralz-since.
	since int
	// validations are the x-ruralz-validations rules (not evaluated in M1).
	validations []validation
}

// validation is one x-ruralz-validations rule.
type validation struct {
	rule    string
	message string
}

// Validate implements jsonschema.SchemaExt. The keywords are annotations:
// they never fail an instance. Keyed-list uniqueness, which JSON Schema
// cannot express, is checked on the positioned tree (checkLists), because
// a custom error kind would need golang.org/x/text (01 risk 29).
func (*keywords) Validate(*jsonschema.ValidatorContext, any) {}

// compileKeywords parses the x-ruralz-* keywords of obj. It returns nil
// when obj carries none. The meta-schema has already asserted the shapes;
// the checks here keep a schema compiled without AssertVocabs from
// producing a half-parsed keyword.
func compileKeywords(_ *jsonschema.CompilerContext, obj map[string]any) (jsonschema.SchemaExt, error) {
	var k keywords
	found := false
	for name, v := range obj {
		if !strings.HasPrefix(name, "x-ruralz-") {
			continue
		}
		found = true
		if err := k.parse(name, v); err != nil {
			return nil, err
		}
	}
	if !found {
		return nil, nil
	}
	return &k, nil
}

// errMalformed marks a keyword whose value has the wrong shape.
var errMalformed = errors.New("schemaview: malformed keyword")

// parse reads one x-ruralz-* keyword into k.
func (k *keywords) parse(name string, v any) error {
	bad := fmt.Errorf("%w %s", errMalformed, name)
	switch name {
	case "x-ruralz-ref":
		s, ok := v.(string)
		if !ok || s == "" {
			return bad
		}
		k.ref = s
	case "x-ruralz-secret":
		if b, ok := v.(bool); !ok || !b {
			return bad
		}
		k.secret = true
	case "x-ruralz-cel":
		m, ok := v.(map[string]any)
		if !ok {
			return bad
		}
		vars, ok := stringArray(m["variables"])
		result, okR := m["result"].(string)
		if !ok || !okR {
			return bad
		}
		k.celVariables, k.celResult = vars, result
	case "x-ruralz-list":
		m, ok := v.(map[string]any)
		if !ok {
			return bad
		}
		t, ok := m["type"].(string)
		if !ok {
			return bad
		}
		k.listType = t
		k.listKey, _ = m["key"].(string)
	case "x-ruralz-impact":
		l, ok := stringArray(v)
		if !ok {
			return bad
		}
		k.impact = l
	case "x-ruralz-since":
		n, ok := v.(json.Number)
		if !ok {
			return bad
		}
		i, err := n.Int64()
		if err != nil || i < 1 || i > 1<<20 {
			return bad
		}
		k.since = int(i)
	case "x-ruralz-validations":
		l, ok := v.([]any)
		if !ok {
			return bad
		}
		for _, e := range l {
			r, ok := e.(map[string]any)
			rule, okRule := r["rule"].(string)
			message, okMsg := r["message"].(string)
			if _, has := r["message"]; !ok || !okRule || rule == "" || (has && !okMsg) {
				return bad
			}
			k.validations = append(k.validations, validation{rule: rule, message: message})
		}
	default:
		return fmt.Errorf("schemaview: unknown keyword %s", name)
	}
	return nil
}

// stringArray converts a JSON array of strings.
func stringArray(v any) ([]string, bool) {
	l, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// vocabulary returns the Ruralz vocabulary with its compiled meta-schema.
func vocabulary() (*jsonschema.Vocabulary, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(vocabularyMeta))
	if err != nil {
		return nil, fmt.Errorf("schemaview: vocabulary meta-schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(refusingLoader{})
	if err := c.AddResource(vocabularyURL, doc); err != nil {
		return nil, fmt.Errorf("schemaview: vocabulary meta-schema: %w", err)
	}
	sch, err := c.Compile(vocabularyURL)
	if err != nil {
		return nil, fmt.Errorf("schemaview: vocabulary meta-schema: %w", err)
	}
	return &jsonschema.Vocabulary{URL: vocabularyURL, Schema: sch, Compile: compileKeywords}, nil
}

// newCompiler returns a compiler configured as every Ruralz schema is
// compiled (01 req 32): draft 2020-12, formats and content keywords as
// annotations only (the library defaults: no AssertFormat, no
// AssertContent), Go regexp (RE2) patterns, a loader that refuses every
// URL, and the Ruralz vocabulary asserted on every schema object.
func newCompiler() (*jsonschema.Compiler, error) {
	vocab, err := vocabulary()
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(refusingLoader{})
	c.RegisterVocabulary(vocab)
	c.AssertVocabs()
	return c, nil
}
