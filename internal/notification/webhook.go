package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type WebhookNotifier struct {
	Client  *http.Client
	Targets []Target
}

func (n *WebhookNotifier) NotifySessionEvent(ctx context.Context, value SessionNotification) error {
	for _, target := range n.Targets {
		if !target.Enabled || strings.TrimSpace(target.URL) == "" {
			continue
		}
		if delivery := n.NotifyTarget(ctx, target, value); !delivery.Delivered {
			// One provider failure must not prevent other configured targets from receiving the notice.
			continue
		}
	}
	return nil
}

func (n *WebhookNotifier) NotifyTarget(ctx context.Context, target Target, value SessionNotification) Delivery {
	delivery := Delivery{TargetID: target.ID, Provider: target.Provider, CreatedAt: time.Now().UTC()}
	body, contentType, err := Payload(target.Provider, value)
	if err != nil {
		delivery.Error = err.Error()
		return delivery
	}
	client := n.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		delivery.Error = err.Error()
		return delivery
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		delivery.Error = err.Error()
		return delivery
	}
	defer resp.Body.Close()
	delivery.StatusCode = resp.StatusCode
	delivery.Delivered = resp.StatusCode >= 200 && resp.StatusCode < 300
	if !delivery.Delivered {
		delivery.Error = fmt.Sprintf("webhook returned HTTP %d", resp.StatusCode)
	}
	return delivery
}

func Payload(provider Provider, value SessionNotification) ([]byte, string, error) {
	text := value.Title
	if value.Summary != "" {
		text += "\n" + value.Summary
	}
	if value.OpenURL != "" {
		text += "\n打开 Agora：" + value.OpenURL
	}
	text = strings.TrimSpace(text)
	switch provider {
	case ProviderFeishu:
		body, err := json.Marshal(map[string]any{"msg_type": "text", "content": map[string]string{"text": text}})
		return body, "application/json", err
	case ProviderDingTalk:
		body, err := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": text}})
		return body, "application/json", err
	case ProviderWeCom:
		body, err := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": text}})
		return body, "application/json", err
	case ProviderGeneric:
		body, err := json.Marshal(map[string]any{"text": text, "notification": value})
		return body, "application/json", err
	default:
		return nil, "", fmt.Errorf("unsupported notification provider %q", provider)
	}
}
