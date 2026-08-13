package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/store"
)

func TestPTYSnapshotUnavailable(t *testing.T) {
	db, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), runtime.NewPTYManager("", ""))
	srv := New(":0", db, manager)
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/missing/pty/snapshot", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Code)
	}
	if err := srv.Shutdown(context.Background()); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
func TestHealthz(t *testing.T) {
	db, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := runtime.NewManager(db, adapter.NewClaudeCodeAdapter(""), runtime.NewPTYManager("", ""))
	srv := New(":0", db, manager)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Code)
	}
	if err := srv.Shutdown(context.Background()); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
