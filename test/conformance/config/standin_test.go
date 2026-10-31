// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The test-only stand-ins the suite links and no binary ships, through
// which it reaches the offline codes the shipped pipeline cannot raise yet
// (docs/engineering/03-testing-and-quality-strategy.md "Conformance
// suites"): a converter registry whose lifecycle table deprecates an
// apiVersion and a field (RZ-CFG-025), and a second, test-only served
// apiVersion, whose overlay documents mismatch a ruralz/v1alpha1 base
// (RZ-CFG-030).
const (
	// standInAPIVersion is the test-only apiVersion: the ruralz/v1alpha1
	// schema under another name, converted to the hub as it stands.
	standInAPIVersion = "ruralz/v0test"
	// standInField is the deprecated field of standInAPIVersion, by its
	// schema path (convert.Lifecycle).
	standInField = "spec.timeout"
	// standInRemoval is the release the stand-in table removes both in.
	standInRemoval = "0.9.0"
)

// standInCodes returns the codes the suite reaches only through the
// stand-ins: their fixtures run on the stand-in pipelines and name only
// the in-process paths in binaries.
func standInCodes() []string { return []string{"RZ-CFG-025", "RZ-CFG-030"} }

// usesStandIns reports whether a fixture of code runs on the stand-ins.
func usesStandIns(code string) bool { return slices.Contains(standInCodes(), code) }

// standIns holds the schemas, views and converter registry of the
// stand-in pipelines.
type standIns struct {
	schemas *schemaidx.Set
	views   *schemaview.Set
	conv    *convert.Registry
}

// newStandIns builds the stand-ins: the embedded ruralz/v1alpha1 schema,
// and the same rendered view under standInAPIVersion, both indexed and
// compiled, with a registry that converts both and deprecates
// standInAPIVersion and its standInField.
func newStandIns() (*standIns, error) {
	embedded, err := schemaidx.Embedded()
	if err != nil {
		return nil, err
	}
	hubIdx, ok := embedded.Index(v1alpha1.APIVersion)
	if !ok {
		return nil, errors.New("no embedded ruralz/v1alpha1 schema")
	}
	hubViews, err := schemaview.Embedded(embedded)
	if err != nil {
		return nil, err
	}
	hubView, ok := hubViews.View(v1alpha1.APIVersion)
	if !ok {
		return nil, errors.New("no ruralz/v1alpha1 view")
	}
	from := []byte(`"const": "` + v1alpha1.APIVersion + `"`)
	rendered := schema.RenderedV1alpha1()
	if bytes.Count(rendered, from) == 0 {
		return nil, fmt.Errorf("the rendered schema holds no %s", from)
	}
	rendered = bytes.ReplaceAll(rendered, from, []byte(`"const": "`+standInAPIVersion+`"`))
	idx, err := schemaidx.Load(standInAPIVersion, rendered)
	if err != nil {
		return nil, err
	}
	view, err := schemaview.Compile(rendered, idx)
	if err != nil {
		return nil, err
	}
	si := &standIns{}
	if si.schemas, err = schemaidx.NewSet(hubIdx, idx); err != nil {
		return nil, err
	}
	if si.views, err = schemaview.NewSet(hubView, view); err != nil {
		return nil, err
	}
	lc := convert.Lifecycle{
		APIVersions: map[string]convert.Notice{standInAPIVersion: {Removal: standInRemoval, Replacement: v1alpha1.APIVersion}},
		Fields:      map[string]map[string]convert.Notice{standInAPIVersion: {standInField: {Removal: standInRemoval}}},
	}
	if si.conv, err = convert.NewRegistry(lc, convert.V1alpha1(), spoke{}); err != nil {
		return nil, err
	}
	return si, nil
}

// options returns the pipeline options of a stand-in pipeline with the
// CEL compiler c and workers.
func (si *standIns) options(c *cel.Compiler, workers int) pipeline.Options {
	return pipeline.Options{
		Checks: checks.Table(), CEL: c, Workers: workers,
		Schemas: si.schemas, Views: si.views, Converters: si.conv,
	}
}

// spoke converts the test-only apiVersion: its field set is the hub's, so
// both directions keep the tree as it is.
type spoke struct{}

func (spoke) APIVersion() string { return standInAPIVersion }

func (spoke) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) { return doc, nil }

func (spoke) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) { return doc, nil }
