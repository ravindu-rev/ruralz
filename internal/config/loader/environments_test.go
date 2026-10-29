// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
)

// environment returns an Environment document with a spec body.
func environment(name, spec string) string {
	return "apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: " + name + "\nspec:" + spec + "\n"
}

// readEnvs reads src as an environments file.
func readEnvs(t testing.TB, l *Loader, src string) *Environments {
	t.Helper()
	e, err := l.ReadEnvironments(t.Context(), Source{FS: fstest.MapFS{"control/envs.yaml": {Data: []byte(src)}}, Path: "control/envs.yaml"})
	if err != nil {
		t.Fatalf("ReadEnvironments: %v", err)
	}
	return e
}

// TestReadEnvironments covers what a CLI render reads of an Environment:
// spec.overlay (default: its name, spec.overlay then not explicit) and
// spec.variables; the example's control/environments.yaml shape.
func TestReadEnvironments(t *testing.T) {
	e := readEnvs(t, newLoader(t, Options{}), docs(
		environment("staging", "\n  variables:\n    OTEL_EXPORTER_OTLP_ENDPOINT: \"https://otel-collector.observability:4317\""),
		environment("prod", "\n  overlay: production\n  promotion:\n    from: staging\n    requireApproval: true\n  variables:\n    A: \"1\"\n    B: \"\""),
	))
	wantClean(t, "environments", e.Diagnostics())
	if got := e.Names(); !slices.Equal(got, []string{"prod", "staging"}) {
		t.Errorf("Names = %q", got)
	}
	prod, ds := e.Select("prod")
	if prod == nil || len(ds) > 0 {
		t.Fatalf("Select(prod) = %v, %q", prod, texts(ds))
	}
	if prod.Overlay != "production" || !prod.OverlayExplicit || prod.From != "staging" ||
		!maps.Equal(prod.Variables, subst.Map{"A": "1", "B": ""}) {
		t.Errorf("prod = %+v", prod)
	}
	if prod.OverlayLocation != (diag.Location{File: "control/envs.yaml", Line: 14, Column: 12}) ||
		prod.Location != (diag.Location{File: "control/envs.yaml", Line: 12, Column: 9}) {
		t.Errorf("prod locations = %+v, %+v", prod.OverlayLocation, prod.Location)
	}
	staging, _ := e.Select("staging")
	if staging.Overlay != "staging" || staging.OverlayExplicit || staging.From != "" ||
		staging.Variables["OTEL_EXPORTER_OTLP_ENDPOINT"] != "https://otel-collector.observability:4317" {
		t.Errorf("staging = %+v", staging)
	}
	if e.Path() != "control/envs.yaml" {
		t.Errorf("Path = %q", e.Path())
	}
}

// TestEnvironmentsKinds covers the kinds of an environments file: a Cluster
// is skipped, any other kind is RZ-CFG-007, and the envelope rules of
// stage C apply.
func TestEnvironmentsKinds(t *testing.T) {
	e := readEnvs(t, newLoader(t, Options{}), docs(
		environment("prod", " {}"),
		"apiVersion: ruralz/v1alpha1\nkind: Cluster\nmetadata:\n  name: eu\nspec:\n  environment: prod\n",
		upstream("orders"),
		"apiVersion: ruralz/v2\nkind: Environment\nmetadata:\n  name: qa\nspec: {}\n",
	))
	wantTexts(t, "kinds", e.Diagnostics(),
		`control/envs.yaml:15:7 error RZ-CFG-007 Upstream/orders kind: kind "Upstream" does not belong in an environments file, which holds Environment resources`,
		`control/envs.yaml:23:13 error RZ-CFG-007 Environment/qa apiVersion: apiVersion "ruralz/v2" is not served by this release (served apiVersions: ruralz/v1alpha1)`,
	)
	if got := e.Names(); !slices.Equal(got, []string{"prod"}) {
		t.Errorf("Names = %q", got)
	}
	if env, ds := e.Select("qa"); env != nil || len(ds) > 0 {
		t.Errorf("Select(qa), declared but failing: %v, %q; want nil and no diagnostic", env, texts(ds))
	}
}

// TestEnvironmentsDuplicate covers RZ-CFG-008: one Environment name twice;
// the first is kept.
func TestEnvironmentsDuplicate(t *testing.T) {
	e := readEnvs(t, newLoader(t, Options{}), docs(environment("prod", " {}"), environment("prod", "\n  overlay: other")))
	wantTexts(t, "duplicate", e.Diagnostics(),
		"control/envs.yaml:10:9 error RZ-CFG-008 Environment/prod metadata.name: Environment/prod is declared twice (first declared in control/envs.yaml:4:9)")
	if env, _ := e.Select("prod"); env == nil || env.Overlay != "prod" {
		t.Errorf("Select(prod) = %+v, want the first declaration", env)
	}
}

// TestEnvironmentsSchema covers stage F on Environments: a value of the
// wrong shape or an unknown field fails the Environment, which Select then
// returns as nil without a diagnostic of its own.
func TestEnvironmentsSchema(t *testing.T) {
	e := readEnvs(t, newLoader(t, Options{}), docs(
		environment("prod", "\n  variables: [a, b]"),
		environment("qa", "\n  overlays: qa"),
		environment("Bad_Name", " {}"),
	))
	got := e.Diagnostics()
	want := []string{"RZ-CFG-005", "RZ-CFG-006", "RZ-CFG-005"}
	if !slices.Equal(codes(got), want) {
		t.Fatalf("diagnostics %q, want codes %q", texts(got), want)
	}
	if len(e.Names()) != 0 {
		t.Errorf("Names = %q, want none", e.Names())
	}
	for _, name := range []string{"prod", "qa", "Bad_Name"} {
		if env, ds := e.Select(name); env != nil || len(ds) > 0 {
			t.Errorf("Select(%s) = %v, %q", name, env, texts(ds))
		}
	}
}

// TestPromotionCycles covers RZ-CFG-022: every Environment on a cycle is
// reported at its spec.promotion.from; a chain that leads into a cycle,
// or ends at an Environment the file lacks, is not a cycle.
func TestPromotionCycles(t *testing.T) {
	from := func(name, src string) string {
		return environment(name, "\n  promotion:\n    from: "+src)
	}
	e := readEnvs(t, newLoader(t, Options{}), docs(
		from("self", "self"),
		from("a", "b"), from("b", "a"),
		from("into", "a"),
		from("x", "y"), from("y", "z"), from("z", "x"),
		from("dangling", "nowhere"),
		environment("root", " {}"), from("child", "root"),
	))
	got := texts(e.Diagnostics())
	want := []string{
		"control/envs.yaml:7:11 error RZ-CFG-022 Environment/self spec.promotion.from: promotion cycle: self -> self (promotion.from chains must end at an Environment without a promotion)",
		"control/envs.yaml:15:11 error RZ-CFG-022 Environment/a spec.promotion.from: promotion cycle: a -> b -> a (promotion.from chains must end at an Environment without a promotion)",
		"control/envs.yaml:23:11 error RZ-CFG-022 Environment/b spec.promotion.from: promotion cycle: b -> a -> b (promotion.from chains must end at an Environment without a promotion)",
		"control/envs.yaml:39:11 error RZ-CFG-022 Environment/x spec.promotion.from: promotion cycle: x -> y -> z -> x (promotion.from chains must end at an Environment without a promotion)",
		"control/envs.yaml:47:11 error RZ-CFG-022 Environment/y spec.promotion.from: promotion cycle: y -> z -> x -> y (promotion.from chains must end at an Environment without a promotion)",
		"control/envs.yaml:55:11 error RZ-CFG-022 Environment/z spec.promotion.from: promotion cycle: z -> x -> y -> z (promotion.from chains must end at an Environment without a promotion)",
	}
	if !slices.Equal(got, want) {
		t.Errorf("cycles:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if env, _ := e.Select("into"); env == nil {
		t.Error("an Environment leading into a cycle is not usable")
	}
}

// TestLongPromotionCycle covers a cycle longer than the message shows: at
// most four names, then "...", and the cycle's length.
func TestLongPromotionCycle(t *testing.T) {
	var ds []string
	for i := range 6 {
		ds = append(ds, environment("e"+strconv.Itoa(i), "\n  promotion:\n    from: e"+strconv.Itoa((i+1)%6)))
	}
	e := readEnvs(t, newLoader(t, Options{}), docs(ds...))
	got := e.Diagnostics()
	if len(got) != 6 || got[0].Message != "promotion cycle: e0 -> e1 -> e2 -> e3 -> ... -> e0 (6 Environments)" {
		t.Errorf("diagnostics = %q", texts(got))
	}
}

// TestSelectMissing covers --env naming an Environment the file does not
// declare: RZ-CFG-009 at the file, with the nearest name as the hint.
func TestSelectMissing(t *testing.T) {
	e := readEnvs(t, newLoader(t, Options{}), docs(environment("prod", " {}"), environment("staging", " {}")))
	env, ds := e.Select("prd")
	if env != nil {
		t.Fatalf("Select(prd) = %+v", env)
	}
	wantTexts(t, "missing", ds,
		`control/envs.yaml error RZ-CFG-009 Environment/prd: Environment "prd" is not declared in control/envs.yaml (did you mean "prod"?)`)
	_, ds = e.Select("zzzzzzzz")
	if len(ds) != 1 || ds[0].Hint != "" {
		t.Errorf("far name: %q", texts(ds))
	}
}

// TestSelectAfterParseError covers a file that lost documents to a parse
// error: a missing name may be among them, so Select reports nothing more.
func TestSelectAfterParseError(t *testing.T) {
	e := readEnvs(t, newLoader(t, Options{}), docs(environment("prod", " {}"), "apiVersion: [\n"))
	if env, ds := e.Select("staging"); env != nil || len(ds) > 0 {
		t.Errorf("Select(staging) = %v, %q", env, texts(ds))
	}
	if !e.Diagnostics().HasErrors() {
		t.Error("the parse error is not reported")
	}
}

// TestEnvironmentsLimits covers the environments file's own limits: its
// source budget and the resource limit, each RZ-CFG-001.
func TestEnvironmentsLimits(t *testing.T) {
	l := newLoader(t, Options{Limits: Limits{MaxSourceBytes: 100, MaxResources: 2}})
	e := readEnvs(t, l, strings.Repeat("#", 101))
	wantTexts(t, "bytes", e.Diagnostics(), "control/envs.yaml:1:101 error RZ-CFG-001: the source text passes the limit of 100 bytes")
	l = newLoader(t, Options{Limits: Limits{MaxResources: 2}})
	e = readEnvs(t, l, docs(environment("a", " {}"), environment("b", " {}"), environment("c", " {}")))
	wantTexts(t, "resources", e.Diagnostics(), "control/envs.yaml:12:1 error RZ-CFG-001: the environments file holds more than 2 Environments")
	if len(e.Names()) != 0 {
		t.Errorf("Names = %q after the limit stopped the read", e.Names())
	}
}

// TestEnvironmentsSourceErrors covers an environments file that cannot be
// read: ErrSource.
func TestEnvironmentsSourceErrors(t *testing.T) {
	l := newLoader(t, Options{})
	for name, src := range map[string]Source{
		"missing":   {FS: fstest.MapFS{}, Path: "envs.yaml"},
		"directory": {FS: fstest.MapFS{"d/x": {}}, Path: "d"},
	} {
		if _, err := l.ReadEnvironments(t.Context(), src); !errors.Is(err, ErrSource) {
			t.Errorf("%s: error %v, want ErrSource", name, err)
		}
	}
}

// TestEnvironmentsLFS covers an environments file that is a Git LFS
// pointer: RZ-CFG-001.
func TestEnvironmentsLFS(t *testing.T) {
	e := readEnvs(t, newLoader(t, Options{}), "version https://git-lfs.github.com/spec/v1\n")
	if got := codes(e.Diagnostics()); !slices.Equal(got, []string{CodeLimit}) {
		t.Errorf("diagnostics = %q", texts(e.Diagnostics()))
	}
}
