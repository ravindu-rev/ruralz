---
description: Change the ruralz/v1alpha1 configuration types in pkg/config/v1alpha1 (add or change a field, enum value or Policy type) and regenerate the committed JSON Schema so schemagen, TestCommittedSchemaIsCurrent and make generate pass.
when_to_use: Editing pkg/config/v1alpha1, adding a Policy type config, changing +ruralz markers, or when the generated schema is stale.
argument-hint: <field, enum or Policy type change>
---

# Change the configuration schema: $ARGUMENTS

Read `.claude/rules/config-schema.md` first; it has the marker table and the enforced field rules.

1. **Confirm the design.** Find the change in the Kind catalog of `docs/architecture/02-configuration-model.md` (and, for a Policy type, foundation pack section 10). If it is not there, do not invent it: add an `OQ-configuration-model-<n>` row to that document's Open questions, tell the user, and stop. Do not change an existing default or add `+ruralz:since` without asking (schema levels).
2. **Edit the types** in `pkg/config/v1alpha1`, translating the sketch literally:
   - Doc comment first (it becomes the schema `description`), then marker lines, then the field with a camelCase `json` tag.
   - `# required` becomes `+ruralz:required` with no `omitempty`; every other field gets `omitempty`.
   - `default X` becomes a pointer field plus `+ruralz:default=X` (quote strings, as in `+ruralz:default="1.3"`); optional bools and numbers are pointers too.
   - Lists: `map keyed by k` becomes `+ruralz:list=map,key=k`, `orderedMap keyed by k` becomes `+ruralz:list=orderedMap,key=k`, `set` becomes `+ruralz:list=set`, anything else `+ruralz:list=atomic`.
   - A `{secretRef: ...}` value is a `SecretValue` field with `+ruralz:secret`; a field naming another resource gets `+ruralz:ref=<Kind>`.
   - Use the package scalar types (`Duration`, `ByteSize`, `Decimal`, `IntOrString`) and standard library imports only.
3. **Policy type only**:
   - Add the `PolicyType` constant in `policy.go` with a `Planned (Mx)` doc comment. Add `//nolint:gosec // A Policy type name, not a credential.` only if gosec (G101) reports the new constant, as it does for `PolicyTypeAuthAPIKey` (`policy.go:16`); nolintlint rejects an unused directive.
   - Add the config struct to `policy_config.go` with `+ruralz:policyType=<type>`, and `+ruralz:open` on it and its nested structs unless every feature field is registered (see the rule file).
   - Append it to `policyConfigTypes()` in `internal/tool/schemagen/kinds.go`.
   - Update the Policy type count in `TestKeywords` (`internal/tool/schemagen/schemagen_test.go`, the check and its message) and in `AGENTS.md`.
4. **Regenerate**: `go generate ./pkg/config/v1alpha1`. Never edit `api/schema/ruralz/v1alpha1/*.schema.json` by hand.
5. **Test**: `go test ./internal/tool/schemagen/ ./api/schema/ ./pkg/...`, `go vet ./pkg/... ./internal/tool/schemagen/` and `go run ./internal/tool/repocheck`, then `/preflight`. Fix any schemagen rule error at the Go type, not in the JSON.
6. **Review the diff** of both schema files: authoring and rendered views should differ only where `${VAR}` substitution applies. Stage the types and the JSON together (`make generate` compares against the index).
7. **Report** what changed, the doc section it implements, and a suggested title such as `feat(config): add <field> to <Kind>`. `pkg/` and `api/` changes need two approvals; mention it. If `go` is not installed, say that generation and tests were not run and the committed schema is therefore stale.
