package runtime

import (
	"context"

	"github.com/delve8/agora/internal/session"
)

// SessionCatalog discovers provider-owned sessions and returns normalized
// session metadata. It must not return transcript events; HistoryPath and
// provider-specific locator fields are used to read history on demand.
//
// A catalog may be backed by files, SQLite, an HTTP API, or a provider
// watcher. The Manager treats all catalogs identically and the Daemon polls
// the registry when a provider has no reliable change stream.
type SessionCatalog interface {
	// Provider returns the stable provider name used for diagnostics and
	// future per-provider capability/discovery policy.
	Provider() string

	// List returns normalized session metadata only. It must not return
	// transcript events or mutate the Server store.
	List(context.Context, string, string) ([]session.Session, error)
}

type sessionCatalogFunc struct {
	provider string
	list     func(context.Context, string, string) ([]session.Session, error)
}

func (f sessionCatalogFunc) Provider() string { return f.provider }
func (f sessionCatalogFunc) List(ctx context.Context, coordinationID, owner string) ([]session.Session, error) {
	return f.list(ctx, coordinationID, owner)
}
