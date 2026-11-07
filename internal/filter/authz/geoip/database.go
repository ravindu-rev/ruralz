// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package geoip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// ComponentName names the database in logs and shutdown errors.
const ComponentName = "geoip database"

// PollInterval is how often Run polls the database file: the poll of the
// file-mode configuration watcher, 1 s (target)
// (docs/architecture/03-data-plane.md "Sources, Last-Known-Good and
// Drain").
const PollInterval = time.Second

// racyWindow is how close to a read the file's modification time must be
// for the next poll to read the file again although its stamp did not
// change, 2 s (target) (docs/architecture/08-security-and-identity.md "IP
// filtering and GeoIP"): a same-size rewrite within the file system's
// timestamp granularity leaves size, time and identity unchanged.
const racyWindow = 2 * time.Second

// degradedSource is the geoip_database_stale source the database raises.
const degradedSource = "geoip"

// Log messages (static, sloglint).
const (
	msgLoaded     = "geoip database loaded"
	msgRefused    = "geoip database file unusable; the last valid database keeps serving"
	msgRefusedNew = "geoip database file unusable; no database serves, so every authz.geoip Policy fails closed"
	msgChanged    = "geoip database file changed while it was read; the read is discarded"
)

// errNotRegular reports a database path that is not a regular file.
var errNotRegular = errors.New("not a regular file")

// DatabaseOptions configure NewDatabase. Every field may be left zero.
type DatabaseOptions struct {
	// Path is RURALZ_GEOIP_DATABASE_FILE, an absolute path; "" means no
	// database, so every authz.geoip Policy cannot decide.
	Path string
	// Clock times the polls and judges a read's racy window; nil means
	// clock.Real().
	Clock clock.Clock
	// Logger receives loads and unusable files; nil discards them.
	Logger *slog.Logger
	// Status receives geoip_database_stale; nil reports none.
	Status emit.NodeStatus
	// Interval is the poll interval; zero means PollInterval.
	Interval time.Duration
}

// Database is the Node's GeoIP database (filter.Component): the
// MaxMind-format file RURALZ_GEOIP_DATABASE_FILE names, read into memory
// at start and read again when it changes, which every authz.geoip
// Filter places addresses with
// (docs/architecture/08-security-and-identity.md "IP filtering and
// GeoIP"). A file that cannot be read or is refused keeps the last valid
// database serving and raises geoip_database_stale, which also stays
// raised while an authz.geoip Filter is live and no database serves. It
// is safe for concurrent use; the request path only loads an atomic
// pointer.
type Database struct {
	path     string
	clk      clock.Clock
	log      *slog.Logger
	status   emit.NodeStatus
	interval time.Duration

	// cur is the database in use; nil while none was loaded.
	cur atomic.Pointer[countryDB]

	// pollMu serializes polls, which hold it while they read and parse
	// the file, and guards seen, tried and racy.
	pollMu sync.Mutex
	// seen is the stamp of the last poll; tried that of the last read,
	// whatever its outcome.
	seen, tried stamp
	// racy is set when the last read was too close to the file's
	// modification time to trust its stamp.
	racy bool

	// mu guards the degraded state, held briefly so a Build never waits
	// for a poll.
	mu sync.Mutex
	// users counts the live Filters (acquire and release).
	users int
	// failing is set while the last read failed.
	failing bool
	// raised mirrors geoip_database_stale.
	raised bool

	// polled, set by tests before Run starts, runs after each poll of
	// Run; afterRead, set by tests, runs after each read of the file and
	// before its second stamp.
	polled, afterRead func()
}

var _ filter.Component = (*Database)(nil)

// NewDatabase returns the database of o.Path, read at once: a file that
// cannot be read or is refused leaves the Node without a database and
// raises geoip_database_stale, and Run reads it again once it changes. A
// file that changes while it is read is read by Run once it settles.
func NewDatabase(o DatabaseOptions) *Database {
	d := &Database{path: o.Path, clk: o.Clock, log: o.Logger, status: o.Status, interval: o.Interval}
	if d.clk == nil {
		d.clk = clock.Real()
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	if d.interval <= 0 {
		d.interval = PollInterval
	}
	if d.path != "" {
		d.pollMu.Lock()
		d.loadLocked(context.Background(), d.stampFile())
		d.pollMu.Unlock()
	}
	return d
}

// Name returns ComponentName.
func (d *Database) Name() string { return ComponentName }

// Run polls the file every interval until ctx is done, then returns nil:
// a replaced file (a rename into place, a new symbolic-link target, a
// file that appears) is read at once, any other change once a poll sees
// no further change, and a file that changes while it is read is read
// again. Without a path it waits for ctx.
func (d *Database) Run(ctx context.Context) error {
	if d.path == "" {
		<-ctx.Done()
		return nil
	}
	t := d.clk.NewTimer(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C():
		}
		d.poll(ctx)
		if d.polled != nil {
			d.polled()
		}
		t.Reset(d.interval)
	}
}

// current returns the database in use, nil without one; a nil d has none.
func (d *Database) current() *countryDB {
	if d == nil {
		return nil
	}
	return d.cur.Load()
}

// acquire counts a live Filter: without a database it raises
// geoip_database_stale. A nil d does nothing.
func (d *Database) acquire() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.users++
	d.syncDegradedLocked()
}

// release ends a Filter acquire counted. A nil d does nothing.
func (d *Database) release() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.users--
	d.syncDegradedLocked()
}

// poll stamps the file and reads it when a read is due: a change since
// the last read, or a last read inside its racy window. A replaced file
// is read at once and any other change once a poll sees no further
// change.
func (d *Database) poll(ctx context.Context) {
	d.pollMu.Lock()
	defer d.pollMu.Unlock()
	cur := d.stampFile()
	switch {
	case cur.missing:
		d.seen, d.racy = cur, false
		d.fail(ctx, cur, cur.err)
	case cur.equal(d.tried) && !d.racy:
		d.seen = cur
	case cur.equal(d.tried), !cur.sameFile(d.seen), cur.equal(d.seen):
		// A racy read again, a replaced file, or a change that settled.
		d.loadLocked(ctx, cur)
	default:
		// The settle debounce: the next poll that sees no further change
		// reads it.
		d.seen = cur
	}
}

// loadLocked reads the file stamped before and publishes its database,
// unless the file changed during the read: then the read is discarded,
// and done again at once when the file was replaced, else left to the
// poll that sees it settled. Callers hold pollMu.
func (d *Database) loadLocked(ctx context.Context, before stamp) {
	for ctx.Err() == nil {
		if before.missing {
			d.seen, d.racy = before, false
			d.fail(ctx, before, before.err)
			return
		}
		buf, err := readFile(d.path)
		if d.afterRead != nil {
			d.afterRead()
		}
		after := d.stampFile()
		if !after.equal(before) {
			d.log.DebugContext(ctx, msgChanged, catalog.KeyPath, d.path)
			if !after.missing && !after.sameFile(before) {
				before = after
				continue
			}
			d.seen = after
			if after.missing {
				d.racy = false
				d.fail(ctx, after, after.err)
			}
			return
		}
		d.seen, d.racy = before, before.racyAt(d.clk.Now())
		if err != nil {
			d.fail(ctx, before, err)
			return
		}
		d.publish(ctx, before, buf)
		return
	}
}

// publish opens buf, the content of the file in state st, and makes it
// the database in use; a refused file keeps the last valid database.
// Content equal to the database in use changes nothing but the stamp.
// Callers hold pollMu.
func (d *Database) publish(ctx context.Context, st stamp, buf []byte) {
	if db := d.cur.Load(); db == nil || !bytes.Equal(db.buf, buf) {
		db, err := openCountryDB(ctx, buf)
		if err != nil {
			if ctx.Err() == nil {
				d.fail(ctx, st, err)
			}
			return
		}
		d.cur.Store(db)
		d.log.InfoContext(ctx, msgLoaded, catalog.KeyPath, d.path,
			"database_type", db.typ, "build_time", db.built.Format(time.RFC3339), "records", len(db.offsets))
	}
	d.tried = st
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failing = false
	d.syncDegradedLocked()
}

// fail records a failed read of the file in state st: the database in
// use, if any, stays, and geoip_database_stale is raised; the failure is
// logged unless it repeats the last failure of the same state. Callers
// hold pollMu.
func (d *Database) fail(ctx context.Context, st stamp, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	repeated := d.failing && d.tried.equal(st)
	d.tried, d.failing = st, true
	d.syncDegradedLocked()
	if repeated {
		return
	}
	if d.cur.Load() == nil {
		d.log.WarnContext(ctx, msgRefusedNew, catalog.KeyPath, d.path, catalog.KeyError, err.Error())
		return
	}
	d.log.WarnContext(ctx, msgRefused, catalog.KeyPath, d.path, catalog.KeyError, err.Error())
}

// syncDegradedLocked raises geoip_database_stale while the last read
// failed, or while a Filter is live and no database serves, and clears it
// otherwise. Callers hold mu.
func (d *Database) syncDegradedLocked() {
	on := d.failing || d.users > 0 && d.cur.Load() == nil
	if on == d.raised {
		return
	}
	d.raised = on
	if d.status != nil {
		d.status.SetDegraded(catalog.ReasonGeoIPDatabaseStale, degradedSource, on)
	}
}

// stamp is what one poll sees of the file: its identity through
// symbolic links, its own link target, and its size, modification time
// and mode; or why it cannot be stat'ed. The zero stamp sees nothing yet.
type stamp struct {
	missing bool
	err     error
	info    fs.FileInfo
	link    string
}

// stampFile stamps d's path.
func (d *Database) stampFile() stamp {
	var s stamp
	if li, err := os.Lstat(d.path); err == nil && li.Mode()&fs.ModeSymlink != 0 {
		s.link, _ = os.Readlink(d.path)
	}
	info, err := os.Stat(d.path)
	if err != nil {
		return stamp{missing: true, err: err}
	}
	s.info = info
	return s
}

// sameFile reports whether s and o see the same file through the same
// symbolic-link target; two missing stamps are the same when they fail
// alike, and two zero stamps are the same.
func (s stamp) sameFile(o stamp) bool {
	if s.info == nil || o.info == nil {
		return s.info == nil && o.info == nil && s.missing == o.missing && errText(s.err) == errText(o.err)
	}
	return s.link == o.link && os.SameFile(s.info, o.info)
}

// equal reports whether s and o see the same file unchanged.
func (s stamp) equal(o stamp) bool {
	if !s.sameFile(o) {
		return false
	}
	if s.info == nil {
		return true
	}
	return s.info.Size() == o.info.Size() && s.info.ModTime().Equal(o.info.ModTime()) && s.info.Mode() == o.info.Mode()
}

// racyAt reports whether a read at now cannot trust s: the file changed
// within racyWindow of now, or in the future.
func (s stamp) racyAt(now time.Time) bool {
	if s.info == nil {
		return false
	}
	return now.Sub(s.info.ModTime()) < racyWindow
}

// errText returns err's text, "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// readFile reads the regular file at path whole. It opens without
// blocking on unix, so a FIFO or a device at path is refused instead of
// hanging the poll.
func readFile(path string) ([]byte, error) {
	f, err := openNoBlock(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w (mode %s)", errNotRegular, fi.Mode().Type())
	}
	return io.ReadAll(f)
}
