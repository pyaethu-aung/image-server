package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Local is a Storage backed by a directory tree on the local filesystem. All
// access is confined to the root directory through an *os.Root handle, so a
// key can never read or write outside the root even in the presence of
// pre-existing symlinks (the os.Root methods reject symlink escapes at the
// syscall level). Key strings are additionally validated before use as a
// first line of defence.
type Local struct {
	root *os.Root
}

// Compile-time assertion that *Local satisfies the interface.
var _ Storage = (*Local)(nil)

// NewLocal opens root as the storage directory, creating it (and any missing
// parents) if necessary. It fails fast on a misconfigured or inaccessible
// root rather than deferring the error to the first Put.
func NewLocal(root string) (*Local, error) {
	if root == "" {
		return nil, errors.New("storage: empty root path")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("storage: create root %q: %w", root, err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("storage: open root %q: %w", root, err)
	}
	return &Local{root: r}, nil
}

// cleanKey validates an opaque storage key and returns the equivalent clean
// relative path for use with the os.Root handle. It rejects anything that
// could escape the root: empty keys, NUL bytes, backslashes (which are legal
// filename bytes on Linux but path separators on Windows, so rejecting them
// keeps keys canonical), absolute paths, and any ".." traversal.
func cleanKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidKey)
	}
	if strings.ContainsRune(key, 0) {
		return "", fmt.Errorf("%w: contains NUL byte", ErrInvalidKey)
	}
	if strings.Contains(key, `\`) {
		return "", fmt.Errorf("%w: contains backslash", ErrInvalidKey)
	}
	if !filepath.IsLocal(key) {
		return "", fmt.Errorf("%w: %q escapes root", ErrInvalidKey, key)
	}
	clean := filepath.Clean(key)
	if clean == "." {
		return "", fmt.Errorf("%w: %q resolves to the root itself", ErrInvalidKey, key)
	}
	return clean, nil
}

// Put writes the object atomically: it streams into a temp file in the target
// directory, fsyncs, then renames over the final name, so a crashed or failed
// write never leaves a partial object readable at key.
func (l *Local) Put(_ context.Context, key string, r io.Reader, contentType string) error {
	// contentType is advisory metadata; the local backend does not persist it
	// (the mime type is stored in the images table). Accepted to satisfy the
	// Storage interface.
	_ = contentType

	clean, err := cleanKey(key)
	if err != nil {
		return err
	}

	if dir := filepath.Dir(clean); dir != "." {
		if err := l.root.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("storage: mkdir for %q: %w", key, err)
		}
	}

	tmp, err := tempName(clean)
	if err != nil {
		return fmt.Errorf("storage: temp name for %q: %w", key, err)
	}
	f, err := l.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("storage: create temp for %q: %w", key, err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = f.Close()
			_ = l.root.Remove(tmp)
		}
	}()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("storage: write %q: %w", key, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("storage: sync %q: %w", key, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("storage: close %q: %w", key, err)
	}
	if err := l.root.Rename(tmp, clean); err != nil {
		return fmt.Errorf("storage: publish %q: %w", key, err)
	}
	committed = true
	return nil
}

// Get opens the object at key. The returned *os.File is exposed only as an
// io.ReadCloser, so no filesystem type leaks to callers.
func (l *Local) Get(_ context.Context, key string) (io.ReadCloser, error) {
	clean, err := cleanKey(key)
	if err != nil {
		return nil, err
	}
	f, err := l.root.Open(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	return f, nil
}

// Delete removes the object at key. A missing key is treated as success
// (idempotent, matching object-store semantics).
func (l *Local) Delete(_ context.Context, key string) error {
	clean, err := cleanKey(key)
	if err != nil {
		return err
	}
	if err := l.root.Remove(clean); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	return nil
}

// Exists reports whether an object exists at key.
func (l *Local) Exists(_ context.Context, key string) (bool, error) {
	clean, err := cleanKey(key)
	if err != nil {
		return false, err
	}
	if _, err := l.root.Stat(clean); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("storage: exists %q: %w", key, err)
	}
	return true, nil
}

// List walks the tree and returns every object key that begins with prefix,
// in lexical order. In-progress temp files are skipped so a concurrent Put is
// never observed. fs.FS paths are always slash-separated, which matches the
// opaque key format callers use.
func (l *Local) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	walkErr := fs.WalkDir(l.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(path.Base(p), ".tmp-") {
			return nil
		}
		if strings.HasPrefix(p, prefix) {
			keys = append(keys, p)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("storage: list %q: %w", prefix, walkErr)
	}
	return keys, nil
}

// tempName returns a random hidden temp key in the same directory as clean, so
// the subsequent rename is atomic (same filesystem) and the temp file sorts
// away from real objects.
func tempName(clean string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	name := ".tmp-" + hex.EncodeToString(b[:])
	if dir := filepath.Dir(clean); dir != "." {
		return filepath.Join(dir, name), nil
	}
	return name, nil
}
