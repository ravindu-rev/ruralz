// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Grafana panel types and units used by the dashboard pack.
const (
	panelTimeseries = "timeseries"
	panelStat       = "stat"

	unitReqps   = "reqps"
	unitOps     = "ops"
	unitSeconds = "s"
	unitBytes   = "bytes"
	unitPercent = "percentunit"
	unitShort   = "short"
)

// Layout of the generated dashboards: a 24-column grid, 8 rows per panel.
const (
	gridColumns  = 24
	panelHeight  = 8
	defaultWidth = 12
)

// dashboardDef is one generated Grafana dashboard.
type dashboardDef struct {
	uid, title, description string
	vars                    []queryVar
	panels                  []panelDef
}

// file is the dashboard's path under deploy/grafana.
func (d dashboardDef) file() string { return "dashboards/" + d.uid + ".json" }

// queryVar is a multi-value template variable over one label of one family
// (label_values(metric, label)).
type queryVar struct {
	name, label, metric string
}

// panelDef is one panel.
type panelDef struct {
	title, description string
	kind               string // panelTimeseries (default) or panelStat
	unit               string
	width              int       // grid columns; defaultWidth when 0
	thresholds         []float64 // drawn as lines on a timeseries, colors on a stat
	targets            []targetDef
}

// targetDef is one Prometheus query of a panel.
type targetDef struct {
	expr, legend string
	exemplar     bool
}

// ts, stat and q keep the dashboard tables short.
func ts(title, unit string, targets ...targetDef) panelDef {
	return panelDef{title: title, kind: panelTimeseries, unit: unit, targets: targets}
}

func stat(title, unit string, targets ...targetDef) panelDef {
	return panelDef{title: title, kind: panelStat, unit: unit, width: 6, targets: targets}
}

func q(expr, legend string) targetDef { return targetDef{expr: expr, legend: legend} }

// withDescription and withWidth adjust a panel.
func (p panelDef) withDescription(d string) panelDef { p.description = d; return p }

func (p panelDef) withWidth(w int) panelDef { p.width = w; return p }

// Grafana JSON model (dashboard schema 39), limited to the fields the pack
// sets. Field order is the output order.
type (
	gDashboard struct {
		Annotations   gList       `json:"annotations"`
		Description   string      `json:"description"`
		Editable      bool        `json:"editable"`
		GraphTooltip  int         `json:"graphTooltip"`
		Links         []gLink     `json:"links"`
		Panels        []gPanel    `json:"panels"`
		Refresh       string      `json:"refresh"`
		SchemaVersion int         `json:"schemaVersion"`
		Tags          []string    `json:"tags"`
		Templating    gTemplating `json:"templating"`
		Time          gTime       `json:"time"`
		Timezone      string      `json:"timezone"`
		Title         string      `json:"title"`
		UID           string      `json:"uid"`
		Version       int         `json:"version"`
	}
	gList struct {
		List []any `json:"list"`
	}
	gLink struct {
		AsDropdown  bool     `json:"asDropdown"`
		IncludeVars bool     `json:"includeVars"`
		KeepTime    bool     `json:"keepTime"`
		Tags        []string `json:"tags"`
		Title       string   `json:"title"`
		Type        string   `json:"type"`
	}
	gDatasource struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	}
	gGridPos struct {
		H int `json:"h"`
		W int `json:"w"`
		X int `json:"x"`
		Y int `json:"y"`
	}
	gPanel struct {
		ID          int          `json:"id"`
		Type        string       `json:"type"`
		Title       string       `json:"title"`
		Description string       `json:"description,omitempty"`
		Datasource  gDatasource  `json:"datasource"`
		GridPos     gGridPos     `json:"gridPos"`
		FieldConfig gFieldConfig `json:"fieldConfig"`
		Options     any          `json:"options"`
		Targets     []gTarget    `json:"targets"`
	}
	gFieldConfig struct {
		Defaults  gDefaults `json:"defaults"`
		Overrides []any     `json:"overrides"`
	}
	gDefaults struct {
		Custom     *gCustom    `json:"custom,omitempty"`
		Thresholds gThresholds `json:"thresholds"`
		Unit       string      `json:"unit"`
	}
	gCustom struct {
		ThresholdsStyle gMode `json:"thresholdsStyle"`
	}
	gMode struct {
		Mode string `json:"mode"`
	}
	gThresholds struct {
		Mode  string  `json:"mode"`
		Steps []gStep `json:"steps"`
	}
	gStep struct {
		Color string   `json:"color"`
		Value *float64 `json:"value"`
	}
	gTimeseriesOptions struct {
		Legend  gLegend  `json:"legend"`
		Tooltip gTooltip `json:"tooltip"`
	}
	gLegend struct {
		Calcs       []string `json:"calcs"`
		DisplayMode string   `json:"displayMode"`
		Placement   string   `json:"placement"`
		ShowLegend  bool     `json:"showLegend"`
	}
	gTooltip struct {
		Mode string `json:"mode"`
		Sort string `json:"sort"`
	}
	gStatOptions struct {
		ColorMode     string  `json:"colorMode"`
		GraphMode     string  `json:"graphMode"`
		JustifyMode   string  `json:"justifyMode"`
		Orientation   string  `json:"orientation"`
		ReduceOptions gReduce `json:"reduceOptions"`
		TextMode      string  `json:"textMode"`
	}
	gReduce struct {
		Calcs  []string `json:"calcs"`
		Fields string   `json:"fields"`
		Values bool     `json:"values"`
	}
	gTarget struct {
		Datasource   gDatasource `json:"datasource"`
		Exemplar     bool        `json:"exemplar"`
		Expr         string      `json:"expr"`
		LegendFormat string      `json:"legendFormat"`
		RefID        string      `json:"refId"`
	}
	gTemplating struct {
		List []gVar `json:"list"`
	}
	gVar struct {
		AllValue   string       `json:"allValue,omitempty"`
		Current    struct{}     `json:"current"`
		Datasource *gDatasource `json:"datasource,omitempty"`
		Definition string       `json:"definition,omitempty"`
		Hide       int          `json:"hide"`
		IncludeAll bool         `json:"includeAll"`
		Label      string       `json:"label"`
		Multi      bool         `json:"multi"`
		Name       string       `json:"name"`
		Options    []any        `json:"options"`
		Query      any          `json:"query"`
		Refresh    int          `json:"refresh"`
		Regex      string       `json:"regex"`
		Sort       int          `json:"sort"`
		Type       string       `json:"type"`
	}
	gVarQuery struct {
		Query string `json:"query"`
		RefID string `json:"refId"`
	}
	gTime struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
)

// datasourceRef points every query at the dashboard's data source variable.
func datasourceRef() gDatasource { return gDatasource{Type: "prometheus", UID: "${datasource}"} }

// packTag tags every dashboard of the pack; the links menu lists them.
const packTag = "ruralz"

// model builds the Grafana JSON model of d.
func (d dashboardDef) model() gDashboard {
	ds := datasourceRef()
	vars := []gVar{{
		Label: "Data source", Name: "datasource", Options: []any{}, Query: "prometheus", Refresh: 1, Type: "datasource",
	}}
	for _, v := range d.vars {
		def := fmt.Sprintf("label_values(%s, %s)", v.metric, v.name)
		vars = append(vars, gVar{
			AllValue: ".*", Datasource: &ds, Definition: def, IncludeAll: true, Label: v.label, Multi: true,
			Name: v.name, Options: []any{}, Query: gVarQuery{Query: def, RefID: "PrometheusVariableQueryEditor-VariableQuery"},
			Refresh: 2, Sort: 1, Type: "query",
		})
	}
	out := gDashboard{
		Annotations:   gList{List: []any{}},
		Description:   d.description + " Generated by telemetrygen from the Ruralz telemetry catalog; Apache-2.0.",
		Editable:      true,
		GraphTooltip:  1,
		Links:         []gLink{{AsDropdown: true, IncludeVars: false, KeepTime: true, Tags: []string{packTag}, Title: "Ruralz dashboards", Type: "dashboards"}},
		Refresh:       "1m",
		SchemaVersion: 39,
		Tags:          []string{packTag},
		Templating:    gTemplating{List: vars},
		Time:          gTime{From: "now-6h", To: "now"},
		Timezone:      "utc",
		Title:         d.title,
		UID:           d.uid,
		Version:       1,
	}
	x, y := 0, 0
	for i, p := range d.panels {
		w := p.width
		if w == 0 {
			w = defaultWidth
		}
		if x+w > gridColumns {
			x, y = 0, y+panelHeight
		}
		out.Panels = append(out.Panels, p.model(i+1, gGridPos{H: panelHeight, W: w, X: x, Y: y}))
		x += w
	}
	return out
}

// model builds the Grafana JSON model of one panel.
func (p panelDef) model(id int, pos gGridPos) gPanel {
	ds := datasourceRef()
	steps := []gStep{{Color: "green"}}
	colors := []string{"orange", "red"}
	for i, t := range p.thresholds {
		steps = append(steps, gStep{Color: colors[min(i, len(colors)-1)], Value: &t})
	}
	defaults := gDefaults{Thresholds: gThresholds{Mode: "absolute", Steps: steps}, Unit: p.unit}
	var options any
	if p.kind == panelStat {
		options = gStatOptions{
			ColorMode: "value", GraphMode: "none", JustifyMode: "auto", Orientation: "auto",
			ReduceOptions: gReduce{Calcs: []string{"lastNotNull"}}, TextMode: "auto",
		}
	} else {
		if len(p.thresholds) > 0 {
			defaults.Custom = &gCustom{ThresholdsStyle: gMode{Mode: "line"}}
		}
		options = gTimeseriesOptions{
			Legend:  gLegend{Calcs: []string{}, DisplayMode: "list", Placement: "bottom", ShowLegend: true},
			Tooltip: gTooltip{Mode: "multi", Sort: "desc"},
		}
	}
	panel := gPanel{
		ID: id, Type: p.kind, Title: p.title, Description: p.description, Datasource: ds, GridPos: pos,
		FieldConfig: gFieldConfig{Defaults: defaults, Overrides: []any{}}, Options: options,
	}
	for i, t := range p.targets {
		panel.Targets = append(panel.Targets, gTarget{
			Datasource: ds, Exemplar: t.exemplar, Expr: t.expr, LegendFormat: t.legend, RefID: string(rune('A' + i)),
		})
	}
	return panel
}

// renderDashboard encodes d as indented JSON without HTML escaping, so
// PromQL comparisons stay readable.
func renderDashboard(d dashboardDef) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d.model()); err != nil {
		return nil, fmt.Errorf("dashboard %s: %w", d.uid, err)
	}
	return b.Bytes(), nil
}

// checkDashboardDefs returns the problems of the generated dashboards:
// every panel and variable query passes checkExpr, UIDs and titles are
// unique, and every panel has a title, a unit and at least one query.
func checkDashboardDefs(defs []dashboardDef) []string {
	var out []string
	var uids, titles []string
	for _, d := range defs {
		if slices.Contains(uids, d.uid) || slices.Contains(titles, d.title) {
			out = append(out, fmt.Sprintf("dashboard %s: duplicate uid or title", d.uid))
		}
		uids, titles = append(uids, d.uid), append(titles, d.title)
		for _, v := range d.vars {
			for _, p := range checkExpr(fmt.Sprintf("label_values(%s, %s)", v.metric, v.name)) {
				out = append(out, fmt.Sprintf("dashboard %s: variable %s: %s", d.uid, v.name, p))
			}
		}
		for _, p := range d.panels {
			where := fmt.Sprintf("dashboard %s: panel %q", d.uid, p.title)
			if p.title == "" || p.unit == "" || len(p.targets) == 0 {
				out = append(out, where+": needs a title, a unit and a query")
			}
			for _, t := range p.targets {
				for _, prob := range checkExpr(t.expr) {
					out = append(out, where+": "+prob)
				}
			}
		}
	}
	return out
}

// checkDashboardJSON returns the problems of one Grafana dashboard file,
// generated or written by hand: every Prometheus query of it passes
// checkExpr (09 req 73: CI fails on a panel expression using a metric not
// in the catalog).
func checkDashboardJSON(name string, data []byte) []string {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return []string{fmt.Sprintf("%s: not JSON: %v", name, err)}
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return []string{name + ": not a dashboard object"}
	}
	var out []string
	if t, _ := root["title"].(string); t == "" {
		out = append(out, name+": dashboard has no title")
	}
	for _, q := range promQueries(root) {
		for _, p := range checkExpr(q.expr) {
			out = append(out, fmt.Sprintf("%s: %s: %s", name, q.where, p))
		}
	}
	return out
}

// dashQuery is one PromQL query of a dashboard and where it sits.
type dashQuery struct {
	where, expr string
}

// dsClass is what a data source reference of a dashboard resolves to.
type dsClass int

const (
	// dsDefault: no reference, or Grafana's Mixed data source. A target
	// falls back to its panel's data source, and from there to Grafana's
	// default data source, which the file does not name; such queries are
	// checked, so a query cannot leave the catalog gate by omitting its
	// data source.
	dsDefault dsClass = iota
	dsPrometheus
	dsOther
)

// promQueries returns the Prometheus queries of a Grafana dashboard model
// in document order: the query of every template variable of type "query",
// the expr of every annotation, and the expr of every panel target, nested
// and legacy row panels included. Queries whose data source resolves to
// anything but Prometheus (Loki, a test data source) are left out, and so
// are custom, interval, textbox, constant, ad hoc and data source variables,
// which hold no PromQL.
func promQueries(root map[string]any) []dashQuery {
	prom := promDatasourceVars(root)
	checked := func(refs ...any) bool {
		for _, ref := range refs {
			switch classifyDatasource(ref, prom) {
			case dsPrometheus:
				return true
			case dsOther:
				return false
			case dsDefault:
			}
		}
		return true
	}
	var out []dashQuery
	for _, v := range listOf(mapOf(root["templating"])["list"]) {
		vm := mapOf(v)
		if typ, _ := vm["type"].(string); typ != "query" || !checked(vm["datasource"]) {
			continue
		}
		name, _ := vm["name"].(string)
		query := vm["query"]
		if qm, ok := query.(map[string]any); ok {
			query = qm["query"]
		}
		expr, _ := query.(string)
		if expr == "" {
			expr, _ = vm["definition"].(string)
		}
		if expr != "" {
			out = append(out, dashQuery{where: "$" + name, expr: expr})
		}
	}
	for _, a := range listOf(mapOf(root["annotations"])["list"]) {
		am := mapOf(a)
		name, _ := am["name"].(string)
		if expr, _ := am["expr"].(string); expr != "" && checked(am["datasource"]) {
			out = append(out, dashQuery{where: "annotation " + name, expr: expr})
		}
	}
	var walk func(panels []any, parent string)
	walk = func(panels []any, parent string) {
		for _, p := range panels {
			pm := mapOf(p)
			title, _ := pm["title"].(string)
			where := joinWhere(parent, title)
			for _, t := range listOf(pm["targets"]) {
				tm := mapOf(t)
				expr, _ := tm["expr"].(string)
				if expr == "" || !checked(tm["datasource"], pm["datasource"]) {
					continue
				}
				ref, _ := tm["refId"].(string)
				out = append(out, dashQuery{where: joinWhere(where, ref), expr: expr})
			}
			walk(listOf(pm["panels"]), where)
		}
	}
	walk(listOf(root["panels"]), "")
	for _, r := range listOf(root["rows"]) { // schema versions before 16
		title, _ := mapOf(r)["title"].(string)
		walk(listOf(mapOf(r)["panels"]), title)
	}
	return out
}

// joinWhere appends one step to a location such as "Row > Panel > A".
func joinWhere(parent, step string) string {
	switch {
	case step == "":
		return parent
	case parent == "":
		return step
	}
	return parent + " > " + step
}

// promDatasourceVars returns the names of the dashboard's Prometheus data
// source variables (type "datasource", query "prometheus") and of the
// Prometheus data source inputs of an exported dashboard (__inputs).
func promDatasourceVars(root map[string]any) []string {
	var out []string
	for _, v := range listOf(mapOf(root["templating"])["list"]) {
		vm := mapOf(v)
		typ, _ := vm["type"].(string)
		query, _ := vm["query"].(string)
		if name, _ := vm["name"].(string); typ == "datasource" && query == "prometheus" {
			out = append(out, name)
		}
	}
	for _, in := range listOf(root["__inputs"]) {
		im := mapOf(in)
		typ, _ := im["type"].(string)
		plugin, _ := im["pluginId"].(string)
		if name, _ := im["name"].(string); typ == "datasource" && plugin == "prometheus" {
			out = append(out, name)
		}
	}
	return out
}

// classifyDatasource resolves a data source reference: an object with a
// plugin type and a UID, or a legacy name string; either may be a variable
// such as ${datasource}.
func classifyDatasource(ref any, promVars []string) dsClass {
	var typ, id string
	switch r := ref.(type) {
	case nil:
		return dsDefault
	case string:
		id = r
	case map[string]any:
		typ, _ = r["type"].(string)
		id, _ = r["uid"].(string)
	default:
		return dsOther
	}
	switch {
	case typ == "prometheus":
		return dsPrometheus
	case id == "-- Mixed --" || (typ == "" && id == ""):
		return dsDefault
	case typ != "":
		return dsOther
	}
	if name, ok := variableName(id); ok {
		if slices.Contains(promVars, name) {
			return dsPrometheus
		}
		return dsOther
	}
	if strings.Contains(strings.ToLower(id), "prometheus") {
		return dsPrometheus // a legacy data source name such as "Prometheus"
	}
	return dsOther
}

// variableName returns the name of a Grafana variable reference ($name,
// ${name}, ${name:format} or [[name]]).
func variableName(ref string) (string, bool) {
	switch {
	case strings.HasPrefix(ref, "${") && strings.HasSuffix(ref, "}"):
		name, _, _ := strings.Cut(ref[2:len(ref)-1], ":")
		return name, true
	case strings.HasPrefix(ref, "[[") && strings.HasSuffix(ref, "]]"):
		name, _, _ := strings.Cut(ref[2:len(ref)-2], ":")
		return name, true
	case strings.HasPrefix(ref, "$"):
		return ref[1:], true
	}
	return "", false
}

// mapOf and listOf read a JSON object or array, nil for anything else.
func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func listOf(v any) []any {
	l, _ := v.([]any)
	return l
}
