package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/domains"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/reqstats"
	"github.com/supavise/supavise/internal/secrets"
)

// KeySource returns the decrypted credentials of a project. lifecycle.Manager
// satisfies it; RegistryKeys is a standalone implementation.
type KeySource interface {
	Keys(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
}

// Waker is the idle-wake hook. Start is called after a request has been
// authorized and before it is forwarded, for every project route including
// WebSocket upgrades. A project that is already running must make it cheap and
// idempotent. An error answers the request with 503 and Retry-After. v1 has no
// sleep logic: the default is a no-op.
type Waker interface {
	Start(ctx context.Context, ref string) error
}

// IdleWaker starts a project the node paused because nothing used it (a branch past
// [branching] idle_pause_after). The proxy calls Wake for a request that reaches such a project,
// after the request has been authorized and before it is forwarded, and holds the request until Wake
// returns. Wake must return when ctx ends, with the cause; the wake itself goes on without the
// request. Wake does nothing for a project that is running, and refuses one the owner paused
// (an error answers the request with 503 and Retry-After).
type IdleWaker interface {
	Wake(ctx context.Context, ref string) error
}

type noopWaker struct{}

func (noopWaker) Start(context.Context, string) error { return nil }

// Options configures a Server.
type Options struct {
	Config *config.Config
	// Registry is the route source: projects and routes, with change notifications.
	Registry registry.Registry
	Keys     KeySource
	// APIHandler serves api.<domain> (the Management API, in process). It owns its
	// own CORS and authentication. Nil answers 503.
	APIHandler http.Handler
	// Waker is called before forwarding; nil means no-op.
	Waker Waker
	// IdleWaker wakes a project that was paused for idleness when a request reaches it. Nil: such a
	// project answers 503 like any paused one.
	IdleWaker IdleWaker
	// FunctionsEnabled routes /functions/v1 to the edge runtime on Ports.EdgeRuntime.
	// While false (v1) the route answers 503 {"message": ...}.
	FunctionsEnabled bool
	// FunctionsProxyToken is the secret sent to the edge runtime in X-Supavise-Proxy-Token.
	// Empty reads the node's secret (config.LoadFunctionsProxyToken) when the first
	// /functions/v1 request arrives. Tests set it.
	FunctionsProxyToken string
	// Resolver answers Studio's custom-domain DNS pre-check (studio_cname.go); nil means the
	// system resolver.
	Resolver domains.Resolver
	// Cluster, when set, ties the proxy to the other nodes: the load balancer's view of them and the
	// follower's certificate mirror. Nil is a server on its own.
	Cluster *Cluster
	// MCPGate guards the remote MCP endpoint, api.<domain>/mcp. The proxy calls it for a request to
	// that path before it forwards anything to Studio. The gate authenticates the bearer and answers
	// CORS preflights and every refusal itself (it has written the response when ok is false); when ok
	// is true the proxy forwards the request to Studio's /api/mcp with rawQuery as its query, which is
	// the gate's rebuilt query and never the client's. The Management API implements it
	// ((*api.Server).MCPGate). Nil: /mcp is not special; the request goes to the Management API like
	// any other path of api.<domain>, which serves nothing there.
	MCPGate func(w http.ResponseWriter, r *http.Request) (rawQuery string, ok bool)
	// Stats, when set, counts the requests that match a route of a project's API, per project, service
	// and minute (internal/reqstats); the Management API answers the dashboard's request charts from it.
	// Nil counts nothing.
	Stats  *reqstats.Counter
	Logger *slog.Logger
}

func (o *Options) validate() error {
	switch {
	case o.Config == nil:
		return errors.New("proxy: Options.Config is required")
	case o.Registry == nil:
		return errors.New("proxy: Options.Registry is required")
	case o.Keys == nil:
		return errors.New("proxy: Options.Keys is required")
	}
	return nil
}

// RegistryKeys is a KeySource that opens the sealed project_secrets of the
// registry. Use it where no lifecycle.Manager is available (the dev command).
type RegistryKeys struct {
	Registry registry.Registry
	Secrets  secrets.Secrets
}

// Keys implements KeySource.
func (r RegistryKeys) Keys(ctx context.Context, ref string) (*secrets.ProjectKeys, error) {
	sealed, err := r.Registry.GetSecrets(ctx, ref)
	if err != nil {
		return nil, err
	}
	if len(sealed) == 0 {
		return nil, registry.ErrNotFound
	}
	m := make(map[string]string, len(sealed))
	for name, blob := range sealed {
		plain, err := r.Secrets.Open(blob)
		if err != nil {
			return nil, err
		}
		m[name] = string(plain)
	}
	return secrets.KeysFromMap(m), nil
}
