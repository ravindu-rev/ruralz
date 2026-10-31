// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mtls

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"net/url"
	"slices"
	"testing"
	"time"
)

// OIDs of the subject tests.
var (
	oidCN     = asn1.ObjectIdentifier{2, 5, 4, 3}
	oidO      = asn1.ObjectIdentifier{2, 5, 4, 10}
	oidOU     = asn1.ObjectIdentifier{2, 5, 4, 11}
	oidC      = asn1.ObjectIdentifier{2, 5, 4, 6}
	oidL      = asn1.ObjectIdentifier{2, 5, 4, 7}
	oidST     = asn1.ObjectIdentifier{2, 5, 4, 8}
	oidStreet = asn1.ObjectIdentifier{2, 5, 4, 9}
	oidSerial = asn1.ObjectIdentifier{2, 5, 4, 5}
	oidDC     = asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 25}
	oidUID    = asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 1}
	oidEmail  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 1}
)

// atv returns one attribute with a primitive universal value of tag.
func atv(oid asn1.ObjectIdentifier, tag int, value string) attributeTypeAndValue {
	return attributeTypeAndValue{Type: oid, Value: asn1.RawValue{Class: asn1.ClassUniversal, Tag: tag, Bytes: []byte(value)}}
}

// text returns one attribute with a UTF8String value.
func text(oid asn1.ObjectIdentifier, value string) attributeTypeAndValue {
	return atv(oid, asn1.TagUTF8String, value)
}

// rdn returns one RDN of members.
func rdn(members ...attributeTypeAndValue) relativeDistinguishedNameSET {
	return relativeDistinguishedNameSET(members)
}

// certWith returns a certificate whose RawSubject encodes seq.
func certWith(t *testing.T, seq rdnSequence) *x509.Certificate {
	t.Helper()
	der, err := asn1.Marshal(seq)
	if err != nil {
		t.Fatal(err)
	}
	return &x509.Certificate{RawSubject: der}
}

// TestSubjectRFC4514 checks the RFC 4514 string of a subject: RDNs from
// last to first joined by ",", multi-valued RDN members joined by "+" in
// encoded order, the section 3 short names, directory strings decoded to
// UTF-8 as crypto/x509 decodes them, the section 2.4 escapes, and "#" with
// the hex of the BER encoding for other types and values.
func TestSubjectRFC4514(t *testing.T) {
	tests := []struct {
		name string
		seq  rdnSequence
		want string
	}{
		{"last RDN first", rdnSequence{rdn(text(oidC, "NZ")), rdn(text(oidO, "Acme")), rdn(text(oidCN, "client"))}, "CN=client,O=Acme,C=NZ"},
		{"encoded order kept", rdnSequence{rdn(text(oidCN, "client")), rdn(text(oidO, "Acme"))}, "O=Acme,CN=client"},
		{"multi-valued RDN", rdnSequence{rdn(text(oidCN, "aaa"), text(oidOU, "bbb")), rdn(text(oidO, "Acme"))}, "O=Acme,CN=aaa+OU=bbb"},
		{"repeated attribute", rdnSequence{rdn(text(oidOU, "one")), rdn(text(oidOU, "two"))}, "OU=two,OU=one"},
		{"all short names", rdnSequence{
			rdn(atv(oidDC, asn1.TagIA5String, "com")), rdn(atv(oidDC, asn1.TagIA5String, "example")),
			rdn(atv(oidC, asn1.TagPrintableString, "NZ")), rdn(text(oidST, "Otago")), rdn(text(oidL, "Dunedin")),
			rdn(text(oidStreet, "1 Main St")), rdn(text(oidUID, "jdoe")),
		}, "UID=jdoe,STREET=1 Main St,L=Dunedin,ST=Otago,C=NZ,DC=example,DC=com"},
		{"leading space", rdnSequence{rdn(text(oidCN, " lead"))}, `CN=\ lead`},
		{"trailing space", rdnSequence{rdn(text(oidCN, "trail "))}, `CN=trail\ `},
		{"single space", rdnSequence{rdn(text(oidCN, " "))}, `CN=\ `},
		{"leading number sign", rdnSequence{rdn(text(oidCN, "#hash"))}, `CN=\#hash`},
		{"inner number sign and equals", rdnSequence{rdn(text(oidCN, "a#b=c"))}, `CN=a#b=c`},
		{"special characters", rdnSequence{rdn(text(oidCN, `a,b+c;d<e>f"g\h`))}, `CN=a\,b\+c\;d\<e\>f\"g\\h`},
		{"NUL", rdnSequence{rdn(text(oidCN, "nul\x00byte"))}, `CN=nul\00byte`},
		{"UTF-8", rdnSequence{rdn(text(oidO, "Café Ōtautahi"))}, "O=Café Ōtautahi"},
		{"T61String as Latin-1", rdnSequence{rdn(atv(oidCN, asn1.TagT61String, "caf\xe9"))}, "CN=café"},
		{"BMPString", rdnSequence{rdn(atv(oidCN, asn1.TagBMPString, "\x00h\x00i"))}, "CN=hi"},
		{"BMPString with terminator", rdnSequence{rdn(atv(oidCN, asn1.TagBMPString, "\x00h\x00i\x00\x00"))}, "CN=hi"},
		{"other type", rdnSequence{rdn(atv(oidEmail, asn1.TagIA5String, "a@b.example")), rdn(text(oidCN, "x"))}, "CN=x,1.2.840.113549.1.9.1=#160b6140622e6578616d706c65"},
		{"serialNumber has no short name", rdnSequence{rdn(atv(oidSerial, asn1.TagPrintableString, "1234"))}, "2.5.4.5=#130431323334"},
		{"short name with a non-string value", rdnSequence{rdn(atv(oidCN, asn1.TagInteger, "\x05"))}, "CN=#020105"},
		{"invalid UTF-8", rdnSequence{rdn(text(oidCN, "\xff"))}, "CN=#0c01ff"},
		{"non-ASCII PrintableString", rdnSequence{rdn(atv(oidCN, asn1.TagPrintableString, "\xe9"))}, "CN=#1301e9"},
		{"odd BMPString", rdnSequence{rdn(atv(oidCN, asn1.TagBMPString, "\x00"))}, "CN=#1e0100"},
		{"BMPString surrogate", rdnSequence{rdn(atv(oidCN, asn1.TagBMPString, "\xd8\x00"))}, "CN=#1e02d800"},
		{"empty RDN skipped", rdnSequence{rdn(text(oidCN, "a")), rdn(), rdn(text(oidO, "b"))}, "O=b,CN=a"},
		{"empty subject", rdnSequence{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Subject(certWith(t, tt.seq)); got != tt.want {
				t.Errorf("Subject = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSubjectFallback checks that a subject encoding/asn1 cannot read
// falls back to crypto/x509's rendering, so a parsed certificate always
// has a subject string.
func TestSubjectFallback(t *testing.T) {
	c := &x509.Certificate{RawSubject: []byte{0x30, 0x05, 0x01}, Subject: pkix.Name{CommonName: "fallback"}}
	if got := Subject(c); got != "CN=fallback" {
		t.Errorf("Subject = %q, want CN=fallback", got)
	}
	trailing := certWith(t, rdnSequence{rdn(text(oidCN, "x"))})
	trailing.RawSubject = append(trailing.RawSubject, 0)
	trailing.Subject = pkix.Name{CommonName: "fallback"}
	if got := Subject(trailing); got != "CN=fallback" {
		t.Errorf("Subject with trailing data = %q, want CN=fallback", got)
	}
}

// TestSubjectOfIssuedCertificate checks Subject on a certificate
// crypto/x509 issued and parsed, a multi-valued RDN included.
func TestSubjectOfIssuedCertificate(t *testing.T) {
	root := newCA(t, "Root")
	c := custom(t, root, &x509.Certificate{
		Subject: pkix.Name{ExtraNames: []pkix.AttributeTypeAndValue{
			{Type: oidC, Value: "NZ"},
			{Type: oidO, Value: "Acme, Ltd."},
			{Type: oidCN, Value: "client"},
		}},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if got, want := Subject(c), `CN=client,O=Acme\, Ltd.,C=NZ`; got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
}

// TestURISANsExact checks that URI SANs are read exactly as the
// certificate holds them, in order, other GeneralNames skipped, and that
// an extension encoding/asn1 cannot read falls back to the parsed URIs.
func TestURISANsExact(t *testing.T) {
	san := func(names ...asn1.RawValue) []x509.Certificate {
		der, err := asn1.Marshal(names)
		if err != nil {
			t.Fatal(err)
		}
		return []x509.Certificate{{Extensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 17}, Value: der}}}}
	}
	general := func(tag int, s string) asn1.RawValue {
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: tag, Bytes: []byte(s)}
	}
	c := san(general(2, "dns.example"), general(6, "HTTPS://Example.org/x"), general(6, spiffe))[0]
	if got, want := uriSANs(&c), []string{"HTTPS://Example.org/x", spiffe}; !slices.Equal(got, want) {
		t.Errorf("uriSANs = %q, want %q", got, want)
	}

	root := newCA(t, "Root")
	issued := client(t, root, "client-a", time.Time{}, time.Time{}, spiffe, "https://workload.example/a")
	if got, want := uriSANs(issued), []string{spiffe, "https://workload.example/a"}; !slices.Equal(got, want) {
		t.Errorf("uriSANs of an issued leaf = %q, want %q", got, want)
	}

	u, err := url.Parse(spiffe)
	if err != nil {
		t.Fatal(err)
	}
	broken := x509.Certificate{
		Extensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 17}, Value: []byte{0x30, 0x05}}},
		URIs:       []*url.URL{u},
	}
	if got := uriSANs(&broken); !slices.Equal(got, []string{spiffe}) {
		t.Errorf("uriSANs of an unreadable extension = %q, want the parsed URIs", got)
	}
	if got := uriSANs(&x509.Certificate{}); got != nil {
		t.Errorf("uriSANs without an extension = %q, want none", got)
	}
}
