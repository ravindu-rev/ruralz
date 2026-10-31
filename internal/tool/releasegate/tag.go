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
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
)

// gitObject is the object a ref or annotated tag points at.
type gitObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// gitRef is a ref of the Git database API.
type gitRef struct {
	Ref    string    `json:"ref"`
	Object gitObject `json:"object"`
}

// gitTag is an annotated tag object with GitHub's signature verification.
type gitTag struct {
	Tag    string `json:"tag"`
	SHA    string `json:"sha"`
	Tagger struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"tagger"`
	Object       gitObject `json:"object"`
	Verification struct {
		Verified  bool   `json:"verified"`
		Reason    string `json:"reason"`
		Signature string `json:"signature"`
	} `json:"verification"`
}

// collaborator is a repository collaborator with its role.
type collaborator struct {
	Login       string `json:"login"`
	RoleName    string `json:"role_name"`
	Permissions struct {
		Admin    bool `json:"admin"`
		Maintain bool `json:"maintain"`
	} `json:"permissions"`
}

// maintainer reports the admin or maintain role, the roles that may tag a
// release and skip a gate.
func (c collaborator) maintainer() bool {
	return c.RoleName == "admin" || c.RoleName == "maintain" || c.Permissions.Admin || c.Permissions.Maintain
}

// gpgKey is a user's registered OpenPGP key with its subkeys.
type gpgKey struct {
	KeyID   string `json:"key_id"`
	Subkeys []struct {
		KeyID string `json:"key_id"`
	} `json:"subkeys"`
}

// sshSigningKey is a user's registered SSH signing key.
type sshSigningKey struct {
	Key string `json:"key"`
}

// release is a GitHub Release.
type release struct {
	TagName     string `json:"tag_name"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
}

// publishedReleases lists the tags of the repository's published releases
// that are not pre-releases.
func publishedReleases(ctx context.Context, g *github) ([]string, error) {
	rs, err := list[release](ctx, g, g.repoPath("/releases"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rs {
		if !r.Draft && !r.Prerelease {
			out = append(out, r.TagName)
		}
	}
	return out, nil
}

// tagResult is what the release workflow needs from the tag check.
type tagResult struct {
	Version version
	// Previous is the newest published release below Version's X.Y.Z,
	// when HasPrev: the benchmark baseline and the alloc/op gate's base.
	Previous version
	HasPrev  bool
	// Latest reports whether Version is above every published release.
	Latest bool
	// Signer is the maintainer whose registered key signed the tag.
	Signer   string
	Commit   string
	Findings []string
}

// checkTag checks a pushed tag as release.yml's first job does
// (docs/engineering/04-release-versioning-and-compatibility.md, "Release
// pipeline"): the name is vX.Y.Z or vX.Y.Z-rc.N, and it is an annotated
// tag on the commit whose signature GitHub verified, made with an SSH or
// OpenPGP signing key that a maintainer (repository role admin or
// maintain) registered. Problems are findings; API failures are errors.
func checkTag(ctx context.Context, g *github, tag, commit string) (*tagResult, error) {
	r := &tagResult{Commit: commit}
	v, err := parseTag(tag)
	if err != nil {
		r.Findings = append(r.Findings, err.Error())
		return r, nil
	}
	r.Version = v
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		return nil, fmt.Errorf("commit %q is not a full lower-case SHA-1", commit)
	}
	var ref gitRef
	if err := g.get(ctx, g.repoPath("/git/ref/tags/%s", url.PathEscape(tag)), &ref); err != nil {
		if errors.Is(err, errNotFound) {
			r.Findings = append(r.Findings, fmt.Sprintf("tag %s does not exist in %s", tag, g.repo))
			return r, nil
		}
		return nil, err
	}
	if ref.Object.Type != "tag" {
		r.Findings = append(r.Findings, fmt.Sprintf("tag %s is a lightweight tag: create release tags with git tag -s", tag))
		return r, nil
	}
	var t gitTag
	if err := g.get(ctx, g.repoPath("/git/tags/%s", ref.Object.SHA), &t); err != nil {
		return nil, err
	}
	if t.Object.Type != "commit" || t.Object.SHA != commit {
		r.Findings = append(r.Findings, fmt.Sprintf("tag %s points at %s %s, not the commit %s this run builds", tag, t.Object.Type, t.Object.SHA, commit))
	}
	if !t.Verification.Verified {
		r.Findings = append(r.Findings, fmt.Sprintf("GitHub did not verify the signature of tag %s (%s): sign it with a registered SSH or GPG signing key", tag, t.Verification.Reason))
		return r, nil
	}
	who, err := tagSigner(ctx, g, t.Verification.Signature)
	if err != nil {
		var f *signerFinding
		if errors.As(err, &f) {
			r.Findings = append(r.Findings, fmt.Sprintf("tag %s: %s", tag, f.msg))
			return r, nil
		}
		return nil, err
	}
	r.Signer = who
	published, err := publishedReleases(ctx, g)
	if err != nil {
		return nil, err
	}
	r.Previous, r.HasPrev = previousRelease(v, published)
	r.Latest = isLatest(v, published)
	return r, nil
}

// latestRelease reports whether the release tag is above every published
// release, as the publish job decides it just before publishing.
func latestRelease(ctx context.Context, g *github, tag string) (bool, error) {
	v, err := parseTag(tag)
	if err != nil {
		return false, &usageError{err.Error()}
	}
	published, err := publishedReleases(ctx, g)
	if err != nil {
		return false, err
	}
	return isLatest(v, published), nil
}

// supportedLines returns the newest release tag of each supported line,
// newest line first.
func supportedLines(ctx context.Context, g *github) ([]string, error) {
	published, err := publishedReleases(ctx, g)
	if err != nil {
		return nil, err
	}
	tags := []string{}
	for _, v := range newestOfLines(published) {
		tags = append(tags, v.tag())
	}
	return tags, nil
}

// jsonArray renders strings as a compact JSON array.
func jsonArray(ss []string) string {
	data, err := json.Marshal(ss)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// signerFinding is a signature that no maintainer's registered key made.
type signerFinding struct{ msg string }

func (e *signerFinding) Error() string { return e.msg }

// maintainers lists the collaborators with the admin or maintain role.
func maintainers(ctx context.Context, g *github) ([]string, error) {
	cs, err := list[collaborator](ctx, g, g.repoPath("/collaborators?affiliation=all"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range cs {
		if c.maintainer() {
			out = append(out, c.Login)
		}
	}
	slices.Sort(out)
	return out, nil
}

// tagSigner returns the login of the maintainer whose registered signing
// key made sig.
func tagSigner(ctx context.Context, g *github, sig string) (string, error) {
	s, err := parseSigner(sig)
	if err != nil {
		return "", &signerFinding{"unreadable signature: " + err.Error()}
	}
	logins, err := maintainers(ctx, g)
	if err != nil {
		return "", err
	}
	for _, login := range logins {
		switch s.Kind {
		case sigSSH:
			keys, err := list[sshSigningKey](ctx, g, "/users/"+url.PathEscape(login)+"/ssh_signing_keys")
			if err != nil {
				return "", err
			}
			for _, k := range keys {
				if blob, err := sshKeyBlob(k.Key); err == nil && bytes.Equal(blob, s.SSHKey) {
					return login, nil
				}
			}
		case sigPGP:
			keys, err := list[gpgKey](ctx, g, "/users/"+url.PathEscape(login)+"/gpg_keys")
			if err != nil {
				return "", err
			}
			for _, k := range keys {
				ids := []string{k.KeyID}
				for _, sk := range k.Subkeys {
					ids = append(ids, sk.KeyID)
				}
				for _, id := range ids {
					for _, want := range s.KeyIDs {
						if id != "" && strings.EqualFold(id, want) {
							return login, nil
						}
					}
				}
			}
		}
	}
	return "", &signerFinding{fmt.Sprintf("the %s signing key is registered to no maintainer (repository role admin or maintain) among %s", s.Kind, strings.Join(logins, ", "))}
}

// writeOutputs appends the workflow outputs of a passed tag check to w as
// key=value lines (the $GITHUB_OUTPUT format).
func writeOutputs(w io.Writer, r *tagResult) error {
	prevTag, prevVersion := "", ""
	if r.HasPrev {
		prevTag, prevVersion = r.Previous.tag(), r.Previous.String()
	}
	lines := []string{
		"version=" + r.Version.String(),
		"kind=" + r.Version.kind(),
		fmt.Sprintf("prerelease=%t", r.Version.kind() == kindCandidate),
		"previous=" + prevTag,
		"previous-version=" + prevVersion,
		fmt.Sprintf("latest=%t", r.Latest),
		"signer=" + r.Signer,
	}
	_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	return err
}

// appendOutputs appends the outputs to the file path (created when absent).
func appendOutputs(path string, r *tagResult) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // The output file given on the command line, $GITHUB_OUTPUT in CI.
	if err != nil {
		return err
	}
	if err := writeOutputs(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
