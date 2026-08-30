package notification

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPayloads(t *testing.T) {
	value := SessionNotification{Title: "Agora · Agent 任务完成", Summary: "任务结果已产生", OpenURL: "https://agora.example/sessions/sess-1"}
	for _, provider := range []Provider{ProviderFeishu, ProviderDingTalk, ProviderWeCom, ProviderGeneric} {
		body, contentType, err := Payload(provider, value)
		if err != nil || contentType != "application/json" || len(body) == 0 {
			t.Fatalf("payload %s: body=%q contentType=%q err=%v", provider, body, contentType, err)
		}
	}
}

func TestWebhookNotifierContinuesAfterFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	notifier := &WebhookNotifier{Client: server.Client(), Targets: []Target{
		{ID: "bad", Label: "bad", Provider: ProviderFeishu, URL: server.URL + "/bad", Enabled: true},
		{ID: "good", Provider: ProviderGeneric, URL: server.URL + "/good", Enabled: true},
	}}
	if err := notifier.NotifySessionEvent(context.Background(), SessionNotification{Title: "test"}); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("expected the failed target to be reported after continuing, got %v", err)
	}
}

func TestPolicyDedupes(t *testing.T) {
	policy := NewPolicy(0)
	value := SessionNotification{SessionID: "sess-1", State: "failed", Attention: AttentionFailed}
	if !policy.Allow(value) || policy.Allow(value) {
		t.Fatal("expected duplicate notification to be suppressed")
	}
}
