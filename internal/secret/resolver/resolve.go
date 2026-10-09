// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// errCanceled is the reason of every use left unresolved by a canceled
// Resolve.
var errCanceled = errors.New("secret resolution canceled")

// Resolve resolves every use before a Revision is activated (spec 01
// requirement 44). It returns a Store holding every use, or a nil Store
// and one RZ-CFG-026 diagnostic per failing use: all failing uses are
// reported, each naming the provider, reference and key-aware path, never
// a value. A reference the active Store holds keeps its value and cell
// (no re-read across Hot Reloads; spec 01 requirement 46), so rotations
// reach both Stores; it is still checked against every new use. A file
// reference whose last poll failed also keeps its cell, but its file is
// read again: a current value the new uses accept becomes a pending value
// of the new Store, which Activate applies to the shared cell (see retry
// and Resolver.settleLocked). Other references are read now. Resolve is
// serialized with Activate and the poller.
func (r *Resolver) Resolve(ctx context.Context, uses []secret.Use) (secret.Store, diag.List) {
	byRef := map[secret.Ref][]secret.Use{}
	for _, u := range uses {
		byRef[u.Ref] = append(byRef[u.Ref], u)
	}
	refs := slices.SortedFunc(maps.Keys(byRef), compareRefs)

	r.mu.Lock()
	defer r.mu.Unlock()
	rs := &resolution{r: r, byRef: byRef, now: r.clock.Now()}
	defer rs.close()
	active := r.active.Load()
	st := &store{
		res:     r,
		cells:   make(map[secret.Ref]*cell, len(refs)),
		uses:    make(map[secret.Ref][]useCheck, len(refs)),
		refs:    refs,
		checked: map[secret.Ref]uint64{},
	}
	files := map[string][]secret.Ref{} // clean name -> references
	reads := map[string]*pendingRead{} // clean name -> references to read now
	for _, ref := range refs {
		for _, u := range byRef[ref] {
			st.uses[ref] = append(st.uses[ref], useCheck{kind: u.Kind, check: u.Check})
		}
		if ctx.Err() != nil {
			rs.failRef(ref, errCanceled)
			continue
		}
		if c, ok := active.cells[ref]; ok {
			st.cells[ref] = c
			if ref.Provider == providerFile {
				name, _ := cleanName(ref.Name)
				files[name] = append(files[name], ref)
				st.checked[ref] = c.load().version
				if c.failed {
					p := pendingOf(reads, name)
					p.stale = append(p.stale, c)
					continue
				}
			}
			rs.checkReused(c)
			continue
		}
		switch ref.Provider {
		case providerEnv:
			b, err := r.readEnv(ref)
			if err != nil {
				rs.failRef(ref, err)
				continue
			}
			if c := rs.newCell(ref, b); c != nil {
				st.cells[ref] = c
			}
			clear(b)
		case providerFile:
			name, err := cleanName(ref.Name)
			if err != nil {
				rs.failRef(ref, err)
				continue
			}
			files[name] = append(files[name], ref)
			p := pendingOf(reads, name)
			p.fresh = append(p.fresh, ref)
		case providerKubernetes, providerVault:
			rs.failRef(ref, errPlanned)
		default:
			rs.failRef(ref, errProvider)
		}
	}
	pending := map[secret.Ref]*pendingValue{}
	for _, name := range slices.Sorted(maps.Keys(reads)) {
		rs.readGroup(ctx, name, reads[name], st, pending)
	}
	if rs.diags.HasErrors() {
		rs.diags.Sort()
		return nil, rs.diags
	}
	if len(pending) > 0 {
		st.pending.Store(&pending)
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		g := fileGroup{name: name, refs: files[name]}
		for _, ref := range g.refs {
			for _, u := range st.uses[ref] {
				g.limit = max(g.limit, sizeCap(u.kind))
			}
		}
		st.files = append(st.files, g)
	}
	return st, nil
}

// ResolveWithRetry calls Resolve until every use resolves or ctx is done,
// sleeping between attempts with a delay doubling from RetryInitial to
// RetryMax: a Node cold-starting from Last-Known-Good stays not ready,
// retrying with backoff, until every secretRef resolves (CM "secretRef";
// spec 06 requirement 87 rule 4). report, when not nil, receives each
// failed attempt's number (from 1), diagnostics and the delay before the
// next attempt. When ctx ends first it returns a nil Store, the last
// diagnostics and ctx.Err().
func (r *Resolver) ResolveWithRetry(ctx context.Context, uses []secret.Use, report func(attempt int, diags diag.List, next time.Duration)) (secret.Store, diag.List, error) {
	delay := r.retryInitial
	for attempt := 1; ; attempt++ {
		st, diags := r.Resolve(ctx, uses)
		if st != nil {
			return st, nil, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, diags, err
		}
		if report != nil {
			report(attempt, diags, delay)
		}
		if err := r.clock.Sleep(ctx, delay); err != nil {
			return nil, diags, err
		}
		delay = min(2*delay, r.retryMax)
	}
}

// resolution is the working state of one Resolve call, used under
// Resolver.mu.
type resolution struct {
	r     *Resolver
	byRef map[secret.Ref][]secret.Use
	now   time.Time
	diags diag.List

	root         *os.Root // opened on the first file read
	rootErr      error
	resolvedRoot string
	rootOpened   bool
}

// close releases the root handle.
func (rs *resolution) close() {
	if rs.root != nil {
		_ = rs.root.Close()
	}
}

// openRoot opens the secret root once per Resolve.
func (rs *resolution) openRoot() (*os.Root, error) {
	if !rs.rootOpened {
		rs.rootOpened = true
		rs.root, rs.rootErr = rs.r.openRoot()
		rs.resolvedRoot = rs.r.resolvedRoot()
	}
	return rs.root, rs.rootErr
}

// failRef reports reason for every use of ref.
func (rs *resolution) failRef(ref secret.Ref, reason error) {
	for _, u := range rs.byRef[ref] {
		rs.fail(u, reason)
	}
}

// fail reports one failing use.
func (rs *resolution) fail(u secret.Use, reason error) {
	d := diag.Diagnostic{
		Code:     CodeUnresolvable,
		Severity: diag.SeverityError,
		Location: u.Loc,
		Path:     slices.Clone(u.Path),
		Message:  "secretRef " + u.Ref.String() + ": " + reason.Error(),
	}
	if u.Resource != (diag.ResourceID{}) {
		res := u.Resource
		d.Resource = &res
	}
	if errors.Is(reason, errOutsideRoot) || errors.Is(reason, errEscapes) || errors.Is(reason, errRootOpen) {
		d.Hint = SettingSecretRoot + " is " + rs.r.root
	}
	rs.diags = append(rs.diags, d)
}

// checkUses runs the check of every use of ref on b and reports each
// failing use; it returns true when all pass.
func (rs *resolution) checkUses(ref secret.Ref, b []byte) bool {
	ok := true
	for _, u := range rs.byRef[ref] {
		if err := runCheck(useCheck{kind: u.Kind, check: u.Check}, b); err != nil {
			rs.fail(u, err)
			ok = false
		}
	}
	return ok
}

// checkReused checks the current value of an active cell against the new
// uses of its reference.
func (rs *resolution) checkReused(c *cell) {
	b := c.load().val.Reveal()
	rs.checkUses(c.ref, b)
	clear(b)
}

// newCell checks b against the uses of ref and returns a new cell holding
// it, or nil when a check failed.
func (rs *resolution) newCell(ref secret.Ref, b []byte) *cell {
	if !rs.checkUses(ref, b) {
		return nil
	}
	return newCell(ref, b, rs.r.nextVersionLocked())
}

// pendingRead is the file references of one Resolve call that read one
// file now: fresh references the active Store does not hold, and stale
// cells the active Store holds whose last poll failed.
type pendingRead struct {
	fresh []secret.Ref
	stale []*cell
}

// pendingOf returns the pending read of name in m, adding it when absent.
func pendingOf(m map[string]*pendingRead, name string) *pendingRead {
	p, ok := m[name]
	if !ok {
		p = &pendingRead{}
		m[name] = p
	}
	return p
}

// readGroup reads the file name once for the references of p and adds
// them to st: each fresh reference whose value passes its uses in a new
// cell (failing uses are reported), and each stale reference in its shared
// cell, with the pending value retry finds added to pending. A stale
// reference whose file cannot be read is checked on its last good value.
func (rs *resolution) readGroup(ctx context.Context, name string, p *pendingRead, st *store, pending map[secret.Ref]*pendingValue) {
	if ctx.Err() != nil {
		for _, ref := range p.fresh {
			rs.failRef(ref, errCanceled)
		}
		for _, c := range p.stale {
			rs.failRef(c.ref, errCanceled)
		}
		return
	}
	var limit int64
	for _, ref := range p.fresh {
		limit = max(limit, rs.sizeCapOf(ref))
	}
	for _, c := range p.stale {
		limit = max(limit, rs.sizeCapOf(c.ref))
	}
	content, fp, err := rs.read(name, limit)
	if err != nil {
		for _, ref := range p.fresh {
			rs.failRef(ref, err)
		}
		for _, c := range p.stale {
			rs.checkReused(c)
		}
		return
	}
	defer clear(content)
	for _, ref := range p.fresh {
		val, owned, err := valueOf(content, ref.Key)
		if err != nil {
			rs.failRef(ref, err)
			continue
		}
		if c := rs.newCell(ref, val); c != nil {
			c.examined(fp, rs.now)
			st.cells[ref] = c
			st.checked[ref] = c.load().version
		}
		if owned {
			clear(val)
		}
	}
	for _, c := range p.stale {
		if pv := rs.retry(c, content, fp); pv != nil {
			pending[c.ref] = pv
			st.checked[c.ref] = pv.cv.version
		}
	}
}

// retry re-examines a stale cell, whose last poll failed because the
// active Store's uses refused the file's value (or the file could not be
// read), against the new uses. When the value read now differs from the
// cell's and passes every new use, it is returned as a pending value: the
// refused rotation, which the new Store's Get returns until it is
// activated and which Activate then applies to the shared cell, so every
// Store sharing it follows (spec 01 requirement 46, spec 06 requirements
// 13 and 76; the topology fit of spec 08 requirement 7; R-55). The cell is
// never replaced: a Store kept by a carried-over owner or a retired
// snapshot keeps seeing current values. Otherwise the cell's last good
// value is checked against the new uses, as for any reused cell, and
// retry returns nil; Activate then has the next poll examine the file
// again.
func (rs *resolution) retry(c *cell, content []byte, fp fingerprint) *pendingValue {
	val, owned, err := valueOf(content, c.ref.Key)
	if err == nil {
		if owned {
			defer clear(val)
		}
		base := c.load()
		cur := base.val.Reveal()
		changed := !bytes.Equal(cur, val)
		clear(cur)
		if changed && rs.passes(c.ref, val) {
			return &pendingValue{
				cv:     &cellValue{val: secret.NewValue(val), version: rs.r.nextVersionLocked()},
				base:   base.version,
				baseFP: c.fp,
				fp:     fp,
				at:     rs.now,
			}
		}
	}
	rs.checkReused(c)
	return nil
}

// passes reports whether b passes the check of every use of ref; it
// reports nothing.
func (rs *resolution) passes(ref secret.Ref, b []byte) bool {
	for _, u := range rs.byRef[ref] {
		if runCheck(useCheck{kind: u.Kind, check: u.Check}, b) != nil {
			return false
		}
	}
	return true
}

// sizeCapOf returns the largest size cap of the uses of ref.
func (rs *resolution) sizeCapOf(ref secret.Ref) int64 {
	var limit int64
	for _, u := range rs.byRef[ref] {
		limit = max(limit, sizeCap(u.Kind))
	}
	return limit
}

// read reads the file name under the secret root.
func (rs *resolution) read(name string, limit int64) ([]byte, fingerprint, error) {
	root, err := rs.openRoot()
	if err != nil {
		return nil, fingerprint{}, err
	}
	rel, err := rs.r.relative(name, rs.resolvedRoot)
	if err != nil {
		return nil, fingerprint{}, err
	}
	return rs.r.readFile(root, rel, limit)
}

// valueOf returns the value of a file reference: the exact file bytes
// without key (aliasing content, owned false), else the key member's
// string (a new buffer, owned true).
func valueOf(content []byte, key string) (val []byte, owned bool, err error) {
	if key == "" {
		return content, false, nil
	}
	val, err = jsonMember(content, key)
	return val, err == nil, err
}
