package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Token returns the stored OAuth token for an MCP server, or ErrNotFound.
func (s *Store) Token(ctx context.Context, server string) (*oauth2.Token, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var tok *oauth2.Token
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT access_token, refresh_token, token_type, expiry FROM oauth_tokens WHERE server = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{server},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				tok = &oauth2.Token{
					AccessToken:  stmt.ColumnText(0),
					RefreshToken: stmt.ColumnText(1),
					TokenType:    stmt.ColumnText(2),
				}
				if ms := stmt.ColumnInt64(3); ms > 0 {
					tok.Expiry = time.UnixMilli(ms)
				}
				return nil
			},
		}); err != nil {
		return nil, fmt.Errorf("store: read token for %s: %w", server, err)
	}
	if tok == nil {
		return nil, ErrNotFound
	}
	return tok, nil
}

// PutToken stores or replaces the OAuth token for an MCP server.
func (s *Store) PutToken(ctx context.Context, server string, tok *oauth2.Token) error {
	var expiry int64
	if !tok.Expiry.IsZero() {
		expiry = millis(tok.Expiry)
	}
	tokenType := tok.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return s.exec(ctx,
		`INSERT INTO oauth_tokens (server, access_token, refresh_token, token_type, expiry, updated_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6)
		 ON CONFLICT (server) DO UPDATE SET
		   access_token = excluded.access_token,
		   refresh_token = excluded.refresh_token,
		   token_type = excluded.token_type,
		   expiry = excluded.expiry,
		   updated_at = excluded.updated_at`,
		server, tok.AccessToken, tok.RefreshToken, tokenType, expiry, millis(time.Now()))
}

// DeleteToken removes a stored token, which forces a fresh authorization.
func (s *Store) DeleteToken(ctx context.Context, server string) error {
	return s.exec(ctx, `DELETE FROM oauth_tokens WHERE server = ?1`, server)
}

// OAuthClient is what registration produced for one MCP server, which is enough
// to refresh a token without repeating the flow.
type OAuthClient struct {
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	// AuthStyle is an oauth2.AuthStyle, stored as its integer value.
	AuthStyle int
	Scopes    []string
}

// Client returns the stored client registration for an MCP server, or
// ErrNotFound.
func (s *Store) Client(ctx context.Context, server string) (*OAuthClient, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var client *OAuthClient
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT client_id, client_secret, auth_url, token_url, auth_style, scopes
		 FROM oauth_clients WHERE server = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{server},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				client = &OAuthClient{
					ClientID:     stmt.ColumnText(0),
					ClientSecret: stmt.ColumnText(1),
					AuthURL:      stmt.ColumnText(2),
					TokenURL:     stmt.ColumnText(3),
					AuthStyle:    int(stmt.ColumnInt64(4)),
				}
				if scopes := strings.Fields(stmt.ColumnText(5)); len(scopes) > 0 {
					client.Scopes = scopes
				}
				return nil
			},
		}); err != nil {
		return nil, fmt.Errorf("store: read client for %s: %w", server, err)
	}
	if client == nil {
		return nil, ErrNotFound
	}
	return client, nil
}

// PutClient stores or replaces the client registration for an MCP server.
func (s *Store) PutClient(ctx context.Context, server string, c *OAuthClient) error {
	return s.exec(ctx,
		`INSERT INTO oauth_clients
		   (server, client_id, client_secret, auth_url, token_url, auth_style, scopes, updated_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
		 ON CONFLICT (server) DO UPDATE SET
		   client_id = excluded.client_id,
		   client_secret = excluded.client_secret,
		   auth_url = excluded.auth_url,
		   token_url = excluded.token_url,
		   auth_style = excluded.auth_style,
		   scopes = excluded.scopes,
		   updated_at = excluded.updated_at`,
		server, c.ClientID, c.ClientSecret, c.AuthURL, c.TokenURL,
		c.AuthStyle, strings.Join(c.Scopes, " "), millis(time.Now()))
}

// DeleteClient removes a stored registration, which makes the next
// authorization register a new client.
func (s *Store) DeleteClient(ctx context.Context, server string) error {
	return s.exec(ctx, `DELETE FROM oauth_clients WHERE server = ?1`, server)
}
