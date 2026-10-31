// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseSigner: an SSH signature yields its public key, an OpenPGP
// signature its issuer key IDs; a signature of another namespace or kind,
// or a corrupt one, is refused.
func TestParseSigner(t *testing.T) {
	blob, _ := sshKeyPair(1)
	s, err := parseSigner(sshSignature(blob, "git"))
	if err != nil || s.Kind != sigSSH || !bytes.Equal(s.SSHKey, blob) {
		t.Errorf("SSH signature: %+v, %v", s, err)
	}
	fpr, _ := hex.DecodeString("0123456789ABCDEF0123456789ABCDEF01234567")
	s, err = parseSigner(pgpSignature(fpr))
	if err != nil || s.Kind != sigPGP || len(s.KeyIDs) != 1 || s.KeyIDs[0] != "89ABCDEF01234567" {
		t.Errorf("OpenPGP signature: %+v, %v; want key ID 89ABCDEF01234567", s, err)
	}
	for name, sig := range map[string]string{
		"file namespace":  sshSignature(blob, "file"),
		"not a signature": "hello",
		"truncated SSH":   armor("SSH SIGNATURE", []byte("SSHSIG\x00\x00\x00\x01\x00\x00"), ""),
		"bad base64":      "-----BEGIN SSH SIGNATURE-----\n!!!\n-----END SSH SIGNATURE-----\n",
		"unterminated":    "-----BEGIN PGP SIGNATURE-----\nwsBc\n",
		"PGP no issuer":   armor("PGP SIGNATURE", []byte{0xC2, 8, 4, 0, 22, 10, 0, 0, 0, 0}, ""),
		"PGP version 5":   armor("PGP SIGNATURE", []byte{0xC2, 4, 5, 0, 22, 10}, ""),
	} {
		if _, err := parseSigner(sig); err == nil {
			t.Errorf("%s: parseSigner accepted it", name)
		}
	}
}

// TestPGPIssuers: the old packet format and a v3 signature's key ID field
// name the issuer too, and the first signature packet counts.
func TestPGPIssuers(t *testing.T) {
	v3 := []byte{0x88, 20, 3, 5, 0x00, 1, 2, 3, 4, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 17, 2, 0, 0, 0}
	ids, err := pgpIssuers(v3)
	if err != nil || len(ids) != 1 || ids[0] != "1122334455667788" {
		t.Errorf("v3 signature: %v, %v", ids, err)
	}
	marker := []byte{0xCA, 3, 'P', 'G', 'P'} // a marker packet before the signature
	fpr := bytes.Repeat([]byte{0xAB}, 20)
	sig, _ := armored(pgpSignature(fpr), "PGP SIGNATURE")
	ids, err = pgpIssuers(append(marker, sig...))
	if err != nil || len(ids) != 1 || ids[0] != strings.Repeat("AB", 8) {
		t.Errorf("signature after a marker packet: %v, %v", ids, err)
	}
}

// tagFixture is a fake repository with a maintainer whose SSH key signs
// tags, a GPG-signing maintainer, a writer with a registered key, and
// published releases.
func tagFixture(t *testing.T) (*fakeGitHub, []byte, []byte) {
	t.Helper()
	f := newFakeGitHub(t)
	f.addCollaborator("jane", "admin")
	f.addCollaborator("omar", "maintain")
	f.addCollaborator("wes", "write")
	janeBlob, janeLine := sshKeyPair(1)
	wesBlob, wesLine := sshKeyPair(2)
	f.sshKeys["jane"] = []sshSigningKey{{Key: "ssh-rsa AAAAB3Nza other"}, {Key: janeLine}}
	f.sshKeys["wes"] = []sshSigningKey{{Key: wesLine}}
	f.gpgKeys["omar"] = []gpgKey{{KeyID: "0000000000000001", Subkeys: []struct {
		KeyID string `json:"key_id"`
	}{{KeyID: "89ABCDEF01234567"}}}}
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f.addRelease("v0.1.0", false, day)
	f.addRelease("v0.1.1", false, day)
	f.addRelease("v0.2.0-rc.1", true, day)
	return f, janeBlob, wesBlob
}

// TestCheckTag: a release or candidate tag passes only as an annotated tag
// on the commit whose verified signature a maintainer's registered key
// made; the result names the previous release and whether it is latest.
func TestCheckTag(t *testing.T) {
	f, janeBlob, wesBlob := tagFixture(t)
	fpr, _ := hex.DecodeString("0123456789ABCDEF0123456789ABCDEF01234567")
	f.addTag("v0.2.0", commitA, sshSignature(janeBlob, "git"), true)
	f.addTag("v0.2.1-rc.1", commitA, pgpSignature(fpr), true)
	f.addTag("v0.3.0", commitA, sshSignature(wesBlob, "git"), true)
	f.addTag("v0.4.0", commitA, sshSignature(janeBlob, "git"), false)
	f.addTag("v0.5.0", commitB, sshSignature(janeBlob, "git"), true)
	f.refs["v0.6.0"] = gitObject{Type: "commit", SHA: commitA}
	g := f.client(t)

	r, err := checkTag(t.Context(), g, "v0.2.0", commitA)
	if err != nil || len(r.Findings) != 0 {
		t.Fatalf("v0.2.0: %v %q", err, r.Findings)
	}
	if r.Signer != "jane" || !r.HasPrev || r.Previous.tag() != "v0.1.1" || !r.Latest || r.Version.kind() != kindRelease {
		t.Errorf("v0.2.0 = %+v", r)
	}
	r, err = checkTag(t.Context(), g, "v0.2.1-rc.1", commitA)
	if err != nil || len(r.Findings) != 0 || r.Signer != "omar" || r.Latest {
		t.Errorf("GPG-signed candidate: %+v, %v", r, err)
	}

	cases := []struct {
		tag, commit, want string
	}{
		{"v0.3.0", commitA, "registered to no maintainer"},
		{"v0.4.0", commitA, "GitHub did not verify the signature"},
		{"v0.5.0", commitA, "not the commit " + commitA},
		{"v0.6.0", commitA, "lightweight tag"},
		{"v0.7.0", commitA, "does not exist"},
		{"release-0.1", commitA, "neither vX.Y.Z nor vX.Y.Z-rc.N"},
	}
	for _, c := range cases {
		r, err := checkTag(t.Context(), g, c.tag, c.commit)
		if err != nil {
			t.Fatalf("%s: %v", c.tag, err)
		}
		if !strings.Contains(strings.Join(r.Findings, "\n"), c.want) {
			t.Errorf("checkTag(%s) findings %q, want %q", c.tag, r.Findings, c.want)
		}
	}
	if _, err := checkTag(t.Context(), g, "v0.2.0", "HEAD"); err == nil {
		t.Error("a commit that is no SHA-1 is not an error")
	}
}

// TestTagOutputs: the step outputs of a passed check, appended to the
// $GITHUB_OUTPUT file.
func TestTagOutputs(t *testing.T) {
	v, _ := parseTag("v0.2.0-rc.1")
	p, _ := parseTag("v0.1.1")
	r := &tagResult{Version: v, Previous: p, HasPrev: true, Signer: "jane"}
	path := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(path, []byte("earlier=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendOutputs(path, r); err != nil {
		t.Fatal(err)
	}
	got := readBack(t, path)
	want := "earlier=1\nversion=0.2.0-rc.1\nkind=candidate\nprerelease=true\nprevious=v0.1.1\nprevious-version=0.1.1\nlatest=false\nsigner=jane\n"
	if string(got) != want {
		t.Errorf("outputs %q, want %q", got, want)
	}
	var b bytes.Buffer
	first, _ := parseTag("v0.1.0")
	if err := writeOutputs(&b, &tagResult{Version: first, Latest: true, Signer: "jane"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "previous=\nprevious-version=\nlatest=true\n") {
		t.Errorf("first release outputs %q", b.String())
	}
}

// TestNextLink: listings follow GitHub's rel="next" links.
func TestNextLink(t *testing.T) {
	h := `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`
	if got := nextLink(h); got != "https://api.github.com/x?page=2" {
		t.Errorf("nextLink = %q", got)
	}
	if got := nextLink(`<https://api.github.com/x?page=1>; rel="prev"`); got != "" {
		t.Errorf("nextLink without next = %q", got)
	}
}
