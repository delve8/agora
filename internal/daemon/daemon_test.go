package daemon

import (
	"context"
	"testing"
	"time"
)

func TestReconnectDelayIsBounded(t *testing.T) {
	if got := reconnectDelay(0); got != time.Second {
		t.Fatalf("attempt 0 delay = %s", got)
	}
	if got := reconnectDelay(3); got != 8*time.Second {
		t.Fatalf("attempt 3 delay = %s", got)
	}
	if got := reconnectDelay(100); got != 32*time.Second {
		t.Fatalf("attempt 100 delay = %s", got)
	}
}

func TestWaitReconnectHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := waitReconnect(ctx, time.Minute); err != context.Canceled {
		t.Fatalf("waitReconnect error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellation took too long: %s", elapsed)
	}
}
