package notification

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestDispatcherSuppressesWithoutCallingTargets(t *testing.T) {
	dispatcher := NewDispatcher(nil, time.Minute, http.DefaultClient)
	value := SessionNotification{SessionID: "sess-1", State: "failed", Attention: AttentionFailed, Title: "failed"}
	if err := dispatcher.NotifySessionEvent(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.NotifySessionEvent(context.Background(), value); err != ErrSuppressed {
		t.Fatalf("expected ErrSuppressed, got %v", err)
	}
}
