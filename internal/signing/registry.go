// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// Registry bounds (target values of docs/adr/0017-artifact-signing.md
// "Bounds" and docs/architecture/04-control-plane-and-gitops.md "Building
// and recording a Revision").
const (
	// MaxArtifactBytes bounds one fetched artifact: a Revision layer or a
	// Plugin artifact.
	MaxArtifactBytes = 64 << 20
	// FetchTimeout bounds one fetch, its signature check included.
	FetchTimeout = 30 * time.Second
	// MaxManifestBytes bounds a manifest, a tags list page and a referrers
	// page: the OCI client's metadata bound.
	MaxManifestBytes = 4 << 20
	// maxRedirects bounds the redirects one request follows.
	maxRedirects = 10
	// maxListPages bounds the pages of one tags or referrers listing, so a
	// registry cannot page without end.
	maxListPages = 64
	// dialTimeout and headerTimeout bound one connect and the wait for a
	// response's headers: socket deadlines the Go runtime enforces.
	dialTimeout   = 10 * time.Second
	headerTimeout = 30 * time.Second
	// maxResponseHeaderBytes caps a response's header block.
	maxResponseHeaderBytes = 64 << 10
)

// Media types of OCI content.
const (
	// MediaTypeImageManifest is the OCI image manifest.
	MediaTypeImageManifest = ocispec.MediaTypeImageManifest
	// MediaTypeEmpty is the OCI empty descriptor's media type: the config
	// of an artifact that has none.
	MediaTypeEmpty = ocispec.MediaTypeEmptyJSON
)

// Errors of registry calls; each also carries the registry's answer.
var (
	// ErrNotFound reports a repository, tag, manifest or blob the registry
	// does not hold.
	ErrNotFound = errors.New("signing: the registry holds no such repository, tag, manifest or blob")
	// ErrDenied reports a registry that refused the request's credentials,
	// or answered 401 or 403 to an anonymous one.
	ErrDenied = errors.New("signing: the registry refused the credentials")
	// ErrUnavailable reports a registry that could not answer: a network
	// or TLS failure, a 5xx or 429 answer, a timeout. A caller retries.
	ErrUnavailable = errors.New("signing: the registry is unavailable")
	// ErrInvalidManifest reports a manifest that does not decode as the
	// media type it was served as.
	ErrInvalidManifest = errors.New("signing: not a valid manifest")
	// ErrTimeout reports a call that ran past its deadline: transient.
	ErrTimeout = errors.New("signing: deadline passed")
	// errStopListing ends a listing once it has what it needs.
	errStopListing = errors.New("signing: listing complete")
)

// Descriptor is an OCI content descriptor with a sha256 digest.
type Descriptor struct {
	// MediaType is the content's media type.
	MediaType string
	// Digest is the content's sha256 digest.
	Digest revision.Digest
	// Size is the content's length in bytes.
	Size int64
	// ArtifactType is the artifact type of a manifest, when it has one.
	ArtifactType string
	// Annotations are the descriptor's annotations.
	Annotations map[string]string
}

// oci converts d to the OCI type.
func (d Descriptor) oci() ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType: d.MediaType, Digest: digest.Digest(d.Digest.String()), Size: d.Size,
		ArtifactType: d.ArtifactType, Annotations: d.Annotations,
	}
}

// fromOCI converts an OCI descriptor, refusing every digest but a full
// sha256.
func fromOCI(d ocispec.Descriptor) (Descriptor, error) {
	dg, err := ParseDigest(d.Digest.String())
	if err != nil {
		return Descriptor{}, fmt.Errorf("signing: descriptor digest: %w", err)
	}
	if d.Size < 0 {
		return Descriptor{}, fmt.Errorf("signing: descriptor size %d", d.Size)
	}
	return Descriptor{MediaType: d.MediaType, Digest: dg, Size: d.Size, ArtifactType: d.ArtifactType, Annotations: d.Annotations}, nil
}

// RegistryOptions configure a Repository.
type RegistryOptions struct {
	// Credentials resolve the credential each registry host gets; nil is
	// anonymous.
	Credentials *Credentials
	// RootCAs verify registry certificates; nil uses the system roots
	// (which SSL_CERT_FILE and SSL_CERT_DIR extend on Linux).
	RootCAs *x509.CertPool
	// Proxy picks a proxy per request; nil connects directly.
	Proxy func(*http.Request) (*url.URL, error)
	// DialContext dials connections, such as an egress guard's dialer;
	// nil uses a net.Dialer with a 10 s connect timeout.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// UserAgent is sent with every request; empty sends Go's default.
	UserAgent string
	// Clock arms the deadlines of the calls that take a timeout; nil
	// means clock.Real().
	Clock clock.Clock
}

// Repository is one OCI repository of one registry, reached over TLS 1.2
// or newer with the server certificate verified; there is no cleartext
// registry connection. A credential is sent only to its registry host:
// the client drops Authorization once a redirect leaves the registry's
// origin, follows at most 10 redirects and never from https to http.
// Repository is safe for concurrent use.
type Repository struct {
	ref    Reference
	client *auth.Client
	repo   *remote.Repository
	clk    clock.Clock
}

// NewRepository returns the repository ref names; ref's tag and digest
// are ignored.
func NewRepository(ref Reference, o RegistryOptions) (*Repository, error) {
	if err := checkRegistry(ref.Registry); err != nil {
		return nil, err
	}
	if err := checkRepository(ref.Repository); err != nil {
		return nil, err
	}
	dial := o.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	transport := &http.Transport{
		Proxy:       o.Proxy,
		DialContext: dial,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    o.RootCAs,
		},
		TLSHandshakeTimeout:    dialTimeout,
		ResponseHeaderTimeout:  headerTimeout,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		ForceAttemptHTTP2:      true,
		MaxIdleConnsPerHost:    4,
		IdleConnTimeout:        90 * time.Second,
	}
	httpClient := &http.Client{Transport: transport, CheckRedirect: checkRedirect}
	creds := o.Credentials
	client := &auth.Client{
		Client: httpClient,
		Cache:  auth.NewCache(),
		Credential: func(ctx context.Context, hostport string) (auth.Credential, error) {
			c, err := creds.Lookup(ctx, hostport)
			if err != nil {
				return auth.EmptyCredential, err
			}
			return auth.Credential{
				Username: c.Username, Password: c.Password,
				RefreshToken: c.IdentityToken, AccessToken: c.RegistryToken,
			}, nil
		},
	}
	if o.UserAgent != "" {
		client.SetUserAgent(o.UserAgent)
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.Real()
	}
	return &Repository{
		ref:    Reference{Registry: ref.Registry, Repository: ref.Repository},
		client: client,
		repo: &remote.Repository{
			Client:               client,
			Reference:            registry.Reference{Registry: ref.Registry, Repository: ref.Repository},
			MaxMetadataBytes:     MaxManifestBytes,
			SkipReferrersGC:      true,
			TagListMaxPages:      maxListPages,
			ReferrerListMaxPages: maxListPages,
		},
		clk: clk,
	}, nil
}

// checkRedirect follows at most maxRedirects redirects and never one from
// https to http; the auth client drops credentials once a redirect leaves
// the registry's origin.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("signing: stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("signing: refusing a redirect to %s, which is not https", req.URL.Redacted())
	}
	return nil
}

// Reference returns the repository's registry and repository.
func (r *Repository) Reference() Reference { return r.ref }

// Resolve returns the descriptor of the manifest tag names.
func (r *Repository) Resolve(ctx context.Context, tag string) (Descriptor, error) {
	if err := checkTag(tag); err != nil {
		return Descriptor{}, err
	}
	d, err := r.repo.Resolve(ctx, tag)
	if err != nil {
		return Descriptor{}, r.classify("resolve "+r.ref.Name()+":"+tag, err)
	}
	out, err := fromOCI(d)
	if err != nil {
		return Descriptor{}, fmt.Errorf("%w: resolve %s:%s: %w", ErrInvalidManifest, r.ref.Name(), tag, err)
	}
	return out, nil
}

// manifestAccept is the Accept header of a manifest fetched by digest
// whose media type the caller does not know.
const manifestAccept = ocispec.MediaTypeImageManifest + ", " + ocispec.MediaTypeImageIndex + ", " +
	"application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json"

// FetchManifest fetches the manifest d names by digest and returns its
// bytes, which hash to d.Digest (ErrMismatch otherwise) and hold at most
// MaxManifestBytes (ErrTooLarge).
func (r *Repository) FetchManifest(ctx context.Context, d Descriptor) ([]byte, error) {
	accept := d.MediaType
	if accept == "" {
		accept = manifestAccept
	}
	return r.fetch(ctx, "manifests", d, accept, MaxManifestBytes)
}

// FetchBlob fetches the blob d names by digest: at most limit bytes
// (ErrTooLarge, before any byte when d.Size or the response's length
// already says so), hashing to d.Digest (ErrMismatch).
func (r *Repository) FetchBlob(ctx context.Context, d Descriptor, limit int64) ([]byte, error) {
	return r.fetch(ctx, "blobs", d, "", limit)
}

// fetch GETs /v2/<repository>/<kind>/<digest> through the auth client,
// which follows redirects such as a blob store's, and reads the body
// through SHA-256 within limit.
func (r *Repository) fetch(ctx context.Context, kind string, d Descriptor, accept string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, ErrLimit
	}
	what := kind[:len(kind)-1] + " " + d.Digest.Short()
	if d.Digest.IsZero() {
		return nil, fmt.Errorf("%w: %s", ErrNoDigest, kind)
	}
	if d.Size > limit {
		return nil, fmt.Errorf("%w: %s holds %d bytes, more than %d", ErrTooLarge, what, d.Size, limit)
	}
	ref := r.repo.Reference
	ref.Reference = d.Digest.String()
	ctx = auth.AppendRepositoryScope(ctx, ref, auth.ActionPull)
	u := (&url.URL{Scheme: "https", Host: ref.Host(), Path: "/v2/" + ref.Repository + "/" + kind + "/" + d.Digest.String()}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("signing: fetch %s: %w", what, err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, r.classify("fetch "+what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusError(resp, "fetch "+what); err != nil {
		return nil, err
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("%w: %s is %d bytes long, more than %d", ErrTooLarge, what, resp.ContentLength, limit)
	}
	var buf bytes.Buffer
	if err := streamPlain(ctx, resp.Body, d.Digest, limit, &buf); err != nil {
		switch {
		case errors.Is(err, ErrMismatch), errors.Is(err, ErrTooLarge):
			return nil, fmt.Errorf("%w: %s", err, what)
		case ctx.Err() != nil:
			return nil, fmt.Errorf("signing: fetch %s: %w", what, ctx.Err())
		default:
			return nil, fmt.Errorf("%w: fetch %s: %w", ErrUnavailable, what, err)
		}
	}
	if d.Size > 0 && int64(buf.Len()) != d.Size {
		return nil, fmt.Errorf("%w: %s holds %d bytes, its descriptor says %d", ErrMismatch, what, buf.Len(), d.Size)
	}
	return buf.Bytes(), nil
}

// statusError maps a response that is not 200 to ErrNotFound, ErrDenied,
// ErrUnavailable or a plain error, naming op.
func statusError(resp *http.Response, op string) error {
	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		return nil
	case code == http.StatusNotFound:
		return fmt.Errorf("%w: %s: response status %d", ErrNotFound, op, code)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w: %s: response status %d", ErrDenied, op, code)
	case code == http.StatusTooManyRequests || code >= 500:
		return fmt.Errorf("%w: %s: response status %d", ErrUnavailable, op, code)
	default:
		return fmt.Errorf("signing: %s: unexpected response status %d", op, code)
	}
}

// Referrers returns up to limit descriptors of the manifests whose
// subject is subject and whose artifact type is artifactType, in the
// registry's order, through the referrers API or, where the registry has
// none, the referrers tag schema. It stops listing once it has limit.
func (r *Repository) Referrers(ctx context.Context, subject Descriptor, artifactType string, limit int) ([]Descriptor, error) {
	if limit <= 0 {
		return nil, ErrLimit
	}
	var out []Descriptor
	err := r.repo.Referrers(ctx, subject.oci(), artifactType, func(page []ocispec.Descriptor) error {
		for _, d := range page {
			if artifactType != "" && d.ArtifactType != artifactType {
				continue
			}
			desc, err := fromOCI(d)
			if err != nil {
				// A referrer without a full sha256 digest is skipped, never
				// tried.
				continue
			}
			out = append(out, desc)
			if len(out) == limit {
				return errStopListing
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopListing) {
		return nil, r.classify("list referrers of "+subject.Digest.Short(), err)
	}
	return out, nil
}

// Tags returns the repository's tags in the registry's order, at most
// limit (ErrTooLarge past it).
func (r *Repository) Tags(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, ErrLimit
	}
	var out []string
	err := r.repo.Tags(ctx, "", func(page []string) error {
		if len(out)+len(page) > limit {
			return fmt.Errorf("%w: more than %d tags", ErrTooLarge, limit)
		}
		out = append(out, page...)
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return nil, err
		}
		return nil, r.classify("list tags of "+r.ref.Name(), err)
	}
	return out, nil
}

// PushBlob uploads content as a blob of mediaType and returns its
// descriptor; a blob the registry already holds is not sent again.
func (r *Repository) PushBlob(ctx context.Context, mediaType string, content []byte) (Descriptor, error) {
	d := Descriptor{MediaType: mediaType, Digest: revision.Sum(content), Size: int64(len(content))}
	exists, err := r.repo.Blobs().Exists(ctx, d.oci())
	if err != nil {
		return Descriptor{}, r.classify("check blob "+d.Digest.Short(), err)
	}
	if exists {
		return d, nil
	}
	if err := r.repo.Blobs().Push(ctx, d.oci(), bytes.NewReader(content)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return Descriptor{}, r.classify("push blob "+d.Digest.Short(), err)
	}
	return d, nil
}

// PushManifest uploads content as a manifest of mediaType, tagged tag
// unless tag is empty, and returns its descriptor. A manifest with a
// subject is listed among the subject's referrers, through the referrers
// tag schema where the registry has no referrers API.
func (r *Repository) PushManifest(ctx context.Context, mediaType string, content []byte, tag string) (Descriptor, error) {
	if int64(len(content)) > MaxManifestBytes {
		return Descriptor{}, fmt.Errorf("%w: manifest of %d bytes, more than %d", ErrTooLarge, len(content), MaxManifestBytes)
	}
	d := Descriptor{MediaType: mediaType, Digest: revision.Sum(content), Size: int64(len(content))}
	var err error
	if tag == "" {
		err = r.repo.Manifests().Push(ctx, d.oci(), bytes.NewReader(content))
	} else {
		if terr := checkTag(tag); terr != nil {
			return Descriptor{}, terr
		}
		err = r.repo.Manifests().PushReference(ctx, d.oci(), bytes.NewReader(content), tag)
	}
	if err != nil {
		return Descriptor{}, r.classify("push manifest "+d.Digest.Short(), err)
	}
	return d, nil
}

// classify wraps a registry error in ErrNotFound, ErrDenied or
// ErrUnavailable; the context's own errors pass unwrapped.
func (r *Repository) classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("signing: %s: %w", op, err)
	}
	var resp *errcode.ErrorResponse
	switch {
	case errors.As(err, &resp):
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: %s: %w", ErrNotFound, op, err)
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return fmt.Errorf("%w: %s: %w", ErrDenied, op, err)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
		default:
			return fmt.Errorf("signing: %s: %w", op, err)
		}
	case errors.Is(err, auth.ErrBasicCredentialNotFound):
		return fmt.Errorf("%w: %s: the registry asks for credentials and none are configured for %s", ErrDenied, op, r.ref.Registry)
	case errors.Is(err, errdef.ErrNotFound):
		return fmt.Errorf("%w: %s: %w", ErrNotFound, op, err)
	case errors.Is(err, errdef.ErrSizeExceedsLimit):
		return fmt.Errorf("%w: %s: %w", ErrTooLarge, op, err)
	case errors.Is(err, ErrCredentialHelper):
		return fmt.Errorf("signing: %s: %w", op, err)
	default:
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
	}
}

// within runs fn under a deadline of d on the repository's clock; the
// context's cause is ErrTimeout once it passes.
func (r *Repository) within(ctx context.Context, d time.Duration, fn func(ctx context.Context) error) error {
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	t := r.clk.AfterFunc(d, func() { cancel(ErrTimeout) })
	defer t.Stop()
	err := fn(cctx)
	if cause := context.Cause(cctx); errors.Is(cause, ErrTimeout) && ctx.Err() == nil {
		return fmt.Errorf("%w after %v: %w", ErrTimeout, d, err)
	}
	return err
}
