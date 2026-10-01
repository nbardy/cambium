package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCopyPublishAndRemove(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "metadata", "state.json")
	if err := WriteFileAtomic(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != "two" {
		t.Fatalf("atomic replacement mismatch: %q, %v", bytes, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("wrong mode: %o", info.Mode().Perm())
	}

	copyPath := filepath.Join(root, "copy", "state.json")
	if err := CopyFileAtomic(path, copyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	bytes, err = os.ReadFile(copyPath)
	if err != nil || string(bytes) != "two" {
		t.Fatalf("atomic copy mismatch: %q, %v", bytes, err)
	}

	temporary := filepath.Join(root, ".prepared")
	if err := os.Mkdir(temporary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temporary, "ready"), []byte("yes"), 0o644); err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(root, "published")
	if err := PublishDir(temporary, published); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(published, "ready")); err != nil {
		t.Fatal(err)
	}

	if err := RemoveFile(path); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still exists: %v", err)
	}
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "nested", "value"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
}
