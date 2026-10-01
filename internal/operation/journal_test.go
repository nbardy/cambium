package operation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJournalPersistsAdvancesAndDeletes(t *testing.T) {
	journal := New(t.TempDir())
	record, err := journal.Begin(KindCreate, "agent", "cambium/agent", "/tmp/agent", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if record.Stage != StagePlanned {
		t.Fatalf("initial stage = %s", record.Stage)
	}
	if err := journal.Advance(&record, StageWorktreeRegistered); err != nil {
		t.Fatal(err)
	}
	values, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].Stage != StageWorktreeRegistered {
		t.Fatalf("unexpected journal: %#v", values)
	}
	if err := journal.Delete(record.ID); err != nil {
		t.Fatal(err)
	}
	values, err = journal.List()
	if err != nil || len(values) != 0 {
		t.Fatalf("journal was not empty: %#v, %v", values, err)
	}
}

func TestJournalRejectsCorruption(t *testing.T) {
	root := t.TempDir()
	journal := New(root)
	if err := os.WriteFile(filepath.Join(root, "bad.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.List(); err == nil {
		t.Fatal("corrupt operation record was accepted")
	}
}
