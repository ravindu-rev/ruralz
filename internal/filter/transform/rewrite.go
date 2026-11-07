// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"net/http"

	"github.com/ravindu-rev/ruralz/internal/filter"
)

// The body rules of a response another Policy rewrites the way the
// transform Policies do, as auth.jwt-sign writes its tokens into the
// buffered Upstream response (docs/architecture/08-security-and-identity.md
// "JWT signing"; docs/architecture/03-data-plane.md "Transform Policies",
// "Absent and empty bodies", "Validators").

// rewriteFold is the canonical keys MarkRewritten writes, which it folds
// first.
func rewriteFold() []string { return []string{keyContentType, keyETag, keyAcceptRanges} }

// BodyRewritable reports whether the body of the response message m of x
// may be rewritten: not on a response that cannot have content, to a HEAD
// request or with status 1xx, 204 or 304, nor on a 206 or a response with
// Content-Range, whose body is never rewritten.
func BodyRewritable(x filter.Exchange, m filter.Message) bool {
	return bodyAllowed(x, m, m.Header())
}

// JSONContent reports whether h has exactly one Content-Type field line
// and it names JSON, application/json or a +json type: the rule by which
// CEL reads a body as JSON and a body operation edits it.
func JSONContent(h http.Header) bool { return jsonContent(h) }

// MarkRewritten applies to h, the header of a response whose body a Policy
// rewrote, what the transform Policies apply: each strong ETag turns weak,
// prefixed W/, and Accept-Ranges is removed, as the bytes no longer equal
// the Upstream's; with created, a body written where there was none takes
// Content-Type application/json. The lines h holds for these fields under
// keys that differ only in case are folded into the canonical key first.
// Content-Length, Content-Encoding and the body digests are
// filter.Message.SetBody's.
func MarkRewritten(h http.Header, created bool) {
	fold := rewriteFold()
	var lens uint64
	for _, k := range fold {
		lens |= lenBit(len(k))
	}
	if hasVariant(h, fold, lens) {
		foldVariants(h, fold)
	}
	if created {
		h[keyContentType] = []string{DefaultContentType}
	}
	weakenETag(h)
	delete(h, keyAcceptRanges)
}
