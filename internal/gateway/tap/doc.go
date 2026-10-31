// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package tap is the /tap stream of the Ruralz Gateway admin server
// (docs/architecture/03-data-plane.md "Admin endpoints"): sampled,
// redacted request and response metadata, one NDJSON line per exchange,
// for ruralz dev tap.
//
// # Publishing
//
// The request handler reports each finished exchange to a Hub. Active is
// one atomic load, so /tap costs nothing more while nobody subscribes
// (docs/architecture/10-observability.md, rule O7 and "Overhead per
// signal"); the handler fills an Event only when Active reports true and
// passes it to Publish. Publish draws each subscriber's sampling decision
// first and encodes the event once, as one line, only when a subscriber
// takes it; it then copies the line into the buffer of every subscriber
// that sampled it. Publish never blocks on a subscriber and allocates
// nothing once its encode buffers are warm.
//
// # Redaction
//
// A line carries the adminapi.TapEvent members in their documented order.
// Header names are written in lowercase, and every value of a header the
// snapshot's redact.Set redacts (its credential headers, and every name
// containing token, secret, key, password, session or auth) reads
// "[REDACTED]". The path loses any query string or fragment, and bodies
// never appear (rule O4).
//
// # Subscribing
//
// Hub serves GET /tap as application/x-ndjson. At most MaxSubscribers
// subscribe at once; another gets 503 RZ-RT-019. Each subscriber samples
// exchanges at its own ?sample= rate in (0, 1], default 1 (any other value
// is 400 RZ-RT-022), and holds one BufferBytes ring buffer that its own
// request goroutine drains to the client. When a slow subscriber's buffer
// has no room for a line, the line is dropped for that subscriber and
// counted by ruralz_tap_events_dropped_total, and the subscriber gets one
// {"dropped":N} line before its next event. Every write and flush to a
// subscriber has a WriteTimeout (10 s) deadline, cleared between batches
// so an idle subscription stays; a subscriber that misses it, having
// stopped reading, is dropped and its slot freed. A subscription also ends
// when its request context ends: the client leaves, or the admin server
// shuts down.
package tap
