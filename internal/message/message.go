package message

import "time"

type Endpoint struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type Status string

const (
	StatusPending Status = "pending"
	StatusSent    Status = "sent"
	StatusFailed  Status = "failed"
)

// Message is an explicit human-to-session collaboration record.
type Message struct {
	ID             string    `json:"id"`
	CoordinationID string    `json:"coordination_id"`
	Sender         Endpoint  `json:"sender"`
	Recipient      Endpoint  `json:"recipient"`
	Content        string    `json:"content"`
	ReplyTo        string    `json:"reply_to,omitempty"`
	Status         Status    `json:"status"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}
