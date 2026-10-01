package state

import (
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/model"
)

func TestRegistryRoundTrip(t *testing.T) {
	registry := New(t.TempDir())
	created := time.Now().UTC().Truncate(time.Second)
	workspace := model.Workspace{Version: 2, Name: "agent/one", ID: "abc", CreatedAt: created}
	if err := registry.Save(workspace); err != nil {
		t.Fatal(err)
	}
	loaded, err := registry.Load("agent/one")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != "abc" || !loaded.CreatedAt.Equal(created) {
		t.Fatalf("unexpected workspace: %#v", loaded)
	}
	listed, err := registry.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected one workspace, got %d", len(listed))
	}
}

func TestRegistryNamesWithSameSlugDoNotCollide(t *testing.T) {
	registry := New(t.TempDir())
	first := model.Workspace{Version: 2, Name: "agent/one", ID: "slash"}
	second := model.Workspace{Version: 2, Name: "agent-one", ID: "dash"}
	if err := registry.Save(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(second); err != nil {
		t.Fatal(err)
	}
	loadedFirst, err := registry.Load(first.Name)
	if err != nil {
		t.Fatal(err)
	}
	loadedSecond, err := registry.Load(second.Name)
	if err != nil {
		t.Fatal(err)
	}
	if loadedFirst.ID != "slash" || loadedSecond.ID != "dash" {
		t.Fatalf("metadata collision: %#v %#v", loadedFirst, loadedSecond)
	}
}
