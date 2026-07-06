// Package storage defines an object-storage abstraction and a local
// filesystem implementation. Keys are opaque, forward-slash-separated strings;
// callers never see filesystem paths, so a different backend (S3, ...) can be
// added without touching callers.
package storage

import (
	"context"
	"errors"
	"io"
)

// Storage is a content-addressed object store. Keys are opaque strings using
// forward slashes as separators; implementations map them to their own
// namespace (filesystem paths, S3 object keys, ...) without exposing those
// details to callers.
type Storage interface {
	// Put stores the object at key, reading its bytes from r, overwriting any
	// existing object at key atomically. contentType is advisory; backends that
	// cannot persist it (the local filesystem) may ignore it.
	Put(ctx context.Context, key string, r io.Reader, contentType string) error
	// Get opens the object at key for reading. The caller must Close the
	// returned reader. It returns ErrNotFound when no object exists at key.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object at key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Exists reports whether an object exists at key.
	Exists(ctx context.Context, key string) (bool, error)
	// List returns the keys of all objects whose key begins with prefix, in
	// lexical order. The match is a byte prefix (S3-compatible), not a directory
	// boundary. An empty prefix lists every object; a prefix matching nothing
	// returns an empty slice, not an error.
	List(ctx context.Context, prefix string) ([]string, error)
}

// Sentinel errors returned by Storage implementations.
var (
	// ErrNotFound is returned by Get when no object exists at the key.
	ErrNotFound = errors.New("storage: object not found")
	// ErrInvalidKey is returned when a key is empty or would escape the storage
	// namespace (path traversal, absolute path, and similar).
	ErrInvalidKey = errors.New("storage: invalid key")
)
