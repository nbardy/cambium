//go:build darwin

package native

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAPFSCloneIsIndependent(t *testing.T) {
	root := t.TempDir()
	cloner := Cloner{}
	capability := cloner.Probe(context.Background(), root)
	if !capability.Supported || capability.Mode != "apfs-clone" {
		t.Skipf("APFS clone unavailable: %#v", capability)
	}
	source := filepath.Join(root, "source.bin")
	destination := filepath.Join(root, "destination.bin")
	original := bytes.Repeat([]byte("cambium"), 1024*1024)
	if err := os.WriteFile(source, original, 0o644); err != nil {
		t.Fatal(err)
	}
	mode, err := cloner.ClonePath(context.Background(), source, destination, true)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "apfs-clone" {
		t.Fatalf("clone mode = %q", mode)
	}
	file, err := os.OpenFile(destination, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("changed"), 0); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	sourceBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceBytes, original) {
		t.Fatal("writing the clone modified its source")
	}
}
