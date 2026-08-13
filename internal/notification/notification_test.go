package notification

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPayloads(t *testing.T) {
	value := SessionNotification{Title: "Agora · Session failed", Summary: "Claude stopped", OpenURL: "https://agora.example/sessions/sess-1"}
	for _, provider := range []Provider{ProviderFeishu, ProviderDingTalk, ProviderWeCom, ProviderGeneric} {
		body, contentType, err := Payload(provider, value)
		if err != nil || contentType != "application/json" || len(body) == 0 {
			t.Fatalf("payload %s: body=%q contentType=%q err=%v", provider, body, contentType, err)
		}
	}
}

func TestWebhookNotifierContinuesAfterFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	notifier := &WebhookNotifier{Client: server.Client(), Targets: []Target{{ID: "one", Provider: ProviderFeishu, URL: server.URL, Enabled: true}}}
	if err := notifier.NotifySessionEvent(context.Background(), SessionNotification{Title: "test"}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyDedupes(t *testing.T) {
	policy := NewPolicy(0)
	value := SessionNotification{SessionID: "sess-1", State: "failed", Attention: AttentionFailed}
	if !policy.Allow(value) || policy.Allow(value) {
		t.Fatal("expected duplicate notification to be suppressed")
	}
}
