---
paths:
  - "pkg/config/**"
  - "api/schema/**"
  - "internal/tool/schemagen/**"
---

# Configuration types and the generated JSON Schema

Authority: `docs/architecture/02-configuration-model.md` owns kinds, fields, Policy types and `RZ-CFG` codes; `docs/engineering/02-repository-layout-and-conventions.md` section "Schema generation from Go types" owns the markers. Use `/change-config-schema` for the full recipe.

## Ground rules

- The Go types in `pkg/config/v1alpha1` are the only source of `api/schema/ruralz/v1alpha1/{authoring,rendered}.schema.json`. Never edit the JSON by hand.
- Add or change a field only when `02-configuration-model.md` defines it (its Kind catalog sketches are normative for names, `# required` and list types). Otherwise add an `OQ-configuration-model-<n>` row there and stop.
- `pkg/` non-test files import only the standard library and `pkg/` (repocheck and depguard). No loader, validator or YAML exists yet; do not add one here. `pkg/` and `api/` changes need two approvals.
- The 10 kinds are fixed (foundation pack section 3). Adding one needs a foundation pack amendment: an ADR or an Open questions entry (section 14), with two approvals because it changes `docs/_meta/`, `pkg/` and `api/`.
- Schema levels: every field added within an apiVersion carries `x-ruralz-since`, and changing a `v1alpha1` default is a new schema level. No field carries `+ruralz:since` yet. Do not pick a level or change an existing `+ruralz:default=` yourself: ask, or add an `OQ-configuration-model-<n>` row and stop.

## Regenerate and check

1. Edit the types and their `+ruralz:` markers.
2. `go generate ./pkg/config/v1alpha1` (directive at `pkg/config/v1alpha1/doc.go:21`).
3. `go test ./internal/tool/schemagen/ ./api/schema/ ./pkg/...`. `TestCommittedSchemaIsCurrent` fails when the committed JSON is stale; `TestDeterministic` checks byte-identical output across runs, LF only, a trailing newline and no HTML escaping (key order comes from `encoding/json` map sorting).
4. Stage the types and both JSON files together. `make generate` diffs the working tree against the index, so it fails until the regenerated files are staged.

## Field rules schemagen enforces

- One field per line, with a `json` tag that names it (no missing or `-` tag).
- A field is required only with `+ruralz:required`; every other field has `omitempty`.
- Optional bools and numbers, and optional fields whose default is not the Go zero value, are pointers. `+ruralz:default=` is allowed only on a pointer field.
- Use the package scalar types: `Duration` for durations (never `time.Duration`), `ByteSize` for byte sizes, `Decimal` for prices, `IntOrString` for a port number or name, `SecretValue` for secrets. `Duration` and `ByteSize` are integer kinds, so an optional one is a pointer. Use signed integers for numbers.
- Every slice except `json.RawMessage` has `+ruralz:list=...`.
- `SecretValue` and `SecretRef` fields carry `+ruralz:secret`, except the `SecretRef` field inside `SecretValue` itself, which must not.
- Map keys are strings. No anonymous structs, no struct types from other packages, no embedded fields outside resource types.
- A typed string `const` block becomes the type's `enum`, in source order. Doc comments become `description`; write them as user-facing text.

## Markers (compact)

| Marker | Where | Effect |
|---|---|---|
| `required` | field | Adds to the parent's `required` |
| `secret` | field | `x-ruralz-secret`; no `${VAR}` substitution |
| `ref=<Kind>` | field | `x-ruralz-ref`; value must be a Kind constant |
| `cel=<vars>:<bool\|string\|dyn>` | field | `x-ruralz-cel` |
| `list=<map\|orderedMap\|set\|atomic>[,key=<field>]` | slice field | `x-ruralz-list`; `map` and `orderedMap` need a key, `set` and `atomic` must not have one |
| `impact=<classes>` | field | `x-ruralz-impact`; classes `routing`, `security`, `traffic`, `plugin`, `ai`, `metadata` |
| `since=<n>` | field | `x-ruralz-since`, n of 1 or more |
| `validation=<CEL rule>` | field, repeatable | Appends to `x-ruralz-validations` |
| `default=<value>` | pointer field | `default`, typed (Duration canonical string, byte sizes as integers, quoted strings allowed) |
| `policyType=<type>` | config struct | Selects the config by `Policy.spec.type` |
| `open` | struct | Omits `additionalProperties: false` |
| `minProperties=<n>`, `exactlyOneOf=`, `atMostOneOf=`, `atLeastOneOf=<json fields>` | struct | Presence constraints over existing JSON field names |
| `pattern=`, `minLength=`, `maxLength=`, `minimum=`, `maximum=`, `minItems=` | field or named string type | The standard keyword |

An unknown marker, or a marker given a value it does not take (or missing one it needs), fails generation (`internal/tool/schemagen/source.go`).

## Adding a Policy type

The type must first be in foundation pack section 10 and in `02-configuration-model.md`.

1. Add the `PolicyType` constant in `pkg/config/v1alpha1/policy.go` with a doc comment ending in its `Planned (Mx)` tag.
2. Add the config struct to `pkg/config/v1alpha1/policy_config.go` with a `+ruralz:policyType=<type>` marker. Also mark it and its nested structs `+ruralz:open` unless `02-configuration-model.md` registers every `config` field its feature document authors; only fully registered configs are closed (`policy_config.go:6-10`, OQ-configuration-model-19, which already tracks the unregistered fields).
3. Append the struct to `policyConfigTypes()` in `internal/tool/schemagen/kinds.go`. The enum, the markers and this table must match one to one.
4. Update the hard-coded Policy type count in both the check and its message in `TestKeywords` (`internal/tool/schemagen/schemagen_test.go:181-182`), and the count in `AGENTS.md` ("What exists today"). `TestKeywords` also pins specific output, such as `RateLimitConfig` open and `AuthMTLSConfig` closed; update those expectations only for a deliberate change.
5. Regenerate and test as above.

A new kind would also need a `Kind` constant in `pkg/config/v1alpha1/meta.go` (schemagen rejects a resource type that is not one), an entry in `resourceTypes()` in `kinds.go`, and the 10-kind list in `api/schema/schema_test.go:29`.

## Known open questions

- OQ-configuration-model-20: pattern or enum constrained strings (Duration, `failureMode`, `tls.minVersion`) get no `${VAR}` alternative in the authoring view.
- The docs say schemagen reads comments with `go/doc`; the code uses `go/parser` and `go/ast`. Trust the code.
