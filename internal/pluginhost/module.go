// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Required exports of Plugin ABI v1 (docs/architecture/05-wasm-plugin-
// system.md "Contract").
const (
	exportABIVersion = "rz_abi_version"
	exportAlloc      = "rz_alloc"
	exportFree       = "rz_free"
	exportConfigure  = "rz_configure"
)

// phaseExport returns the export of Phase p: rz_ and the Phase in snake
// case, such as rz_on_request_headers.
func phaseExport(p phase.Phase) string {
	s := p.String()
	var b strings.Builder
	b.WriteString("rz_")
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			b.WriteByte('_')
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// artifact is a binary the structure check and the ABI check accepted for
// one Plugin.spec: its structure and the Capabilities its imports request.
type artifact struct {
	info *moduleInfo
	// requested are the Capabilities its imports need.
	requested capSet
	// phases are its Phase exports.
	phases phase.Set
}

// checkArtifact checks bin against spec before any compile
// (docs/architecture/05-wasm-plugin-system.md "Default limits", Artifact
// size and structure; "Contract"): the size and structure limits
// (RZ-CFG-048); a binary the reader cannot read (RZ-CFG-047, as the compile
// would refuse it); the Plugin.spec it must agree with (RZ-CFG-028): one
// defined memory whose declared minimum and maximum fit limits.memoryBytes
// in pages, the four required exports and Phase exports equal to
// spec.phases with their signatures, and imports the host implements with
// their signatures; then the Capabilities those imports request, which
// must be a subset of spec.capabilities (RZ-CFG-050).
func checkArtifact(bin []byte, spec *v1alpha1.PluginSpec, pages uint32) (*artifact, error) {
	info, err := readModule(bin)
	if err != nil {
		var se *structureError
		if errors.As(err, &se) {
			return nil, fail(CodeStructure, "Plugin artifact: %s", se.msg)
		}
		return nil, fail(CodeCompile, "Plugin artifact does not compile: %s", err.Error())
	}
	if spec.ABI != v1alpha1.PluginABIV1 {
		return nil, fail(CodeArtifact, "Plugin abi %q is not %s", spec.ABI, abiModule)
	}
	if err := checkMemory(info, pages); err != nil {
		return nil, err
	}
	if err := checkRequiredExports(info); err != nil {
		return nil, err
	}
	phases, err := checkPhaseExports(info, spec.Phases)
	if err != nil {
		return nil, err
	}
	requested, err := checkImports(info)
	if err != nil {
		return nil, err
	}
	if excess := requested.minus(parseCapabilities(spec.Capabilities)); excess != 0 {
		return nil, fail(CodeCapability, "the Plugin artifact's imports request Capabilities its spec.capabilities does not grant: %s", excess)
	}
	return &artifact{info: info, requested: requested, phases: phases}, nil
}

// checkMemory requires one defined memory whose declared limits fit pages:
// under WithMemoryCapacityFromMax wazero refuses a declared minimum or
// maximum above the page limit (the wazero study).
func checkMemory(info *moduleInfo, pages uint32) error {
	if info.memoryImports > 0 {
		return fail(CodeArtifact, "the Plugin artifact imports its memory; Plugin ABI v1 guests define it")
	}
	if len(info.memories) != 1 {
		return fail(CodeArtifact, "the Plugin artifact defines %d memories, want 1", len(info.memories))
	}
	m := info.memories[0]
	if m.shared {
		return fail(CodeArtifact, "the Plugin artifact's memory is shared; Plugin ABI v1 has no threads")
	}
	if m.min > uint64(pages) {
		return fail(CodeArtifact, "the Plugin artifact declares a memory minimum of %d pages, above limits.memoryBytes (%d pages)", m.min, pages)
	}
	if m.hasMax && m.max > uint64(pages) {
		return fail(CodeArtifact, "the Plugin artifact declares a memory maximum of %d pages, above limits.memoryBytes (%d pages)", m.max, pages)
	}
	return nil
}

// checkRequiredExports requires rz_abi_version() -> i32, rz_alloc(i32) ->
// i32, rz_free(i32) and rz_configure(i32, i32) -> i32.
func checkRequiredExports(info *moduleInfo) error {
	want := []struct {
		name            string
		params, results []byte
	}{
		{exportABIVersion, nil, []byte{valI32}},
		{exportAlloc, []byte{valI32}, []byte{valI32}},
		{exportFree, []byte{valI32}, nil},
		{exportConfigure, []byte{valI32, valI32}, []byte{valI32}},
	}
	for _, w := range want {
		t, ok := info.exportedFunc(w.name)
		if !ok {
			return fail(CodeArtifact, "the Plugin artifact does not export %s", w.name)
		}
		if !t.equal(w.params, w.results) {
			return fail(CodeArtifact, "the Plugin artifact's %s has the wrong signature", w.name)
		}
	}
	return nil
}

// checkPhaseExports returns the Phases the binary exports, each rz_<phase>
// () -> i32, which must equal spec.phases.
func checkPhaseExports(info *moduleInfo, declared []v1alpha1.Phase) (phase.Set, error) {
	var want, have phase.Set
	for _, v := range declared {
		if p, ok := phase.Parse(v); ok {
			want = want.Add(p)
		}
	}
	for p := phase.OnRequestHeaders; p < phase.Count; p++ {
		t, ok := info.exportedFunc(phaseExport(p))
		if !ok {
			continue
		}
		if !t.equal(nil, []byte{valI32}) {
			return 0, fail(CodeArtifact, "the Plugin artifact's %s has the wrong signature", phaseExport(p))
		}
		have = have.Add(p)
	}
	if have != want {
		return 0, fail(CodeArtifact, "the Plugin artifact exports the Phases [%s], and spec.phases lists [%s]", phaseNames(have), phaseNames(want))
	}
	return have, nil
}

// phaseNames lists a set's Phases, comma-separated.
func phaseNames(s phase.Set) string {
	names := make([]string, 0, phase.Count)
	for _, p := range s.Phases() {
		names = append(names, p.String())
	}
	return strings.Join(names, ", ")
}

// checkImports requires every import to be a Host Function or WASI shim
// function the host implements, with its signature, and returns the
// Capabilities they request.
func checkImports(info *moduleInfo) (capSet, error) {
	var caps capSet
	var unknown []string
	for _, im := range info.imports {
		if im.kind != externFunc {
			unknown = append(unknown, im.module+"."+im.name)
			continue
		}
		f, ok := lookupImport(im.module, im.name)
		if !ok {
			unknown = append(unknown, im.module+"."+im.name)
			continue
		}
		want := f.signature()
		if !im.typ.equal(want.params, want.results) {
			return 0, fail(CodeArtifact, "the Plugin artifact imports %s.%s with the wrong signature", im.module, im.name)
		}
		if f.capability != capNone {
			caps = caps.add(f.capability)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		unknown = slices.Compact(unknown)
		return 0, fail(CodeArtifact, "the Plugin artifact imports what the host does not implement: %s", strings.Join(clipList(unknown, 8), ", "))
	}
	return caps, nil
}

// clipList keeps the first n entries and counts the rest.
func clipList(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], fmt.Sprintf("and %d more", len(s)-n))
}
