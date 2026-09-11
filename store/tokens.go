package store

import (
	"context"
	"fmt"
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
