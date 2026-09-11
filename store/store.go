// Package store persists recordings and everything derived from them in a
// single SQLite database.
//
// Vector columns are written only when the store is opened with an embedding
// dimension. Without one the database still works; retrieval is then keyword
// only.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	vector "github.com/justintout/go-sqlite-vector"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Keys recorded in the meta table.
const (
	metaSchemaVersion = "schema_version"
	metaEmbedModel    = "embedding_model"
	metaEmbedDim      = "embedding_dim"
)

// Options configures Open.
type Options struct {
	Path     string
	PoolSize int
	// EmbedModel and EmbedDim describe the configured embedder. Leave EmbedDim
	// zero to open without vector support.
	EmbedModel string
	EmbedDim   int
}

// Store is a pool of connections to one Riverbed database.
type Store struct {
	pool     *sqlitex.Pool
	embedDim int
}

// Open opens or creates the database, registers vector functions on every
// connection when an embedding dimension is given, and applies migrations.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, errors.New("store: path is required")
	}
	if opts.PoolSize < 2 {
		opts.PoolSize = 2
	}

	uri := "file:" + filepath.ToSlash(opts.Path) + "?_txlock=immediate"
	pool, err := sqlitex.NewPool(uri, sqlitex.PoolOptions{
		PoolSize: opts.PoolSize,
		PrepareConn: func(conn *sqlite.Conn) error {
			// Each pragma runs on its own: ExecuteScript would wrap them in a
			// transaction, and synchronous cannot be changed inside one.
			for _, pragma := range []string{
				"PRAGMA journal_mode = wal",
				"PRAGMA synchronous = normal",
				"PRAGMA foreign_keys = on",
				"PRAGMA busy_timeout = 5000",
			} {
				if err := sqlitex.ExecuteTransient(conn, pragma, nil); err != nil {
					return fmt.Errorf("%s: %w", pragma, err)
				}
			}
			if opts.EmbedDim > 0 {
				if err := vector.Register(conn, opts.EmbedDim); err != nil {
					return fmt.Errorf("register vector functions: %w", err)
				}
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", opts.Path, err)
	}

	s := &Store{pool: pool, embedDim: opts.EmbedDim}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.checkEmbedding(ctx, opts.EmbedModel, opts.EmbedDim); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases every connection.
func (s *Store) Close() error { return s.pool.Close() }

// VectorEnabled reports whether vector columns are written and queryable.
func (s *Store) VectorEnabled() bool { return s.embedDim > 0 }

// conn takes a connection from the pool. The returned function returns it.
func (s *Store) conn(ctx context.Context) (*sqlite.Conn, func(), error) {
	c, err := s.pool.Take(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c, func() { s.pool.Put(c) }, nil
}

// migrate applies every migration the database has not seen yet.
func (s *Store) migrate(ctx context.Context) error {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return err
	}
	defer release()

	if err := sqlitex.ExecuteScript(conn, `
		CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
	`, nil); err != nil {
		return fmt.Errorf("store: create meta: %w", err)
	}

	current, err := metaInt(conn, metaSchemaVersion)
	if err != nil {
		return err
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}
	for i, name := range names {
		version := i + 1
		if version <= current {
			continue
		}
		body, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", name, err)
		}
		end, err := sqlitex.ImmediateTransaction(conn)
		if err != nil {
			return fmt.Errorf("store: begin %s: %w", name, err)
		}
		err = sqlitex.ExecuteScript(conn, trimTrailingComments(string(body)), nil)
		if err == nil {
			err = setMeta(conn, metaSchemaVersion, strconv.Itoa(version))
		}
		end(&err)
		if err != nil {
			return fmt.Errorf("store: apply %s: %w", name, err)
		}
	}
	return nil
}

// trimTrailingComments removes comment and blank lines from the end of a
// migration. sqlitex.ExecuteScript prepares the remaining text repeatedly and
// misuses the API if what is left is only a comment, so a migration that ends
// with one would otherwise fail.
func trimTrailingComments(script string) string {
	lines := strings.Split(script, "\n")
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" || strings.HasPrefix(last, "--") {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}
	return strings.Join(lines, "\n")
}

// migrationNames returns the migration files in lexical, therefore applied,
// order.
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sql" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// checkEmbedding pins the embedding model and dimension on first use and
// refuses to open a database whose vectors were produced by another model: the
// stored vectors would be silently incomparable.
func (s *Store) checkEmbedding(ctx context.Context, model string, dim int) error {
	if dim <= 0 {
		return nil
	}
	conn, release, err := s.conn(ctx)
	if err != nil {
		return err
	}
	defer release()

	storedModel, err := metaText(conn, metaEmbedModel)
	if err != nil {
		return err
	}
	storedDim, err := metaInt(conn, metaEmbedDim)
	if err != nil {
		return err
	}

	if storedModel == "" && storedDim == 0 {
		if err := setMeta(conn, metaEmbedModel, model); err != nil {
			return err
		}
		return setMeta(conn, metaEmbedDim, strconv.Itoa(dim))
	}
	if storedModel != model || storedDim != dim {
		return fmt.Errorf(
			"store: database holds %s embeddings of %d dimensions but %s of %d dimensions are configured; "+
				"re-embed the database or point at a different file",
			storedModel, storedDim, model, dim)
	}
	return nil
}

func metaText(conn *sqlite.Conn, key string) (string, error) {
	var out string
	err := sqlitex.ExecuteTransient(conn, `SELECT value FROM meta WHERE key = ?1`, &sqlitex.ExecOptions{
		Args: []any{key},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			out = stmt.ColumnText(0)
			return nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("store: read meta %s: %w", key, err)
	}
	return out, nil
}

func metaInt(conn *sqlite.Conn, key string) (int, error) {
	text, err := metaText(conn, key)
	if err != nil || text == "" {
		return 0, err
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("store: meta %s is not a number: %q", key, text)
	}
	return n, nil
}

func setMeta(conn *sqlite.Conn, key, value string) error {
	err := sqlitex.Execute(conn,
		`INSERT INTO meta (key, value) VALUES (?1, ?2)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		&sqlitex.ExecOptions{Args: []any{key, value}})
	if err != nil {
		return fmt.Errorf("store: write meta %s: %w", key, err)
	}
	return nil
}

// millis converts a time to unix milliseconds, the representation used
// throughout the schema.
func millis(t time.Time) int64 { return t.UnixMilli() }
