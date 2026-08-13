package server

import (
	"net/http/httptest"
	"testing"
)

func TestSameHostOrigin(t *testing.T) {
	req := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/daemon/ws", nil)
	if !sameHostOrigin(req, "http://127.0.0.1:8080") {
		t.Fatal("expected same origin")
	}
	if sameHostOrigin(req, "https://attacker.example") {
		t.Fatal("unexpected cross origin acceptance")
	}
}
