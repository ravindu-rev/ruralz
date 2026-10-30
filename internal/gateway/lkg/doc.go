// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package lkg keeps a Node's Last-Known-Good files under
// ${RURALZ_DATA_DIR}/lkg/ (docs/architecture/03-data-plane.md "Last-Known-Good
// files"; foundation pack section 8.2 and 8.11).
//
// # Files
//
//	sha256-<64 hex>.json      one Revision's exact ruralz.canonical.v1
//	                          bytes, never resolved secrets
//	candidate.json, lkg.json  pointers: {"format":"ruralz.lkg.v1",
//	                          "digest":"sha256:<64 hex>","writtenAt":
//	                          "<RFC 3339 UTC>","version":"<ruralzd version>"}
//
// candidate.json names the Revision the Node last wrote after activating
// it; lkg.json names its Last-Known-Good, the Revision a Node boots when
// it cannot obtain configuration. In file mode a candidate becomes
// Last-Known-Good on activation, so one Write sets both pointers.
//
// # Writes
//
// Only the process holding the data-directory lock writes, through the
// nodedir.Lock it holds, so nothing is written after the Drain released
// the lock. Every file is written by WriteFileAtomic (a temporary file,
// fsync, rename and a directory fsync), content before pointer, so a
// crash keeps the previous pointer valid; then every file no pointer
// names is deleted. A write that fails raises the degraded reason
// lkg_write_failed until a write succeeds; the activation loader retries
// with RetryDelay, from 1 s doubling to 60 s (target).
//
// # Reads
//
// A pointer that is missing, malformed or of an unknown format counts as
// absent (ErrAbsent). Content is read without following a symbolic link
// or blocking on a FIFO, at most one byte past the Node's source limit,
// so the configuration pipeline, which re-hashes it (RZ-CFG-027) and
// re-validates it from the schema stage (RZ-CFG-024 for newer-release
// content, RZ-CFG-001 past the limit), sees content that passes the limit
// as such. A handover successor reads the holder's candidate while the
// holder may write a newer one and delete the older content, so Candidate
// reads the pointer again when the content it named is gone.
//
// AdminPort reads Gateway.spec.admin.port from canonical content: the
// port a Node binds before its first activation when the Bundle it boots
// does not parse (docs/architecture/03-data-plane.md "Admin endpoints").
package lkg
