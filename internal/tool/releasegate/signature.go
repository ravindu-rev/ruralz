// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Signature kinds of a Git tag: GitHub verifies both against the signing
// keys registered on an account.
const (
	sigSSH = "ssh"
	sigPGP = "gpg"
)

// signer identifies the key that made a tag signature: an SSH public key
// in wire format, or an OpenPGP key ID (16 upper-case hex digits) or
// fingerprint.
type signer struct {
	Kind string
	// SSHKey is the signer's public key blob (SSH wire format).
	SSHKey []byte
	// KeyIDs are the OpenPGP issuer key IDs the signature names.
	KeyIDs []string
}

// armored returns the base64 body between the BEGIN and END lines of an
// ASCII-armored block, skipping armor headers and the OpenPGP checksum.
func armored(text, label string) ([]byte, error) {
	begin, end := "-----BEGIN "+label+"-----", "-----END "+label+"-----"
	_, rest, ok := strings.Cut(text, begin)
	if !ok {
		return nil, fmt.Errorf("no %s block", label)
	}
	body, _, ok := strings.Cut(rest, end)
	if !ok {
		return nil, fmt.Errorf("unterminated %s block", label)
	}
	// Armor headers ("Comment: ...") hold a colon, which base64 never
	// does, and the OpenPGP checksum line starts with "=", which a base64
	// line never does.
	var b64 strings.Builder
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "=") || strings.Contains(l, ":") {
			continue
		}
		b64.WriteString(l)
	}
	data, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		return nil, fmt.Errorf("%s block: %w", label, err)
	}
	return data, nil
}

// parseSigner reads who made an armored Git signature: an SSH signature
// (PROTOCOL.sshsig) carries the public key; an OpenPGP signature names its
// issuer key ID or fingerprint in a subpacket (RFC 9580 section 5.2.3).
func parseSigner(sig string) (signer, error) {
	switch {
	case strings.Contains(sig, "-----BEGIN SSH SIGNATURE-----"):
		data, err := armored(sig, "SSH SIGNATURE")
		if err != nil {
			return signer{}, err
		}
		key, err := sshSigKey(data)
		if err != nil {
			return signer{}, err
		}
		return signer{Kind: sigSSH, SSHKey: key}, nil
	case strings.Contains(sig, "-----BEGIN PGP SIGNATURE-----"):
		data, err := armored(sig, "PGP SIGNATURE")
		if err != nil {
			return signer{}, err
		}
		ids, err := pgpIssuers(data)
		if err != nil {
			return signer{}, err
		}
		return signer{Kind: sigPGP, KeyIDs: ids}, nil
	default:
		return signer{}, errors.New("neither an SSH nor an OpenPGP signature")
	}
}

// sshString reads one SSH wire-format string.
func sshString(b []byte) (s, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, errors.New("truncated SSH signature")
	}
	// A uint32 always fits an int64, as does a length.
	n := int64(binary.BigEndian.Uint32(b))
	if n > int64(len(b))-4 {
		return nil, nil, errors.New("truncated SSH signature")
	}
	return b[4 : 4+n], b[4+n:], nil
}

// sshSigKey returns the public key of an SSHSIG blob: the magic "SSHSIG",
// version 1, then the public key, namespace, reserved, hash algorithm and
// signature strings. Git signs tags in the "git" namespace.
func sshSigKey(blob []byte) ([]byte, error) {
	if !bytes.HasPrefix(blob, []byte("SSHSIG")) || len(blob) < 10 {
		return nil, errors.New("not an SSHSIG signature")
	}
	if v := binary.BigEndian.Uint32(blob[6:10]); v != 1 {
		return nil, fmt.Errorf("SSHSIG version %d, want 1", v)
	}
	key, rest, err := sshString(blob[10:])
	if err != nil {
		return nil, err
	}
	ns, _, err := sshString(rest)
	if err != nil {
		return nil, err
	}
	if string(ns) != "git" {
		return nil, fmt.Errorf("SSH signature namespace %q, want git", ns)
	}
	if len(key) == 0 {
		return nil, errors.New("SSH signature without a public key")
	}
	return key, nil
}

// sshKeyBlob decodes the blob of an OpenSSH public key line such as
// "ssh-ed25519 AAAA... title".
func sshKeyBlob(line string) ([]byte, error) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return nil, fmt.Errorf("%q is not an SSH public key", line)
	}
	return base64.StdEncoding.DecodeString(f[1])
}

// pgpPacket reads one OpenPGP packet header and returns its tag and body.
func pgpPacket(b []byte) (tag int, body, rest []byte, err error) {
	if len(b) < 2 || b[0]&0x80 == 0 {
		return 0, nil, nil, errors.New("not an OpenPGP packet")
	}
	var n, hdr int
	if b[0]&0x40 != 0 { // new format
		tag = int(b[0] & 0x3f)
		switch o := int(b[1]); {
		case o < 192:
			n, hdr = o, 2
		case o < 224:
			if len(b) < 3 {
				return 0, nil, nil, errors.New("truncated OpenPGP packet")
			}
			n, hdr = (o-192)<<8+int(b[2])+192, 3
		case o == 255:
			if len(b) < 6 {
				return 0, nil, nil, errors.New("truncated OpenPGP packet")
			}
			n, hdr = int(binary.BigEndian.Uint32(b[2:6])), 6
		default:
			return 0, nil, nil, errors.New("partial-length OpenPGP packet")
		}
	} else { // old format
		tag = int(b[0]>>2) & 0x0f
		switch b[0] & 3 {
		case 0:
			n, hdr = int(b[1]), 2
		case 1:
			if len(b) < 3 {
				return 0, nil, nil, errors.New("truncated OpenPGP packet")
			}
			n, hdr = int(binary.BigEndian.Uint16(b[1:3])), 3
		case 2:
			if len(b) < 5 {
				return 0, nil, nil, errors.New("truncated OpenPGP packet")
			}
			n, hdr = int(binary.BigEndian.Uint32(b[1:5])), 5
		default:
			return 0, nil, nil, errors.New("indeterminate-length OpenPGP packet")
		}
	}
	if n < 0 || n > len(b)-hdr {
		return 0, nil, nil, errors.New("truncated OpenPGP packet")
	}
	return tag, b[hdr : hdr+n], b[hdr+n:], nil
}

// pgpIssuers returns the issuer key IDs of the first signature packet: from
// a v3 signature's key ID field, or from Issuer Key ID (16) and Issuer
// Fingerprint (33) subpackets of a v4 or v6 signature. A v4 key's ID is the
// last 8 octets of its fingerprint, a v6 key's the first 8.
func pgpIssuers(data []byte) ([]string, error) {
	for len(data) > 0 {
		tag, body, rest, err := pgpPacket(data)
		if err != nil {
			return nil, err
		}
		data = rest
		if tag != 2 {
			continue
		}
		if len(body) < 1 {
			return nil, errors.New("empty OpenPGP signature packet")
		}
		var ids []string
		add := func(id []byte) {
			s := strings.ToUpper(hex.EncodeToString(id))
			for _, x := range ids {
				if x == s {
					return
				}
			}
			ids = append(ids, s)
		}
		switch body[0] {
		case 3:
			if len(body) < 15 {
				return nil, errors.New("truncated v3 OpenPGP signature")
			}
			add(body[7:15])
		case 4, 6:
			wide := body[0] == 6
			p := body[4:]
			for area := 0; area < 2; area++ {
				var n int
				if wide {
					if len(p) < 4 {
						return nil, errors.New("truncated OpenPGP signature")
					}
					n, p = int(binary.BigEndian.Uint32(p)), p[4:]
				} else {
					if len(p) < 2 {
						return nil, errors.New("truncated OpenPGP signature")
					}
					n, p = int(binary.BigEndian.Uint16(p)), p[2:]
				}
				if n < 0 || n > len(p) {
					return nil, errors.New("truncated OpenPGP signature subpackets")
				}
				if err := subpacketIssuers(p[:n], add); err != nil {
					return nil, err
				}
				p = p[n:]
			}
		default:
			return nil, fmt.Errorf("OpenPGP signature version %d is not supported", body[0])
		}
		if len(ids) == 0 {
			return nil, errors.New("OpenPGP signature names no issuer")
		}
		return ids, nil
	}
	return nil, errors.New("no OpenPGP signature packet")
}

// subpacketIssuers walks one signature subpacket area.
func subpacketIssuers(p []byte, add func([]byte)) error {
	for len(p) > 0 {
		var n, hdr int
		switch o := int(p[0]); {
		case o < 192:
			n, hdr = o, 1
		case o < 255:
			if len(p) < 2 {
				return errors.New("truncated OpenPGP subpacket")
			}
			n, hdr = (o-192)<<8+int(p[1])+192, 2
		default:
			if len(p) < 5 {
				return errors.New("truncated OpenPGP subpacket")
			}
			n, hdr = int(binary.BigEndian.Uint32(p[1:5])), 5
		}
		if n < 1 || n > len(p)-hdr {
			return errors.New("truncated OpenPGP subpacket")
		}
		sp := p[hdr : hdr+n]
		switch typ, data := sp[0]&0x7f, sp[1:]; typ {
		case 16: // Issuer Key ID
			if len(data) == 8 {
				add(data)
			}
		case 33: // Issuer Fingerprint: key version, then the fingerprint
			switch {
			case len(data) == 21 && data[0] == 4:
				add(data[13:21])
			case len(data) == 33 && data[0] == 6:
				add(data[1:9])
			}
		}
		p = p[hdr+n:]
	}
	return nil
}
