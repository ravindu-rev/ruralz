// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package geoip is the authz.geoip Policy type: it allows or refuses a
// request in onRequestHeaders by the ISO 3166-1 alpha-2 country of
// source.ip, which a local MaxMind-format database places
// (docs/architecture/08-security-and-identity.md "IP filtering and
// GeoIP"). The offline check every binary runs is package check; only
// ruralzd links this package, which links
// github.com/oschwald/maxminddb-golang.
//
// # Lists
//
// config.allow and config.deny are sets of two upper-case letters, the
// schema's CountryCode. Both lists empty (absent or written as empty
// lists), and a code of another form, which the schema refuses, are
// RZ-CFG-005 at Build.
//
// # Decision
//
// The address is source.ip after trusted-proxy and PROXY v2 handling
// (filter.Exchange.Source, Security and identity "Client address"), the
// one authz.ip filters on. A deny match refuses; otherwise a non-empty
// allow list refuses every address outside it, and an empty one admits.
// An address the database cannot place matches neither list: one without
// a record or whose record has no country.iso_code of two upper-case
// letters, which covers private and reserved ranges, and an IPv6 address
// in an IPv4-only database. An IPv4-mapped IPv6 address is placed as
// IPv4. A refusal is 403 RZ-AUTH-014, counted in
// ruralz_auth_decisions_total with its code, as an admit is without one.
// A request without a valid client address, a Node without a valid
// database, and a lookup that fails cannot decide and fail closed with
// the authz class default, 403 RZ-AUTH-015 (authentication and
// authorization fail closed, Security and identity "Threat model and
// trust boundaries").
//
// # Database
//
// Database is the Node-wide filter.Component of RURALZ_GEOIP_DATABASE_FILE,
// which internal/filter/builtin constructs once per process. It reads the
// file into memory at start, through OpenBytes, so a rewrite of the file
// in place can never change the database a request uses, and polls the
// file every PollInterval as the file-mode configuration watcher polls
// its source: a replaced file (a rename into place, a new symbolic-link
// target, a file that appears) is read at once, any other change to its
// size, modification time or mode once a poll sees no further change, a
// file that changes while it is read is read again, and a read too close
// to the file's modification time to trust its stamp is checked by the
// next poll. A file is valid when it parses as a MaxMind DB of ip_version
// 4 or 6, its whole search tree walks, every country.iso_code is a string
// and at least one is two upper-case letters, the GeoIP2 and GeoLite2
// Country and City layout; the load decodes each record's country once, so
// a lookup allocates nothing. A new valid file replaces the database
// atomically; requests in flight finish on the one they read. A file that
// cannot be read or is not valid keeps the last valid database, if any,
// and raises geoip_database_stale until a valid file is read; the reason
// is also raised while an authz.geoip Filter is live and no database
// serves, whether the setting is unset or no read succeeded.
//
// A Filter is immutable apart from its database pointer and safe for
// concurrent use; Close ends its count as a live user of the database.
package geoip
