// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reposettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The repository and its owner, as the script names them.
const (
	repoName   = "ravindu-rev/ruralz"
	repoPath   = "/repos/" + repoName
	ownerLogin = "ravindu-rev"
	ownerID    = 4242
	// otherLogin is a second user, a reviewer the table does not name.
	otherLogin = "another-maintainer"
	otherID    = 5150
)

// apiHost is the REST API host of GH_HOST=github.localhost, which gh
// reaches over plain HTTP, here through the fake as its HTTP proxy.
const apiHost = "api.github.localhost"

// The GitHub Actions app and the repository role admin, as the settings
// table names them.
const (
	actionsApp = 15368
	adminRole  = 5
)

// ghState is the fake repository's settings, in the shape of GitHub's
// REST API (docs.github.com/en/rest).
type ghState struct {
	OwnerType string `json:"ownerType"`

	AllowSquashMerge         bool   `json:"allowSquashMerge"`
	AllowMergeCommit         bool   `json:"allowMergeCommit"`
	AllowRebaseMerge         bool   `json:"allowRebaseMerge"`
	SquashMergeCommitTitle   string `json:"squashMergeCommitTitle"`
	SquashMergeCommitMessage string `json:"squashMergeCommitMessage"`
	DeleteBranchOnMerge      bool   `json:"deleteBranchOnMerge"`
	SecretScanning           string `json:"secretScanning"`
	PushProtection           string `json:"pushProtection"`

	ActionsEnabled             bool   `json:"actionsEnabled"`
	AllowedActions             string `json:"allowedActions"`
	SHAPinningRequired         bool   `json:"shaPinningRequired"`
	DefaultWorkflowPermissions string `json:"defaultWorkflowPermissions"`
	CanApprovePullRequests     bool   `json:"canApprovePullRequests"`
	ApprovalPolicy             string `json:"approvalPolicy"`

	PrivateVulnerabilityReporting bool `json:"privateVulnerabilityReporting"`
	DependabotAlerts              bool `json:"dependabotAlerts"`
	SecurityUpdates               bool `json:"securityUpdates"`
	SecurityUpdatesPaused         bool `json:"securityUpdatesPaused"`
	ImmutableReleases             bool `json:"immutableReleases"`

	Rulesets      []ruleset `json:"rulesets"`
	NextRulesetID int       `json:"nextRulesetID"`

	Environment    *environment   `json:"environment"`
	BranchPolicies []branchPolicy `json:"branchPolicies"`
	NextPolicyID   int            `json:"nextPolicyID"`
}

// ruleset is one repository ruleset.
type ruleset struct {
	ID           int           `json:"id"`
	Name         string        `json:"name"`
	Target       string        `json:"target"`
	Enforcement  string        `json:"enforcement"`
	Include      []string      `json:"include"`
	Exclude      []string      `json:"exclude"`
	BypassActors []bypassActor `json:"bypassActors"`
	Rules        []rule        `json:"rules"`
}

type bypassActor struct {
	ActorID    *int   `json:"actor_id"`
	ActorType  string `json:"actor_type"`
	BypassMode string `json:"bypass_mode"`
}

type rule struct {
	Type       string         `json:"type"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

// environment is a deployment environment; Reviewers are user IDs, and a
// nil BranchPolicy lets every branch deploy.
type environment struct {
	WaitTimer         int           `json:"waitTimer"`
	PreventSelfReview bool          `json:"preventSelfReview"`
	Reviewers         []int         `json:"reviewers"`
	BranchPolicy      *branchScheme `json:"branchPolicy"`
}

type branchScheme struct {
	ProtectedBranches    bool `json:"protected_branches"`
	CustomBranchPolicies bool `json:"custom_branch_policies"`
}

type branchPolicy struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// faults are the fake's answers that a case injects.
type faults struct {
	// fail answers "METHOD /path" with an error status and message.
	fail map[string]failure
	// ignore accepts the write "METHOD /path" without applying it.
	ignore map[string]bool
	// hideMerge, hideSecurity and hideBypass leave out of the answers the
	// fields GitHub shows only to a token that may write the repository's
	// contents, an admin's, or one that may write the ruleset.
	hideMerge, hideSecurity, hideBypass bool
}

type failure struct {
	status  int
	message string
}

// call is one request the fake answered.
type call struct {
	Method, URI, Body string
}

func (c call) write() bool { return c.Method != http.MethodGet }

// fakeGitHub is an HTTP proxy that answers the REST API of one repository
// from an in-memory ghState, records every call, and refuses any other
// host, so gh reaches nothing else.
type fakeGitHub struct {
	mu         sync.Mutex
	state      *ghState
	faults     faults
	calls      []call
	unexpected []string
}

func newFake(s *ghState, f faults) *fakeGitHub {
	return &fakeGitHub{state: s, faults: f}
}

// snapshot returns the calls and unexpected requests so far, and a copy
// of the state.
func (f *fakeGitHub) snapshot() ([]call, []string, *ghState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls), slices.Clone(f.unexpected), cloneState(f.state)
}

func cloneState(s *ghState) *ghState {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var c ghState
	if err := json.Unmarshal(data, &c); err != nil {
		panic(err)
	}
	return &c
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path, ok := apiPath(r)
	if !ok {
		f.unexpected = append(f.unexpected, r.Method+" "+r.URL.String())
		http.Error(w, "the fake GitHub answers only "+apiHost, http.StatusBadGateway)
		return
	}
	f.calls = append(f.calls, call{Method: r.Method, URI: r.URL.RequestURI(), Body: string(body)})
	key := r.Method + " " + path
	if fl, ok := f.faults.fail[key]; ok {
		writeJSON(w, fl.status, map[string]any{"message": fl.message, "status": strconv.Itoa(fl.status)})
		return
	}
	if r.Method != http.MethodGet && f.faults.ignore[key] {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status, resp := f.route(r.Method, path, r.URL.Query().Get("includes_parents"), body)
	if status == http.StatusNotFound && resp == nil {
		f.unexpected = append(f.unexpected, key)
	}
	switch {
	case status == http.StatusNoContent:
		w.WriteHeader(status)
	case resp == nil:
		writeJSON(w, status, map[string]any{"message": http.StatusText(status), "status": strconv.Itoa(status)})
	default:
		writeJSON(w, status, resp)
	}
}

// apiPath returns the API path of a proxied request to the fake's host.
func apiPath(r *http.Request) (string, bool) {
	if r.Method == http.MethodConnect {
		return "", false
	}
	switch r.URL.Host {
	case apiHost:
		return r.URL.Path, true
	case "github.localhost":
		// The enterprise form of the REST prefix.
		if p, ok := strings.CutPrefix(r.URL.Path, "/api/v3"); ok {
			return p, true
		}
	}
	return "", false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errValidation answers 422 Validation Failed.
var errValidation = errors.New("validation failed")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errValidation, fmt.Sprintf(format, args...))
}

// answer turns a write's error into GitHub's 422 answer.
func answer(err error, ok int, v any) (int, any) {
	if err != nil {
		return http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed", "errors": []string{err.Error()}}
	}
	return ok, v
}

// route answers one call; a nil answer with 404 is a call the fake does
// not model, which fails the case.
func (f *fakeGitHub) route(method, path, includesParents string, body []byte) (int, any) {
	s := f.state
	sub, ok := strings.CutPrefix(path, repoPath)
	if !ok {
		return http.StatusNotFound, nil
	}
	notFound := map[string]any{"message": "Not Found"}
	switch {
	case sub == "":
		switch method {
		case http.MethodGet:
			return http.StatusOK, f.repoJSON()
		case http.MethodPatch:
			return answer(s.patchRepo(body), http.StatusOK, f.repoJSON())
		}
	case sub == "/private-vulnerability-reporting":
		switch method {
		case http.MethodGet:
			return http.StatusOK, map[string]any{"enabled": s.PrivateVulnerabilityReporting}
		case http.MethodPut:
			s.PrivateVulnerabilityReporting = true
			return http.StatusNoContent, nil
		}
	case sub == "/vulnerability-alerts":
		switch method {
		case http.MethodGet:
			if s.DependabotAlerts {
				return http.StatusNoContent, nil
			}
			return http.StatusNotFound, notFound
		case http.MethodPut:
			s.DependabotAlerts = true
			return http.StatusNoContent, nil
		}
	case sub == "/automated-security-fixes":
		switch method {
		case http.MethodGet:
			if s.SecurityUpdates {
				return http.StatusOK, map[string]any{"enabled": true, "paused": s.SecurityUpdatesPaused}
			}
			return http.StatusNotFound, notFound
		case http.MethodPut:
			// Security updates need Dependabot alerts.
			if !s.DependabotAlerts {
				return answer(invalid("Dependabot alerts are off"), 0, nil)
			}
			s.SecurityUpdates, s.SecurityUpdatesPaused = true, false
			return http.StatusNoContent, nil
		}
	case sub == "/actions/permissions":
		switch method {
		case http.MethodGet:
			return http.StatusOK, map[string]any{
				"enabled": s.ActionsEnabled, "allowed_actions": s.AllowedActions, "sha_pinning_required": s.SHAPinningRequired,
				"selected_actions_url": "https://" + apiHost + repoPath + "/actions/permissions/selected-actions",
			}
		case http.MethodPut:
			return answer(s.putActions(body), http.StatusNoContent, nil)
		}
	case sub == "/actions/permissions/workflow":
		switch method {
		case http.MethodGet:
			return http.StatusOK, map[string]any{
				"default_workflow_permissions": s.DefaultWorkflowPermissions, "can_approve_pull_request_reviews": s.CanApprovePullRequests,
			}
		case http.MethodPut:
			return answer(s.putWorkflow(body), http.StatusNoContent, nil)
		}
	case sub == "/actions/permissions/fork-pr-contributor-approval":
		switch method {
		case http.MethodGet:
			return http.StatusOK, map[string]any{"approval_policy": s.ApprovalPolicy}
		case http.MethodPut:
			return answer(s.putApprovalPolicy(body), http.StatusNoContent, nil)
		}
	case sub == "/immutable-releases":
		switch method {
		case http.MethodGet:
			if s.ImmutableReleases {
				return http.StatusOK, map[string]any{"enabled": true, "enforced_by_owner": false}
			}
			return http.StatusNotFound, notFound
		case http.MethodPut:
			s.ImmutableReleases = true
			return http.StatusNoContent, nil
		}
	case sub == "/rulesets":
		switch method {
		case http.MethodGet:
			if includesParents != "false" {
				return answer(invalid("the script lists the repository's own rulesets, includes_parents=false"), 0, nil)
			}
			list := []any{}
			for _, rs := range s.Rulesets {
				list = append(list, rulesetSummary(rs))
			}
			return http.StatusOK, list
		case http.MethodPost:
			rs, err := s.createRuleset(body)
			if err != nil {
				return answer(err, 0, nil)
			}
			return http.StatusCreated, f.rulesetJSON(rs)
		}
	case strings.HasPrefix(sub, "/rulesets/"):
		id, err := strconv.Atoi(strings.TrimPrefix(sub, "/rulesets/"))
		i := slices.IndexFunc(s.Rulesets, func(rs ruleset) bool { return rs.ID == id })
		if err != nil || i < 0 {
			return http.StatusNotFound, notFound
		}
		switch method {
		case http.MethodGet:
			return http.StatusOK, f.rulesetJSON(s.Rulesets[i])
		case http.MethodPut:
			if err := s.updateRuleset(i, body); err != nil {
				return answer(err, 0, nil)
			}
			return http.StatusOK, f.rulesetJSON(s.Rulesets[i])
		}
	case sub == "/environments/release":
		switch method {
		case http.MethodGet:
			if s.Environment == nil {
				return http.StatusNotFound, notFound
			}
			return http.StatusOK, s.environmentJSON()
		case http.MethodPut:
			if err := s.putEnvironment(body); err != nil {
				return answer(err, 0, nil)
			}
			return http.StatusOK, s.environmentJSON()
		}
	case sub == "/environments/release/deployment-branch-policies":
		if s.Environment == nil {
			return http.StatusNotFound, notFound
		}
		switch method {
		case http.MethodGet:
			list := []any{}
			for _, p := range s.BranchPolicies {
				list = append(list, map[string]any{"id": p.ID, "node_id": "DBP_" + strconv.Itoa(p.ID), "name": p.Name, "type": p.Type})
			}
			return http.StatusOK, map[string]any{"total_count": len(list), "branch_policies": list}
		case http.MethodPost:
			p, err := s.createBranchPolicy(body)
			if err != nil {
				return answer(err, 0, nil)
			}
			return http.StatusOK, map[string]any{"id": p.ID, "node_id": "DBP_" + strconv.Itoa(p.ID), "name": p.Name, "type": p.Type}
		}
	case strings.HasPrefix(sub, "/environments/release/deployment-branch-policies/"):
		id, err := strconv.Atoi(strings.TrimPrefix(sub, "/environments/release/deployment-branch-policies/"))
		i := slices.IndexFunc(s.BranchPolicies, func(p branchPolicy) bool { return p.ID == id })
		if s.Environment == nil || err != nil || i < 0 {
			return http.StatusNotFound, notFound
		}
		if method == http.MethodDelete {
			s.BranchPolicies = slices.Delete(s.BranchPolicies, i, i+1)
			return http.StatusNoContent, nil
		}
	}
	return http.StatusNotFound, nil
}

// login returns the login of a user ID the fake knows.
func login(id int) (string, bool) {
	switch id {
	case ownerID:
		return ownerLogin, true
	case otherID:
		return otherLogin, true
	}
	return "", false
}

func (f *fakeGitHub) repoJSON() map[string]any {
	s := f.state
	m := map[string]any{
		"id": 987654321, "node_id": "R_kgDO", "name": "ruralz", "full_name": repoName,
		"owner":   map[string]any{"login": ownerLogin, "id": ownerID, "node_id": "U_kgDO", "type": s.OwnerType, "site_admin": false},
		"private": false, "visibility": "public", "default_branch": "main", "archived": false,
		"permissions": map[string]any{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
	}
	if !f.faults.hideMerge {
		m["allow_squash_merge"] = s.AllowSquashMerge
		m["allow_merge_commit"] = s.AllowMergeCommit
		m["allow_rebase_merge"] = s.AllowRebaseMerge
		m["allow_auto_merge"] = false
		m["squash_merge_commit_title"] = s.SquashMergeCommitTitle
		m["squash_merge_commit_message"] = s.SquashMergeCommitMessage
		m["merge_commit_title"] = "MERGE_MESSAGE"
		m["merge_commit_message"] = "PR_TITLE"
		m["delete_branch_on_merge"] = s.DeleteBranchOnMerge
	}
	if !f.faults.hideSecurity {
		updates := "disabled"
		if s.SecurityUpdates {
			updates = "enabled"
		}
		m["security_and_analysis"] = map[string]any{
			"secret_scanning":                       map[string]any{"status": s.SecretScanning},
			"secret_scanning_push_protection":       map[string]any{"status": s.PushProtection},
			"secret_scanning_non_provider_patterns": map[string]any{"status": "disabled"},
			"dependabot_security_updates":           map[string]any{"status": updates},
		}
	}
	return m
}

// decode decodes a write's JSON body, refusing fields the endpoint does
// not take.
func decode(body []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return invalid("body: %v", err)
	}
	return nil
}

func oneOf(field, value string, allowed ...string) error {
	if !slices.Contains(allowed, value) {
		return invalid("%s %q is not one of %v", field, value, allowed)
	}
	return nil
}

func (s *ghState) patchRepo(body []byte) error {
	type status struct {
		Status string `json:"status"`
	}
	var b struct {
		AllowSquashMerge         *bool   `json:"allow_squash_merge"`
		AllowMergeCommit         *bool   `json:"allow_merge_commit"`
		AllowRebaseMerge         *bool   `json:"allow_rebase_merge"`
		SquashMergeCommitTitle   *string `json:"squash_merge_commit_title"`
		SquashMergeCommitMessage *string `json:"squash_merge_commit_message"`
		DeleteBranchOnMerge      *bool   `json:"delete_branch_on_merge"`
		SecurityAndAnalysis      *struct {
			SecretScanning               *status `json:"secret_scanning"`
			SecretScanningPushProtection *status `json:"secret_scanning_push_protection"`
		} `json:"security_and_analysis"`
	}
	if err := decode(body, &b); err != nil {
		return err
	}
	if b.SquashMergeCommitMessage != nil && b.SquashMergeCommitTitle == nil {
		return invalid("squash_merge_commit_message needs squash_merge_commit_title")
	}
	if b.SquashMergeCommitTitle != nil {
		if err := oneOf("squash_merge_commit_title", *b.SquashMergeCommitTitle, "PR_TITLE", "COMMIT_OR_PR_TITLE"); err != nil {
			return err
		}
		s.SquashMergeCommitTitle = *b.SquashMergeCommitTitle
	}
	if b.SquashMergeCommitMessage != nil {
		if err := oneOf("squash_merge_commit_message", *b.SquashMergeCommitMessage, "PR_BODY", "COMMIT_MESSAGES", "BLANK"); err != nil {
			return err
		}
		s.SquashMergeCommitMessage = *b.SquashMergeCommitMessage
	}
	for _, kv := range []struct {
		from *bool
		to   *bool
	}{
		{b.AllowSquashMerge, &s.AllowSquashMerge},
		{b.AllowMergeCommit, &s.AllowMergeCommit},
		{b.AllowRebaseMerge, &s.AllowRebaseMerge},
		{b.DeleteBranchOnMerge, &s.DeleteBranchOnMerge},
	} {
		if kv.from != nil {
			*kv.to = *kv.from
		}
	}
	if sa := b.SecurityAndAnalysis; sa != nil {
		for _, kv := range []struct {
			from *status
			to   *string
		}{{sa.SecretScanning, &s.SecretScanning}, {sa.SecretScanningPushProtection, &s.PushProtection}} {
			if kv.from == nil {
				continue
			}
			if err := oneOf("status", kv.from.Status, "enabled", "disabled"); err != nil {
				return err
			}
			*kv.to = kv.from.Status
		}
	}
	return nil
}

func (s *ghState) putActions(body []byte) error {
	var b struct {
		Enabled            *bool   `json:"enabled"`
		AllowedActions     *string `json:"allowed_actions"`
		SHAPinningRequired *bool   `json:"sha_pinning_required"`
	}
	if err := decode(body, &b); err != nil {
		return err
	}
	if b.Enabled == nil {
		return invalid("enabled is required")
	}
	if b.AllowedActions != nil {
		if err := oneOf("allowed_actions", *b.AllowedActions, "all", "local_only", "selected"); err != nil {
			return err
		}
		s.AllowedActions = *b.AllowedActions
	}
	s.ActionsEnabled = *b.Enabled
	if b.SHAPinningRequired != nil {
		s.SHAPinningRequired = *b.SHAPinningRequired
	}
	return nil
}

func (s *ghState) putWorkflow(body []byte) error {
	var b struct {
		DefaultWorkflowPermissions *string `json:"default_workflow_permissions"`
		CanApprovePullRequests     *bool   `json:"can_approve_pull_request_reviews"`
	}
	if err := decode(body, &b); err != nil {
		return err
	}
	if b.DefaultWorkflowPermissions != nil {
		if err := oneOf("default_workflow_permissions", *b.DefaultWorkflowPermissions, "read", "write"); err != nil {
			return err
		}
		s.DefaultWorkflowPermissions = *b.DefaultWorkflowPermissions
	}
	if b.CanApprovePullRequests != nil {
		s.CanApprovePullRequests = *b.CanApprovePullRequests
	}
	return nil
}

func (s *ghState) putApprovalPolicy(body []byte) error {
	var b struct {
		ApprovalPolicy *string `json:"approval_policy"`
	}
	if err := decode(body, &b); err != nil {
		return err
	}
	if b.ApprovalPolicy == nil {
		return invalid("approval_policy is required")
	}
	if err := oneOf("approval_policy", *b.ApprovalPolicy, "first_time_contributors_new_to_github", "first_time_contributors", "all_external_contributors"); err != nil {
		return err
	}
	s.ApprovalPolicy = *b.ApprovalPolicy
	return nil
}

// rulesetBody is a ruleset create or update body.
type rulesetBody struct {
	Name        *string `json:"name"`
	Target      *string `json:"target"`
	Enforcement *string `json:"enforcement"`
	Conditions  *struct {
		RefName *struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	BypassActors *[]bypassActor `json:"bypass_actors"`
	Rules        *[]rule        `json:"rules"`
}

// apply validates b and sets its fields on rs, as POST and PUT do.
func (b rulesetBody) apply(rs *ruleset) error {
	if b.Name != nil {
		if *b.Name == "" {
			return invalid("name is empty")
		}
		rs.Name = *b.Name
	}
	if b.Target != nil {
		if err := oneOf("target", *b.Target, "branch", "tag", "push"); err != nil {
			return err
		}
		rs.Target = *b.Target
	}
	if b.Enforcement != nil {
		if err := oneOf("enforcement", *b.Enforcement, "disabled", "active", "evaluate"); err != nil {
			return err
		}
		rs.Enforcement = *b.Enforcement
	}
	if b.Conditions != nil {
		if b.Conditions.RefName == nil || b.Conditions.RefName.Include == nil || b.Conditions.RefName.Exclude == nil {
			return invalid("conditions.ref_name needs include and exclude")
		}
		rs.Include, rs.Exclude = b.Conditions.RefName.Include, b.Conditions.RefName.Exclude
	}
	if b.BypassActors != nil {
		for _, a := range *b.BypassActors {
			if err := oneOf("actor_type", a.ActorType, "Integration", "OrganizationAdmin", "RepositoryRole", "Team", "DeployKey", "User"); err != nil {
				return err
			}
			if err := oneOf("bypass_mode", a.BypassMode, "always", "pull_request", "exempt"); err != nil {
				return err
			}
			if a.ActorID == nil && a.ActorType != "OrganizationAdmin" && a.ActorType != "DeployKey" {
				return invalid("actor_id is required for %s", a.ActorType)
			}
		}
		rs.BypassActors = *b.BypassActors
	}
	if b.Rules != nil {
		for _, r := range *b.Rules {
			if err := validRule(r); err != nil {
				return err
			}
		}
		rs.Rules = *b.Rules
	}
	// GitHub keeps update_allows_fetch_and_merge, which lets a branch sync
	// its fork, only on branch rulesets: a tag ruleset's update rule reads
	// back without parameters.
	if rs.Target == "tag" {
		for i := range rs.Rules {
			if rs.Rules[i].Type == "update" {
				rs.Rules[i].Parameters = nil
			}
		}
	}
	return nil
}

// validRule checks a rule's parameters as GitHub's rulesets API takes
// them.
func validRule(r rule) error {
	p := r.Parameters
	isBool := func(k string) bool { _, ok := p[k].(bool); return ok }
	switch r.Type {
	case "creation", "deletion", "non_fast_forward", "required_linear_history", "required_signatures":
		if len(p) != 0 {
			return invalid("rule %s takes no parameters", r.Type)
		}
	case "update":
		if len(p) != 0 && (len(p) != 1 || !isBool("update_allows_fetch_and_merge")) {
			return invalid("rule update takes no parameters or update_allows_fetch_and_merge")
		}
	case "pull_request":
		for _, k := range []string{"dismiss_stale_reviews_on_push", "require_code_owner_review", "require_last_push_approval", "required_review_thread_resolution"} {
			if !isBool(k) {
				return invalid("rule pull_request needs boolean %s", k)
			}
		}
		if n, ok := p["required_approving_review_count"].(float64); !ok || n < 0 || n > 10 || n != float64(int(n)) {
			return invalid("rule pull_request needs required_approving_review_count from 0 to 10")
		}
		if v, ok := p["allowed_merge_methods"]; ok {
			methods, ok := v.([]any)
			if !ok || len(methods) == 0 {
				return invalid("allowed_merge_methods needs at least one method")
			}
			for _, m := range methods {
				if s, ok := m.(string); !ok || oneOf("merge method", s, "merge", "squash", "rebase") != nil {
					return invalid("allowed_merge_methods holds %v", m)
				}
			}
		}
		for k := range p {
			if !slices.Contains([]string{
				"dismiss_stale_reviews_on_push", "require_code_owner_review", "require_last_push_approval",
				"required_review_thread_resolution", "required_approving_review_count", "allowed_merge_methods",
				"automatic_copilot_code_review_enabled", "required_reviewers",
			}, k) {
				return invalid("rule pull_request takes no parameter %s", k)
			}
		}
	case "required_status_checks":
		if !isBool("strict_required_status_checks_policy") {
			return invalid("rule required_status_checks needs strict_required_status_checks_policy")
		}
		checks, ok := p["required_status_checks"].([]any)
		if !ok {
			return invalid("rule required_status_checks needs required_status_checks")
		}
		for _, c := range checks {
			m, ok := c.(map[string]any)
			if _, hasContext := m["context"].(string); !ok || !hasContext {
				return invalid("a required status check needs a context")
			}
			for k := range m {
				if k != "context" && k != "integration_id" {
					return invalid("a required status check takes no %s", k)
				}
			}
		}
		for k := range p {
			if !slices.Contains([]string{"strict_required_status_checks_policy", "required_status_checks", "do_not_enforce_on_create"}, k) {
				return invalid("rule required_status_checks takes no parameter %s", k)
			}
		}
	default:
		return invalid("unknown rule type %s", r.Type)
	}
	return nil
}

func (s *ghState) createRuleset(body []byte) (ruleset, error) {
	var b rulesetBody
	if err := decode(body, &b); err != nil {
		return ruleset{}, err
	}
	if b.Name == nil || b.Enforcement == nil {
		return ruleset{}, invalid("name and enforcement are required")
	}
	if slices.ContainsFunc(s.Rulesets, func(rs ruleset) bool { return rs.Name == *b.Name }) {
		return ruleset{}, invalid("name %q is taken", *b.Name)
	}
	rs := ruleset{ID: s.NextRulesetID, Target: "branch", Include: []string{}, Exclude: []string{}, BypassActors: []bypassActor{}, Rules: []rule{}}
	if err := b.apply(&rs); err != nil {
		return ruleset{}, err
	}
	s.NextRulesetID++
	s.Rulesets = append(s.Rulesets, rs)
	return rs, nil
}

func (s *ghState) updateRuleset(i int, body []byte) error {
	var b rulesetBody
	if err := decode(body, &b); err != nil {
		return err
	}
	rs := s.Rulesets[i]
	if err := b.apply(&rs); err != nil {
		return err
	}
	s.Rulesets[i] = rs
	return nil
}

func rulesetSummary(rs ruleset) map[string]any {
	return map[string]any{
		"id": rs.ID, "name": rs.Name, "target": rs.Target, "source_type": "Repository", "source": repoName,
		"enforcement": rs.Enforcement, "node_id": "RRS_" + strconv.Itoa(rs.ID),
		"_links": map[string]any{
			"self": map[string]any{"href": "https://" + apiHost + repoPath + "/rulesets/" + strconv.Itoa(rs.ID)},
			"html": map[string]any{"href": "https://github.com/" + repoName + "/rules/" + strconv.Itoa(rs.ID)},
		},
		"created_at": "2026-10-01T09:00:00Z", "updated_at": "2026-10-01T09:00:00Z",
	}
}

// rulesetJSON answers a ruleset as GitHub does, which adds parameters the
// request left out, such as required_reviewers, to a pull_request rule.
func (f *fakeGitHub) rulesetJSON(rs ruleset) map[string]any {
	m := rulesetSummary(rs)
	m["current_user_can_bypass"] = "never"
	m["conditions"] = map[string]any{"ref_name": map[string]any{"include": rs.Include, "exclude": rs.Exclude}}
	rules := []any{}
	for _, r := range rs.Rules {
		out := map[string]any{"type": r.Type}
		if r.Parameters != nil || r.Type == "pull_request" {
			p := map[string]any{}
			for k, v := range r.Parameters {
				p[k] = v
			}
			if r.Type == "pull_request" {
				for k, v := range map[string]any{"required_reviewers": []any{}, "automatic_copilot_code_review_enabled": false} {
					if _, ok := p[k]; !ok {
						p[k] = v
					}
				}
			}
			out["parameters"] = p
		}
		rules = append(rules, out)
	}
	m["rules"] = rules
	if !f.faults.hideBypass {
		m["bypass_actors"] = rs.BypassActors
	}
	return m
}

func (s *ghState) environmentJSON() map[string]any {
	e := s.Environment
	rules := []any{}
	if e.WaitTimer > 0 {
		rules = append(rules, map[string]any{"id": 3736, "node_id": "GA_wait", "type": "wait_timer", "wait_timer": e.WaitTimer})
	}
	if len(e.Reviewers) > 0 {
		reviewers := []any{}
		for _, id := range e.Reviewers {
			name, _ := login(id)
			reviewers = append(reviewers, map[string]any{
				"type": "User", "reviewer": map[string]any{"login": name, "id": id, "node_id": "U_" + strconv.Itoa(id), "type": "User"},
			})
		}
		rules = append(rules, map[string]any{
			"id": 3755, "node_id": "GA_reviewers", "type": "required_reviewers",
			"prevent_self_review": e.PreventSelfReview, "reviewers": reviewers,
		})
	}
	var scheme any
	if e.BranchPolicy != nil {
		rules = append(rules, map[string]any{"id": 3756, "node_id": "GA_branches", "type": "branch_policy"})
		scheme = map[string]any{"protected_branches": e.BranchPolicy.ProtectedBranches, "custom_branch_policies": e.BranchPolicy.CustomBranchPolicies}
	}
	return map[string]any{
		"id": 161088068, "node_id": "EN_release", "name": "release",
		"url":        "https://" + apiHost + repoPath + "/environments/release",
		"html_url":   "https://github.com/" + repoName + "/deployments/activity_log?environments_filter=release",
		"created_at": "2026-10-01T09:00:00Z", "updated_at": "2026-10-01T09:00:00Z",
		"can_admins_bypass": true, "protection_rules": rules, "deployment_branch_policy": scheme,
	}
}

func (s *ghState) putEnvironment(body []byte) error {
	var b struct {
		WaitTimer         *int  `json:"wait_timer"`
		PreventSelfReview *bool `json:"prevent_self_review"`
		Reviewers         *[]struct {
			Type string `json:"type"`
			ID   int    `json:"id"`
		} `json:"reviewers"`
		DeploymentBranchPolicy json.RawMessage `json:"deployment_branch_policy"`
	}
	if err := decode(body, &b); err != nil {
		return err
	}
	e := environment{}
	if s.Environment != nil {
		e = *s.Environment
	}
	if b.WaitTimer != nil {
		if *b.WaitTimer < 0 || *b.WaitTimer > 43200 {
			return invalid("wait_timer out of range")
		}
		e.WaitTimer = *b.WaitTimer
	}
	if b.PreventSelfReview != nil {
		e.PreventSelfReview = *b.PreventSelfReview
	}
	if b.Reviewers != nil {
		e.Reviewers = nil
		for _, r := range *b.Reviewers {
			if _, ok := login(r.ID); r.Type != "User" || !ok {
				return invalid("reviewer %s %d does not exist", r.Type, r.ID)
			}
			e.Reviewers = append(e.Reviewers, r.ID)
		}
	}
	switch raw := string(b.DeploymentBranchPolicy); raw {
	case "":
	case "null":
		e.BranchPolicy = nil
	default:
		var p struct {
			ProtectedBranches    *bool `json:"protected_branches"`
			CustomBranchPolicies *bool `json:"custom_branch_policies"`
		}
		if err := decode(b.DeploymentBranchPolicy, &p); err != nil {
			return err
		}
		if p.ProtectedBranches == nil || p.CustomBranchPolicies == nil || *p.ProtectedBranches == *p.CustomBranchPolicies {
			return invalid("protected_branches and custom_branch_policies are both required and opposite")
		}
		e.BranchPolicy = &branchScheme{ProtectedBranches: *p.ProtectedBranches, CustomBranchPolicies: *p.CustomBranchPolicies}
	}
	if e.BranchPolicy == nil || !e.BranchPolicy.CustomBranchPolicies {
		s.BranchPolicies = nil
	}
	s.Environment = &e
	return nil
}

func (s *ghState) createBranchPolicy(body []byte) (branchPolicy, error) {
	var b struct {
		Name *string `json:"name"`
		Type *string `json:"type"`
	}
	if err := decode(body, &b); err != nil {
		return branchPolicy{}, err
	}
	if s.Environment.BranchPolicy == nil || !s.Environment.BranchPolicy.CustomBranchPolicies {
		return branchPolicy{}, invalid("the environment has no custom branch policies")
	}
	if b.Name == nil || *b.Name == "" {
		return branchPolicy{}, invalid("name is required")
	}
	p := branchPolicy{ID: s.NextPolicyID, Name: *b.Name, Type: "branch"}
	if b.Type != nil {
		if err := oneOf("type", *b.Type, "branch", "tag"); err != nil {
			return branchPolicy{}, err
		}
		p.Type = *b.Type
	}
	if slices.ContainsFunc(s.BranchPolicies, func(q branchPolicy) bool { return q.Name == p.Name && q.Type == p.Type }) {
		return branchPolicy{}, invalid("policy %s %s exists", p.Type, p.Name)
	}
	s.NextPolicyID++
	s.BranchPolicies = append(s.BranchPolicies, p)
	return p, nil
}
