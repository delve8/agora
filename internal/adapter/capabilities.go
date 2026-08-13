package adapter

import "github.com/delve8/agora/internal/session"

// Capabilities is an alias kept at the adapter boundary. The canonical model
// lives in session so domain types do not depend on adapter implementations.
type Capabilities = session.Capabilities
