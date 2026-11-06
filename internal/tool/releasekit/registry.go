// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// maxManifestBytes bounds a manifest the registry returns: an image index
// of two platforms is a few hundred bytes, and registries refuse
// manifests above 4 MiB.
const maxManifestBytes = 4 << 20

// manifestTypes are the manifest media types the tags may copy.
func manifestTypes() []string {
	return []string{
		ociIndexType,
		"application/vnd.docker.distribution.manifest.list.v2+json",
		ociManifestType,
		"application/vnd.docker.distribution.manifest.v2+json",
	}
}

// tagImageOptions are the flags of `releasekit tag-image`.
type tagImageOptions struct {
	// version is the release, X.Y.Z, and digest its verified image index.
	version, digest string
	// check only checks the immutable X.Y.Z tag.
	check bool
	// repository is host/path, such as ghcr.io/ravindu-rev/ruralzd;
	// registry overrides its API root (tests).
	repository, registry string
	// user and token sign in to the registry's token service.
	user, token string
}

// registry is a minimal OCI distribution client: manifests by tag or
// digest, with the bearer token flow of the registry's WWW-Authenticate
// challenge.
type registry struct {
	base, name  string
	user, token string
	http        *http.Client
	// bearer caches the token of the last scope fetched.
	bearer, scope string
}

// newRegistry validates the repository reference.
func newRegistry(o tagImageOptions, client *http.Client) (*registry, error) {
	m := regexp.MustCompile(`^([a-z0-9.-]+(?::[0-9]+)?)/([a-z0-9]+(?:[._/-][a-z0-9]+)*)$`).FindStringSubmatch(o.repository)
	if m == nil {
		return nil, fmt.Errorf("repository %q is not host/name", o.repository)
	}
	base := o.registry
	if base == "" {
		base = "https://" + m[1]
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("registry %q is not an http(s) URL", base)
	}
	if o.token == "" {
		return nil, errors.New("no registry token: set GH_TOKEN or GITHUB_TOKEN")
	}
	user := o.user
	if user == "" {
		user = "token"
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &registry{base: strings.TrimRight(base, "/"), name: m[2], user: user, token: o.token, http: client}, nil
}

// errManifestUnknown is a 404 for a manifest.
var errManifestUnknown = errors.New("manifest unknown")

// manifestBlob is a manifest's bytes, media type and digest.
type manifestBlob struct {
	data      []byte
	mediaType string
	digest    string
}

// do sends a request, answering a bearer challenge once with a token for
// scope from the realm the challenge names.
func (r *registry) do(ctx context.Context, method, ref, scope string, body []byte, contentType string) (*http.Response, error) {
	target := r.base + "/v2/" + r.name + "/manifests/" + ref
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", strings.Join(manifestTypes(), ", "))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if r.bearer != "" && r.scope == scope {
			req.Header.Set("Authorization", "Bearer "+r.bearer)
		}
		resp, err := r.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", method, target, err)
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxManifestBytes))
		_ = resp.Body.Close()
		if err := r.authorize(ctx, challenge, scope); err != nil {
			return nil, err
		}
	}
}

// authorize fetches a bearer token for scope from the challenge's realm
// with the registry credentials.
func (r *registry) authorize(ctx context.Context, challenge, scope string) error {
	params, ok := parseChallenge(challenge)
	if !ok || params["realm"] == "" {
		return fmt.Errorf("registry asked for credentials without a bearer realm: %q", challenge)
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || (realm.Scheme != "https" && realm.Scheme != "http") {
		return fmt.Errorf("registry token realm %q is not an http(s) URL", params["realm"])
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(r.user, r.token)
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("registry token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return fmt.Errorf("registry token: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry token: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return fmt.Errorf("registry token: %w", err)
	}
	r.bearer, r.scope = t.Token, scope
	if r.bearer == "" {
		r.bearer = t.AccessToken
	}
	if r.bearer == "" {
		return errors.New("registry token: the token service returned no token")
	}
	return nil
}

// parseChallenge parses `Bearer k="v",k2="v2"`.
func parseChallenge(h string) (map[string]string, bool) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`([A-Za-z_]+)="([^"]*)"`).FindAllStringSubmatch(rest, -1) {
		out[strings.ToLower(m[1])] = m[2]
	}
	return out, true
}

// pullScope and pushScope are the token scopes of the repository.
func (r *registry) pullScope() string { return "repository:" + r.name + ":pull" }
func (r *registry) pushScope() string { return "repository:" + r.name + ":pull,push" }

// get fetches the manifest ref names, a tag or a digest.
func (r *registry) get(ctx context.Context, ref, scope string) (manifestBlob, error) {
	resp, err := r.do(ctx, http.MethodGet, ref, scope, nil, "")
	if err != nil {
		return manifestBlob{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return manifestBlob{}, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return manifestBlob{}, errManifestUnknown
	case resp.StatusCode != http.StatusOK:
		return manifestBlob{}, fmt.Errorf("GET manifest %s: %d %s", ref, resp.StatusCode, http.StatusText(resp.StatusCode))
	case len(data) > maxManifestBytes:
		return manifestBlob{}, fmt.Errorf("GET manifest %s: larger than %d bytes", ref, maxManifestBytes)
	}
	mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	if mt = strings.TrimSpace(mt); !slices.Contains(manifestTypes(), mt) {
		// A registry that labels no manifest type: the manifest names its
		// own.
		var doc struct {
			MediaType string `json:"mediaType"`
		}
		if json.Unmarshal(data, &doc) == nil {
			mt = doc.MediaType
		}
	}
	return manifestBlob{data: data, mediaType: mt, digest: "sha256:" + sha256Hex(data)}, nil
}

// put stores the manifest under tag, byte for byte, so the tag names the
// same digest.
func (r *registry) put(ctx context.Context, tag string, m manifestBlob) error {
	resp, err := r.do(ctx, http.MethodPut, tag, r.pushScope(), m.data, m.mediaType)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxManifestBytes))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PUT manifest %s: %d %s", tag, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" && d != m.digest {
		return fmt.Errorf("PUT manifest %s: the registry stored %s, not %s", tag, d, m.digest)
	}
	return nil
}

// runTagImage checks and, unless o.check, sets the image tags of a
// published release (docs/engineering/04-release-versioning-and-compatibility.md,
// "Release pipeline"): the immutable X.Y.Z tag must be absent or name the
// digest already (a re-run of the publish job), so it is never pushed
// again; X.Y moves to the digest only when the release is the newest
// published release of its line. Each tag copies the verified index byte
// for byte and is read back. published lists the published releases,
// which a check does not read.
func runTagImage(ctx context.Context, o tagImageOptions, client *http.Client, published []releaseVersion, stdout io.Writer) ([]string, error) {
	v, ok := parseReleaseTag("v" + o.version)
	if !ok {
		return nil, fmt.Errorf("version %q is not a release X.Y.Z", o.version)
	}
	if !digestPattern().MatchString(o.digest) {
		return nil, fmt.Errorf("digest %q is not sha256:<64 hex>", o.digest)
	}
	r, err := newRegistry(o, client)
	if err != nil {
		return nil, err
	}
	scope := r.pullScope()
	if !o.check {
		scope = r.pushScope()
	}
	index, err := r.get(ctx, o.digest, scope)
	if errors.Is(err, errManifestUnknown) {
		return []string{fmt.Sprintf("%s@%s is not in the registry", o.repository, o.digest)}, nil
	}
	if err != nil {
		return nil, err
	}
	if index.digest != o.digest {
		return []string{fmt.Sprintf("%s@%s: the registry returned content of digest %s", o.repository, o.digest, index.digest)}, nil
	}
	immutable := v.String()
	cur, err := r.get(ctx, immutable, scope)
	switch {
	case errors.Is(err, errManifestUnknown):
		_, _ = fmt.Fprintf(stdout, "releasekit: %s:%s is absent\n", o.repository, immutable)
	case err != nil:
		return nil, err
	case cur.digest == o.digest:
		_, _ = fmt.Fprintf(stdout, "releasekit: %s:%s already names %s\n", o.repository, immutable, o.digest)
	default:
		return []string{fmt.Sprintf("%s:%s already names %s; a version's tag is never pushed again", o.repository, immutable, cur.digest)}, nil
	}
	if o.check {
		return nil, nil
	}
	tags := []string{immutable}
	if newestOfLine(v, published) {
		tags = append(tags, v.line())
	} else {
		_, _ = fmt.Fprintf(stdout, "releasekit: %s:%s stays: a newer release of line %s is published\n", o.repository, v.line(), v.line())
	}
	for _, t := range tags {
		if err := r.put(ctx, t, index); err != nil {
			return nil, err
		}
		back, err := r.get(ctx, t, scope)
		if err != nil {
			return nil, err
		}
		if back.digest != o.digest {
			return []string{fmt.Sprintf("%s:%s names %s after tagging, not %s", o.repository, t, back.digest, o.digest)}, nil
		}
		_, _ = fmt.Fprintf(stdout, "releasekit: tagged %s:%s as %s\n", o.repository, t, o.digest)
	}
	return nil, nil
}
