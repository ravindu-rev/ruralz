// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// overrides is one read of a --secret-overrides file
// (docs/reference/01-cli-and-api-surface.md "Local ruralzd launched by the
// CLI", Secrets): a YAML or JSON object, read under the restricted YAML
// profile, whose keys are secretRef provider:name pairs and whose values
// are local file paths relative to the file.
type overrides struct {
	// file is the file as given.
	file    string
	entries []*override
}

// override is one entry of a --secret-overrides file.
type override struct {
	// key is the entry's key, provider:name.
	key            string
	provider, name string
	// src is the local file, absolute or relative to the working
	// directory; dst its copy under the private secret root.
	src, dst string
	// used reports that a secretRef of the render names the entry.
	used bool
}

// errOverrides marks a --secret-overrides file that cannot be used.
var errOverrides = errors.New("unusable secret overrides")

// readOverrides reads and checks the --secret-overrides file: it must
// parse under the restricted profile and hold one object whose members
// map a provider:name key, both parts non-empty, to a non-empty path
// string; every path is resolved against the file's directory and must
// name a readable regular file. root is the private secret root the
// copies go to. A failure exits 2, any parse diagnostics in its message.
func readOverrides(ctx context.Context, file, root string) (*overrides, error) {
	fail := func(format string, args ...any) error {
		return command.NoResultf("--secret-overrides %s: %w: %s", file, errOverrides, fmt.Sprintf(format, args...))
	}
	data, err := os.ReadFile(file) //nolint:gosec // G304: the overrides file the user names on the command line.
	if err != nil {
		return nil, command.NoResultf("--secret-overrides %s: %w", file, err)
	}
	format, ok := profile.FormatOf(file)
	if !ok {
		format = profile.FormatYAML
	}
	docs, ds, err := profile.Parse(ctx, data, 0, file, format, profile.Options{})
	if err != nil {
		return nil, err
	}
	if len(ds) > 0 {
		return nil, fail("%s", diagnosticsText(ds))
	}
	if len(docs) != 1 {
		return nil, fail("want one object of provider:name keys and file paths, found %d documents", len(docs))
	}
	o := &overrides{file: file}
	dir := filepath.Dir(file)
	for _, m := range docs[0].Root.Members {
		provider, name, ok := strings.Cut(m.Key, ":")
		if !ok || provider == "" || name == "" {
			return nil, fail("key %q is not a secretRef provider:name pair", m.Key)
		}
		if m.Value.Kind != tree.KindString || m.Value.Text == "" {
			return nil, fail("the value of %q is not a file path", m.Key)
		}
		src := m.Value.Text
		if !filepath.IsAbs(src) {
			src = filepath.Join(dir, src)
		}
		if err := checkRegular(src); err != nil {
			return nil, fail("the file of %q: %v", m.Key, err)
		}
		dst, err := secretPath(root, provider, name)
		if err != nil {
			return nil, fail("%q: %v", m.Key, err)
		}
		o.entries = append(o.entries, &override{key: m.Key, provider: provider, name: name, src: src, dst: dst})
	}
	return o, nil
}

// CheckOverrides reads and checks the --secret-overrides file as a render
// does, exit 2 when it cannot be used: ruralz dev run checks it before the
// first render, and a later render that finds it unusable keeps the
// active Revision.
func CheckOverrides(ctx context.Context, file string) error {
	_, err := readOverrides(ctx, file, os.TempDir())
	return err
}

// OverrideFiles returns the files ruralz dev run watches for the
// --secret-overrides file: the file and every file it names; the file
// alone when it cannot be read or used.
func OverrideFiles(ctx context.Context, file string) []string {
	o, err := readOverrides(ctx, file, os.TempDir())
	if err != nil {
		return []string{file}
	}
	out := []string{o.file}
	for _, e := range o.entries {
		out = append(out, e.src)
	}
	return out
}

// use returns the copy that replaces the secretRef provider:name and marks
// its entry used; ok is false without an entry.
func (o *overrides) use(provider, name string) (string, bool) {
	for _, e := range o.entries {
		if e.provider == provider && e.name == name {
			e.used = true
			return e.dst, true
		}
	}
	return "", false
}

// unused returns a warning per entry no secretRef of the render named.
func (o *overrides) unused() []string {
	var out []string
	for _, e := range o.entries {
		if !e.used {
			out = append(out, fmt.Sprintf("warning: --secret-overrides %s: %s overrides no secretRef", o.file, e.key))
		}
	}
	return out
}

// copies returns the files to copy for the used entries.
func (o *overrides) copies() []secretCopy {
	var out []secretCopy
	for _, e := range o.entries {
		if e.used {
			out = append(out, secretCopy{src: e.src, dst: e.dst})
		}
	}
	return out
}

// secretCopy is one secret file copied into the private secret root.
type secretCopy struct {
	src, dst string
}

// copySecrets copies each file, mode 0600, into its place under the
// private secret root, replacing an earlier copy atomically so ruralzd's
// file watch never reads a part.
func copySecrets(cs []secretCopy) error {
	for _, c := range cs {
		data, err := readSecretFile(c.src)
		if err != nil {
			return command.NoResultf("copy secret file %s: %w", c.src, err)
		}
		if err := os.MkdirAll(filepath.Dir(c.dst), 0o700); err != nil {
			return command.NoResultf("copy secret file %s: %w", c.src, err)
		}
		err = writeAtomic(c.dst, data)
		clear(data)
		if err != nil {
			return command.NoResultf("copy secret file %s: %w", c.src, err)
		}
	}
	return nil
}

// readSecretFile reads a regular file named by a secret override.
func readSecretFile(path string) ([]byte, error) {
	if err := checkRegular(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // G304: a secret file the user's --secret-overrides names.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// checkRegular reports why path is no readable regular file, or nil.
func checkRegular(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}

// secretPath maps a reference to its copy under root: the provider, then
// the name as a relative path, so every reference of one provider and name
// shares a file and no name leaves root.
func secretPath(root, provider, name string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(strings.TrimLeft(name, `/\`)))
	if strings.ContainsAny(provider, `/\.`) || rel == "." || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("cannot place secretRef %s:%s under the private secret root", provider, name)
	}
	return filepath.Join(root, provider, rel), nil
}

// diagnosticsText joins the text form of the first diagnostics of ds.
func diagnosticsText(ds diag.List) string {
	var parts []string
	for i, d := range ds {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(ds)-3))
			break
		}
		parts = append(parts, string(d.AppendText(nil)))
	}
	return strings.Join(parts, "; ")
}
