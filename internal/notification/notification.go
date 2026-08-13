package notification

import (
	"context"
	"time"
)

type Provider string

const (
	ProviderGeneric  Provider = "generic"
	ProviderFeishu   Provider = "feishu"
	ProviderDingTalk Provider = "dingtalk"
	ProviderWeCom    Provider = "wecom"
)

type Attention string

const (
	AttentionNone      Attention = "none"
	AttentionFailed    Attention = "failed"
	AttentionStopped   Attention = "stopped"
	AttentionCompleted Attention = "completed"
	AttentionResumed   Attention = "resumed"
)

type Target struct {
	ID       string   `json:"id"`
	Provider Provider `json:"provider"`
	Label    string   `json:"label"`
	URL      string   `json:"-"`
	Enabled  bool     `json:"enabled"`
}

type SessionNotification struct {
	ID             string    `json:"id"`
	CoordinationID string    `json:"coordination_id"`
	SessionID      string    `json:"session_id"`
	SessionName    string    `json:"session_name"`
	State          string    `json:"state"`
	Attention      Attention `json:"attention"`
	Title          string    `json:"title"`
	Summary        string    `json:"summary"`
	OpenURL        string    `json:"open_url,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type Delivery struct {
	TargetID   string    `json:"target_id"`
	Provider   Provider  `json:"provider"`
	StatusCode int       `json:"status_code,omitempty"`
	Delivered  bool      `json:"delivered"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Notifier interface {
	NotifySessionEvent(context.Context, SessionNotification) error
}

type TargetNotifier interface {
	NotifyTarget(context.Context, Target, SessionNotification) Delivery
}
