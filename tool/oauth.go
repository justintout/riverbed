package tool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/justintout/riverbed/config"
	"github.com/justintout/riverbed/store"
)

// TokenStore persists what an authorization produced, so a restart neither needs
// a browser nor registers a second client.
type TokenStore interface {
	Token(ctx context.Context, server string) (*oauth2.Token, error)
	PutToken(ctx context.Context, server string, tok *oauth2.Token) error
	Client(ctx context.Context, server string) (*store.OAuthClient, error)
	PutClient(ctx context.Context, server string, client *store.OAuthClient) error
}

// ErrAuthorizationRequired reports that a server has no usable token. The
// daemon cannot open a browser, so it surfaces this instead of blocking.
var ErrAuthorizationRequired = errors.New("tool: authorization required")

// Authorization describes an authorization flow waiting on a person.
type Authorization struct {
	// URL must be opened in a browser.
	URL string
	// Wait blocks until the browser is redirected back, then returns the code
	// and state.
	Wait func(ctx context.Context) (code, state string, err error)
	// Close releases the callback listener.
	Close func() error
}

// Prompter presents an authorization URL to a person and reports the result.
// Interactive commands supply one; the daemon does not.
type Prompter func(ctx context.Context, server string, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error)

// OAuth builds OAuth handlers for MCP servers.
//
// Discovery of the protected resource and authorization server metadata,
// dynamic client registration, and PKCE are all handled by the MCP SDK. This
// type supplies the two things the SDK cannot know: where tokens are kept, and
// how to reach a person when consent is needed.
type OAuth struct {
	store    TokenStore
	baseURL  string
	prompter Prompter
	log      *slog.Logger

	mu       sync.Mutex
	handlers map[string]*auth.AuthorizationCodeHandler
}

// OAuthOptions configures an OAuth authorizer.
type OAuthOptions struct {
	Store TokenStore
	// BaseURL is Riverbed's externally reachable URL; the redirect is built
	// under it.
	BaseURL string
	// Prompter is nil in the daemon, which then reports
	// ErrAuthorizationRequired rather than waiting for a person.
	Prompter Prompter
	Logger   *slog.Logger
}

// NewOAuth returns an authorizer for servers configured with auth = "oauth".
func NewOAuth(opts OAuthOptions) (*OAuth, error) {
	if opts.Store == nil {
		return nil, errors.New("tool: oauth needs a token store")
	}
	if opts.BaseURL == "" {
		return nil, errors.New("tool: oauth needs a base URL for the redirect")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &OAuth{
		store:    opts.Store,
		baseURL:  strings.TrimSuffix(opts.BaseURL, "/"),
		prompter: opts.Prompter,
		log:      opts.Logger,
		handlers: make(map[string]*auth.AuthorizationCodeHandler),
	}, nil
}

// RedirectPath is the path the authorization server redirects back to.
const RedirectPath = "/oauth/callback"

// RedirectURL returns the redirect URL registered for a server.
func (o *OAuth) RedirectURL(server string) string {
	return o.baseURL + RedirectPath + "/" + url.PathEscape(server)
}

// Handler implements Authorizer.
func (o *OAuth) Handler(cfg config.MCP) (auth.OAuthHandler, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if h, ok := o.handlers[cfg.Name]; ok {
		return h, nil
	}

	redirect := o.RedirectURL(cfg.Name)
	handlerConfig := &auth.AuthorizationCodeHandlerConfig{
		// Riverbed is pre-registered nowhere, so it registers itself the first
		// time. Dynamic registration stays configured as the fallback for a
		// server Riverbed has not met yet.
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:   "Riverbed",
				RedirectURIs: []string{redirect},
				GrantTypes:   []string{"authorization_code", "refresh_token"},
				Scope:        strings.Join(cfg.Scopes, " "),
			},
		},
		RedirectURL: redirect,
		// Refresh tokens are stored, so ask for one: a home server should not
		// need a browser every hour.
		RequestRefreshToken:      true,
		AuthorizationCodeFetcher: o.fetcher(cfg.Name),
		InitialTokenSource:       o.stored(cfg.Name),
		NewTokenSource:           o.persisting(cfg.Name),
	}

	// A stored registration is offered to the SDK, which prefers it over
	// registering again. Registering on every authorization would leave a trail
	// of clients on the authorization server, and some rate-limit it.
	if client, err := o.store.Client(context.Background(), cfg.Name); err == nil && client.ClientID != "" {
		handlerConfig.PreregisteredClient = preregistered(client)
	}

	handler, err := auth.NewAuthorizationCodeHandler(handlerConfig)
	if err != nil {
		return nil, fmt.Errorf("oauth handler: %w", err)
	}
	o.handlers[cfg.Name] = handler
	return handler, nil
}

// preregistered converts a stored registration into the SDK's credentials.
//
// Issuer is deliberately left empty. It would make the SDK reject a
// registration whose authorization server has changed, but the issuer is not
// among the values the SDK hands back, so it cannot be stored. A stale
// registration surfaces as a failed authorization, which "riverbed auth -reset"
// clears.
func preregistered(client *store.OAuthClient) *oauthex.ClientCredentials {
	credentials := &oauthex.ClientCredentials{ClientID: client.ClientID}
	if client.ClientSecret != "" {
		credentials.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: client.ClientSecret}
	}
	return credentials
}

// fetcher returns the function the SDK calls to start a flow.
func (o *OAuth) fetcher(server string) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		if o.prompter == nil {
			o.log.Warn("mcp server needs authorization",
				"mcp", server, "hint", "run: riverbed auth "+server)
			return nil, fmt.Errorf("%w: run \"riverbed auth %s\"", ErrAuthorizationRequired, server)
		}
		return o.prompter(ctx, server, args)
	}
}

// stored returns a token source seeded from the database, or nil when nothing
// has been stored yet, which makes the SDK start a flow.
//
// When the registration was stored too, the source is a real oauth2 one built
// from it, so an expired access token is refreshed against the token endpoint
// rather than sending someone to a browser. Refreshed tokens are written back.
func (o *OAuth) stored(server string) oauth2.TokenSource {
	ctx := context.Background()
	tok, err := o.store.Token(ctx, server)
	if err != nil || tok == nil || tok.AccessToken == "" {
		return nil
	}

	client, err := o.store.Client(ctx, server)
	if err != nil || client.TokenURL == "" {
		// Without a registration there is nothing to refresh against, so the
		// token is used as it stands. An expired one produces a 401, and the
		// transport then starts the flow.
		o.log.Debug("no stored oauth registration, cannot refresh without a browser", "mcp", server)
		return &storedSource{token: tok}
	}

	cfg := &oauth2.Config{
		ClientID:     client.ClientID,
		ClientSecret: client.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   client.AuthURL,
			TokenURL:  client.TokenURL,
			AuthStyle: oauth2.AuthStyle(client.AuthStyle),
		},
		RedirectURL: o.RedirectURL(server),
		Scopes:      client.Scopes,
	}
	return &persistingSource{
		oauth:  o,
		server: server,
		inner:  cfg.TokenSource(ctx, tok),
		last:   tok.AccessToken,
	}
}

// persisting wraps the SDK's token source so every refreshed token is written
// back. Without this a refresh would live only in memory and be lost on
// restart, sending the user back to a browser.
func (o *OAuth) persisting(server string) func(context.Context, *oauth2.Config, *oauth2.Token) (oauth2.TokenSource, error) {
	return func(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		// This config is the only place the SDK reveals what registration and
		// discovery produced, so it is recorded here.
		if err := o.store.PutClient(ctx, server, &store.OAuthClient{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			AuthURL:      cfg.Endpoint.AuthURL,
			TokenURL:     cfg.Endpoint.TokenURL,
			AuthStyle:    int(cfg.Endpoint.AuthStyle),
			Scopes:       cfg.Scopes,
		}); err != nil {
			return nil, fmt.Errorf("store client registration for %s: %w", server, err)
		}
		if err := o.store.PutToken(ctx, server, tok); err != nil {
			return nil, fmt.Errorf("store token for %s: %w", server, err)
		}
		return &persistingSource{
			oauth:  o,
			server: server,
			inner:  cfg.TokenSource(ctx, tok),
			last:   tok.AccessToken,
		}, nil
	}
}

// storedSource serves a token that cannot be refreshed, because no registration
// was stored alongside it. Returning an expired token makes the transport see a
// 401 and start the flow.
type storedSource struct {
	token *oauth2.Token
}

func (s *storedSource) Token() (*oauth2.Token, error) { return s.token, nil }

// persistingSource writes a token back whenever the inner source rotates it.
type persistingSource struct {
	oauth  *OAuth
	server string
	inner  oauth2.TokenSource

	mu   sync.Mutex
	last string
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := p.inner.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	changed := tok.AccessToken != p.last
	if changed {
		p.last = tok.AccessToken
	}
	p.mu.Unlock()

	if changed {
		if err := p.oauth.store.PutToken(context.Background(), p.server, tok); err != nil {
			// A write failure must not break a working request; the next start
			// simply authorizes again.
			p.oauth.log.Error("store refreshed token", "mcp", p.server, "error", err)
		} else {
			p.oauth.log.Info("stored refreshed oauth token", "mcp", p.server)
		}
	}
	return tok, nil
}

// LocalPrompter returns a Prompter that serves the redirect on a local listener
// and prints the authorization URL. It suits the "riverbed auth" command, where
// a person is present but the host may have no browser.
//
// The listener address must match the redirect URL the server was configured
// with, which is why the port is taken from that URL.
func LocalPrompter(redirectBase string, present func(url string)) (Prompter, error) {
	u, err := url.Parse(redirectBase)
	if err != nil {
		return nil, fmt.Errorf("tool: unreadable base URL %q: %w", redirectBase, err)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	return func(ctx context.Context, server string, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		type callback struct {
			code, state, iss string
		}
		done := make(chan callback, 1)

		mux := http.NewServeMux()
		mux.HandleFunc(RedirectPath+"/", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if e := q.Get("error"); e != "" {
				http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
				done <- callback{}
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("Riverbed is authorized. You can close this tab."))
			done <- callback{code: q.Get("code"), state: q.Get("state"), iss: q.Get("iss")}
		})

		listener, err := net.Listen("tcp", ":"+port)
		if err != nil {
			return nil, fmt.Errorf("listen for the oauth redirect on port %s: %w", port, err)
		}
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(listener) }()
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
		}()

		present(args.URL)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case cb := <-done:
			if cb.code == "" {
				return nil, errors.New("authorization was refused")
			}
			return &auth.AuthorizationResult{Code: cb.code, State: cb.state, Iss: cb.iss}, nil
		}
	}, nil
}
