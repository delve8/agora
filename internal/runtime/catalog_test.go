package runtime

import (
	"context"
	"testing"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/session"
)

type testSessionCatalog struct {
	provider string
	values   []session.Session
}

func (c testSessionCatalog) Provider() string { return c.provider }
func (c testSessionCatalog) List(_ context.Context, _, _ string) ([]session.Session, error) {
	return append([]session.Session(nil), c.values...), nil
}

func TestManagerSessionCatalogRegistry(t *testing.T) {
	manager := NewManager(NewMemoryStore(), nil, nil)
	manager.history = adapter.NewHistoryCatalog(t.TempDir())
	manager.piHistory = adapter.NewPiHistoryCatalog("", t.TempDir())
	manager.catalogs = manager.newSessionCatalogs()
	catalog := testSessionCatalog{provider: "opencode", values: []session.Session{{ID: "history-1", Agent: "opencode"}}}
	if err := manager.RegisterSessionCatalog(catalog); err != nil {
		t.Fatalf("register catalog: %v", err)
	}
	if err := manager.RegisterSessionCatalog(catalog); err == nil {
		t.Fatal("duplicate provider catalog was accepted")
	}
	values, err := manager.DiscoverHistorySessions(context.Background(), "coord-1", "daemon-1")
	if err != nil {
		t.Fatalf("discover sessions: %v", err)
	}
	if len(values) != 1 || values[0].ID != "history-1" {
		t.Fatalf("unexpected catalog sessions: %+v", values)
	}
}
