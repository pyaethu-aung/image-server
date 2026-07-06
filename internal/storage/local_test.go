package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func newTestLocal(t *testing.T) *Local {
	t.Helper()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return l
}

func putString(t *testing.T, l *Local, key, data string) {
	t.Helper()
	if err := l.Put(context.Background(), key, bytes.NewBufferString(data), "application/octet-stream"); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
}

func getString(t *testing.T, l *Local, key string) string {
	t.Helper()
	rc, err := l.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll(%q): %v", key, err)
	}
	return string(b)
}

func TestNewLocal(t *testing.T) {
	t.Run("creates missing dir", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "a", "b", "c")
		if _, err := NewLocal(root); err != nil {
			t.Fatalf("NewLocal: %v", err)
		}
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			t.Fatalf("root not created as dir: err=%v", err)
		}
	})

	t.Run("empty root errors", func(t *testing.T) {
		if _, err := NewLocal(""); err == nil {
			t.Fatal("expected error for empty root")
		}
	})

	t.Run("root is an existing file errors", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewLocal(file); err == nil {
			t.Fatal("expected error when root is a file")
		}
	})
}

func TestCleanKeyRejectsInvalid(t *testing.T) {
	invalid := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"dot", "."},
		{"parent", ".."},
		{"traversal", "../etc/passwd"},
		{"escaping traversal", "foo/../../bar"},
		{"absolute", "/etc/passwd"},
		{"leading slash", "/foo"},
		{"nul byte", "a\x00b"},
		{"backslash", `foo\bar`},
	}
	l := newTestLocal(t)
	ctx := context.Background()
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := l.Put(ctx, tc.key, bytes.NewBufferString("x"), ""); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Put: got %v, want ErrInvalidKey", err)
			}
			if _, err := l.Get(ctx, tc.key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Get: got %v, want ErrInvalidKey", err)
			}
			if _, err := l.Exists(ctx, tc.key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Exists: got %v, want ErrInvalidKey", err)
			}
			if err := l.Delete(ctx, tc.key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Delete: got %v, want ErrInvalidKey", err)
			}
		})
	}
}

func TestTraversalLeavesOutsideFilesUntouched(t *testing.T) {
	root := t.TempDir()
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	// A canary in the parent directory of the root must never be reachable.
	canary := filepath.Join(filepath.Dir(root), "canary.txt")
	if err := os.WriteFile(canary, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := l.Put(context.Background(), "../canary.txt", bytes.NewBufferString("hacked"), ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Put traversal: got %v, want ErrInvalidKey", err)
	}
	b, err := os.ReadFile(canary)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "original" {
		t.Fatalf("canary was modified: %q", b)
	}
}

func TestSymlinkEscapeBlocked(t *testing.T) {
	root := t.TempDir()
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plant a symlink inside the root that points outside it.
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if _, err := l.Get(context.Background(), "evil/secret.txt"); err == nil {
		t.Fatal("Get through escaping symlink should fail")
	}
	if err := l.Put(context.Background(), "evil/planted.txt", bytes.NewBufferString("x"), ""); err == nil {
		t.Fatal("Put through escaping symlink should fail")
	}
}

func TestRoundTrip(t *testing.T) {
	keys := []string{"abc", "derivatives/abc", "a/b/c.jpg", "originals/ab/cd/hash"}
	l := newTestLocal(t)
	ctx := context.Background()
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			want := "payload for " + key
			putString(t, l, key, want)

			ok, err := l.Exists(ctx, key)
			if err != nil || !ok {
				t.Fatalf("Exists after Put: ok=%v err=%v", ok, err)
			}
			if got := getString(t, l, key); got != want {
				t.Fatalf("Get = %q, want %q", got, want)
			}
			if err := l.Delete(ctx, key); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			ok, err = l.Exists(ctx, key)
			if err != nil || ok {
				t.Fatalf("Exists after Delete: ok=%v err=%v", ok, err)
			}
			if _, err := l.Get(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after Delete: got %v, want ErrNotFound", err)
			}
		})
	}
}

func TestPutNormalizesInternalDotDot(t *testing.T) {
	l := newTestLocal(t)
	// foo/../bar stays inside the root and normalizes to bar.
	putString(t, l, "foo/../bar", "value")
	if got := getString(t, l, "bar"); got != "value" {
		t.Fatalf("Get(bar) = %q, want %q", got, "value")
	}
}

func TestPutOverwrite(t *testing.T) {
	l := newTestLocal(t)
	putString(t, l, "obj", "first")
	putString(t, l, "obj", "second")
	if got := getString(t, l, "obj"); got != "second" {
		t.Fatalf("Get = %q, want %q", got, "second")
	}
}

// errReader yields one chunk then fails, to exercise the mid-write error path.
type errReader struct{ done bool }

func (e *errReader) Read(p []byte) (int, error) {
	if e.done {
		return 0, errors.New("simulated read failure")
	}
	e.done = true
	return copy(p, []byte("partial data")), nil
}

func TestPutReaderErrorLeavesNoTempFile(t *testing.T) {
	root := t.TempDir()
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Put(context.Background(), "will-fail", &errReader{}, ""); err == nil {
		t.Fatal("expected Put to fail on reader error")
	}
	// No object and no leftover temp file.
	if ok, _ := l.Exists(context.Background(), "will-fail"); ok {
		t.Fatal("object should not exist after failed Put")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) >= 5 && e.Name()[:5] == ".tmp-" {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestPutParentIsFile(t *testing.T) {
	l := newTestLocal(t)
	putString(t, l, "a", "i am a file")
	// "a" is a file, so creating "a/b" must fail (cannot mkdir under a file).
	if err := l.Put(context.Background(), "a/b", bytes.NewBufferString("x"), ""); err == nil {
		t.Fatal("expected Put to fail when parent path is a file")
	}
}

func TestOperationsOnNonDirParent(t *testing.T) {
	l := newTestLocal(t)
	putString(t, l, "a", "i am a file")
	ctx := context.Background()
	// "a" is a file, so "a/b" cannot be traversed: these return a real error
	// (ENOTDIR), not the not-found sentinel.
	if _, err := l.Exists(ctx, "a/b"); err == nil {
		t.Error("Exists under a file parent: want error")
	}
	if err := l.Delete(ctx, "a/b"); err == nil {
		t.Error("Delete under a file parent: want error")
	}
	if _, err := l.Get(ctx, "a/b"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Get under a file parent: want non-NotFound error, got %v", err)
	}
}

func TestPutRenameOntoDirectoryFails(t *testing.T) {
	l := newTestLocal(t)
	putString(t, l, "d/f", "child")
	// "d" is a non-empty directory; publishing an object at key "d" cannot
	// rename over it.
	if err := l.Put(context.Background(), "d", bytes.NewBufferString("x"), ""); err == nil {
		t.Fatal("expected Put to fail renaming onto a directory")
	}
}

func TestPutIntoReadOnlyDirFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks are bypassed when running as root")
	}
	root := t.TempDir()
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "ro")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o750) })

	if err := l.Put(context.Background(), "ro/obj", bytes.NewBufferString("x"), ""); err == nil {
		t.Fatal("expected Put into read-only dir to fail")
	}
}

func TestListWalkError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks are bypassed when running as root")
	}
	root := t.TempDir()
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "locked")
	if err := os.Mkdir(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o750) })

	if _, err := l.List(context.Background(), ""); err == nil {
		t.Fatal("expected List to surface the walk error on an unreadable dir")
	}
}

func TestDeleteMissingIsIdempotent(t *testing.T) {
	l := newTestLocal(t)
	if err := l.Delete(context.Background(), "never/existed"); err != nil {
		t.Fatalf("Delete missing: got %v, want nil", err)
	}
}

func TestExistsMissing(t *testing.T) {
	l := newTestLocal(t)
	ok, err := l.Exists(context.Background(), "nope")
	if err != nil || ok {
		t.Fatalf("Exists missing: ok=%v err=%v", ok, err)
	}
}

func TestGetMissing(t *testing.T) {
	l := newTestLocal(t)
	if _, err := l.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: got %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	l := newTestLocal(t)
	putString(t, l, "originals/aa/bb/one", "1")
	putString(t, l, "derivatives/id1/x", "2")
	putString(t, l, "derivatives/id1/y", "3")
	putString(t, l, "derivatives/id2/z", "4")

	ctx := context.Background()

	t.Run("prefix filters and sorts", func(t *testing.T) {
		got, err := l.List(ctx, "derivatives/id1/")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"derivatives/id1/x", "derivatives/id1/y"}
		if !equalStrings(got, want) {
			t.Fatalf("List = %v, want %v", got, want)
		}
	})

	t.Run("empty prefix lists all", func(t *testing.T) {
		got, err := l.List(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 4 {
			t.Fatalf("List(\"\") = %v, want 4 keys", got)
		}
	})

	t.Run("no match returns empty", func(t *testing.T) {
		got, err := l.List(ctx, "nothing/here/")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("List = %v, want empty", got)
		}
	})

	t.Run("ignores temp files", func(t *testing.T) {
		// Simulate an in-progress write left in the tree.
		if err := os.WriteFile(filepath.Join(l.root.Name(), ".tmp-abcdef"), []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := l.List(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range got {
			if k == ".tmp-abcdef" {
				t.Fatalf("List included temp file: %v", got)
			}
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
