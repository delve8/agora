package coordination

import "time"

// Coordination is the user-visible group that contains Agent sessions.
type Coordination struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}
