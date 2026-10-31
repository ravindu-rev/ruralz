---
paths:
  - "pkg/config/**"
  - "api/schema/**"
  - "internal/tool/schemagen/**"
---

# Configuration types and the generated JSON Schema

Authority: `docs/architecture/02-configuration-model.md` owns kinds, fields, Policy types and `RZ-CFG` codes; the layout document's "Schema generation from Go types" section owns the markers.

## Ground rules

- The Go types in `pkg/config/v1alpha1` are the only source of `api/schema/ruralz/v1alpha1/{authoring,rendered}.schema.json` and of the served-feature table `internal/config/registry/planned_gen.go`. Never edit either. After a change run `go generate ./pkg/config/v1alpha1` and commit types, JSON and table together; `TestCommittedSchemaIsCurrent` and `TestCommittedPlannedIsCurrent` fail on stale output and `make generate` diffs against the index.
- Add or change a field only when the configuration model defines it; its Kind catalog sketches are normative for names, `# required` and list types. Otherwise ask the owner.
- `pkg/` non-test files import only the standard library and `pkg/`.
- The 10 kinds are fixed (foundation pack section 3).
- Every field added within an apiVersion carries `x-ruralz-since`, and changing a `v1alpha1` default is a new schema level: ask the owner before adding `+ruralz:since` or changing a `+ruralz:default=`.

## Translating a sketch

- Doc comment first (it becomes the schema `description`, so write user-facing text), then marker lines, then the field with a camelCase `json` tag.
- `# required` becomes `+ruralz:required` with no `omitempty`; every other field gets `omitempty`.
- `default X` becomes a pointer field plus `+ruralz:default=X` (quote strings); optional bools and numbers are pointers too.
- Lists: `map keyed by k` is `+ruralz:list=map,key=k`, `orderedMap keyed by k` is `+ruralz:list=orderedMap,key=k`, `set` is `+ruralz:list=set`, anything else `+ruralz:list=atomic`.
- A `{secretRef: ...}` value is a `SecretValue` field with `+ruralz:secret` and `+ruralz:impact` including `security`; a field naming another resource gets `+ruralz:ref=<Kind>`.
- Use the package scalar types: `Duration` (never `time.Duration`), `ByteSize`, `Decimal`, `IntOrString`, `SecretValue`; signed integers for numbers.

## Field rules schemagen enforces

- One field per line with a naming `json` tag; required only with `+ruralz:required`, everything else `omitempty`.
- Optional bools and numbers, and optional fields whose default is not the Go zero value, are pointers; `+ruralz:default=` only on a pointer field, never on a required one, and it must satisfy the field's own `pattern`, length and range keywords.
- Every slice except `json.RawMessage` has `+ruralz:list=...`. Map keys are strings. No anonymous structs, no struct types from other packages, no embedded fields outside resource types.
- `SecretValue` and `SecretRef` fields carry `+ruralz:secret`, except the `SecretRef` inside `SecretValue`.
- A typed string `const` block becomes the type's `enum`, in source order.
- `+ruralz:impact` appears once per field, each class once, in ascending order (`ai`, `metadata`, `plugin`, `routing`, `security`, `traffic`).

## Markers

| Marker | Where | Effect |
|---|---|---|
| `required` | field | Adds to the parent's `required` |
| `secret` | field | `x-ruralz-secret`; no `${VAR}` substitution |
| `ref=<Kind>` | field | `x-ruralz-ref`; value must be a Kind constant |
| `cel=<vars>:<bool\|string\|int\|dyn>` | field | `x-ruralz-cel` and `minLength: 1`: an empty expression is RZ-CFG-005; a CEL field takes no `minLength` marker of its own |
| `list=<map\|orderedMap\|set\|atomic>[,key=<field>]` | slice field | `x-ruralz-list`; `map` and `orderedMap` need a key, `set` and `atomic` must not have one |
| `impact=<classes>` | field | `x-ruralz-impact` |
| `since=<n>` | field | `x-ruralz-since`, n of 1 or more |
| `validation=<CEL rule>` | field, repeatable | Appends to `x-ruralz-validations` |
| `default=<value>` | pointer field | `default`, typed |
| `policyType=<type>` | config struct | Selects the config by `Policy.spec.type` |
| `open` | struct | Omits `additionalProperties: false` |
| `minProperties=<n>` | struct or map field | The standard keyword |
| `exactlyOneOf=`, `atMostOneOf=`, `atLeastOneOf=<json fields>` | struct | Presence constraints |
| `requiredIf=<field>,<member>=<value>` | struct | `if`/`then`: `<field>` is required when `<member>` equals `<value>`, which must be in the member's enum, or `true` or `false` for a bool member, whose `const` is then the boolean |
| `planned=<milestone>` | a kind's spec type, field or enum `const` | No schema keyword: a row of the served-feature table `internal/config/registry/planned_gen.go`, which the Node's serve check refuses with RZ-CFG-040 when present with a value other than its default; the milestone must come after `ReleaseMilestone` of `internal/config/registry`, up to `M5`. Never on a `PolicyType` or `Kind` constant: Policy types take their milestone from the Policy type registry |
| `pattern=`, `minLength=`, `maxLength=`, `minimum=`, `maximum=`, `minItems=` | field or named string type | The standard keyword |

An unknown marker, or a marker given a value it does not take, fails generation (`internal/tool/schemagen/source.go`). A constant takes no marker but `planned`.

## Adding a Policy type

The type must first be in foundation pack section 10 and in the configuration model.

1. Add the `PolicyType` constant in `pkg/config/v1alpha1/policy.go` with a doc comment ending in its `Planned (Mx)` tag; add `//nolint:gosec // A Policy type name, not a credential.` only if gosec reports it.
2. Add the config struct to `policy_config.go` with `+ruralz:policyType=<type>`, marked `+ruralz:open` (with its nested structs) unless the configuration model registers every `config` field.
3. Append the struct to `policyConfigTypes()` in `internal/tool/schemagen/kinds.go`.
4. Update the Policy type count in `TestKeywords` (`internal/tool/schemagen/schemagen_test.go`), the check and its message.
5. Regenerate.

A new kind would also need a `Kind` constant in `meta.go`, an entry in `resourceTypes()` in `kinds.go` and the kind list in `api/schema/schema_test.go`.
