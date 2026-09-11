package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	vector "github.com/justintout/go-sqlite-vector"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Query describes a retrieval request over stored recordings.
//
// Text drives keyword matching, Vector drives nearest-neighbour matching, and
// supplying both fuses the two rankings. The remaining fields filter whichever
// rankings run.
type Query struct {
	Text   string
	Vector []float32

	// Since and Until bound recorded_at. Zero means unbounded.
	Since time.Time
	Until time.Time

	// Route restricts results to recordings handled by this agent.
	Route string
	// Tags restricts results to recordings carrying every listed tag.
	Tags []string
	// ToolUsed restricts results to recordings where a tool was, or was not,
	// invoked. Nil means no restriction.
	ToolUsed *bool

	// Limit caps the results. Zero means 20.
	Limit int
}

// Result is one retrieved recording.
type Result struct {
	Recording Recording
	Tags      []string
	// Score is the fused rank score. Larger is better.
	Score float64
	// KeywordRank and VectorRank are 1-based positions in each ranking, or 0
	// when that ranking did not return the row.
	KeywordRank int
	VectorRank  int
	// Snippet is the matching chunk text when the vector ranking found the row.
	Snippet string
}

// rrfK is the reciprocal rank fusion constant. 60 is the value from the
// original paper and is not sensitive.
const rrfK = 60.0

// Search runs the requested rankings and returns fused results.
//
// Reciprocal rank fusion is used rather than mixing bm25 scores with vector
// distances directly: the two are on unrelated scales, so only their orderings
// can be combined meaningfully.
func (s *Store) Search(ctx context.Context, q Query) ([]Result, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if len(q.Vector) > 0 && !s.VectorEnabled() {
		return nil, fmt.Errorf("store: vector search needs an embedder")
	}
	if q.Text == "" && len(q.Vector) == 0 {
		return s.recent(ctx, q)
	}

	conn, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	// Over-fetch each ranking so fusion has room to reorder.
	depth := q.Limit * 4

	fused := make(map[int64]*Result)
	at := func(id int64) *Result {
		r, ok := fused[id]
		if !ok {
			r = &Result{}
			fused[id] = r
		}
		return r
	}

	if q.Text != "" {
		ids, err := s.keywordRanking(conn, q, depth)
		if err != nil {
			return nil, err
		}
		for i, id := range ids {
			r := at(id)
			r.KeywordRank = i + 1
			r.Score += 1 / (rrfK + float64(i+1))
		}
	}

	if len(q.Vector) > 0 {
		ids, snippets, err := s.vectorRanking(conn, q, depth)
		if err != nil {
			return nil, err
		}
		for i, id := range ids {
			r := at(id)
			r.VectorRank = i + 1
			r.Snippet = snippets[i]
			r.Score += 1 / (rrfK + float64(i+1))
		}
	}

	results := make([]Result, 0, len(fused))
	for id, r := range fused {
		rec, err := s.getConn(conn, id)
		if err != nil {
			return nil, err
		}
		r.Recording = rec
		results = append(results, *r)
	}

	// Sort by fused score, breaking ties by recency so equal matches are
	// ordered predictably.
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Recording.RecordedAt.After(results[j].Recording.RecordedAt)
	})
	if len(results) > q.Limit {
		results = results[:q.Limit]
	}
	return s.attachTags(conn, results)
}

// keywordRanking returns recording ids ordered by FTS5 relevance.
func (s *Store) keywordRanking(conn *sqlite.Conn, q Query, limit int) ([]int64, error) {
	where, args := q.filters(3)
	query := `SELECT r.id FROM recordings_fts f
		JOIN recordings r ON r.id = f.rowid
		WHERE f.transcription MATCH ?1` + where + `
		ORDER BY bm25(recordings_fts) LIMIT ?2`

	ids := make([]int64, 0, limit)
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: append([]any{ftsQuery(q.Text), limit}, args...),
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ids = append(ids, stmt.ColumnInt64(0))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: keyword search: %w", err)
	}
	return ids, nil
}

// vectorRanking returns recording ids ordered by distance to the query vector,
// keeping each recording's closest chunk only.
func (s *Store) vectorRanking(conn *sqlite.Conn, q Query, limit int) ([]int64, []string, error) {
	where, args := q.filters(3)
	query := `SELECT e.recording_id, e.text, MIN(vector_distance(e.vector, ?1)) AS d
		FROM embeddings e
		JOIN recordings r ON r.id = e.recording_id
		WHERE 1 = 1` + where + `
		GROUP BY e.recording_id
		ORDER BY d LIMIT ?2`

	ids := make([]int64, 0, limit)
	snippets := make([]string, 0, limit)
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: append([]any{vector.Float32ToBlob(q.Vector), limit}, args...),
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ids = append(ids, stmt.ColumnInt64(0))
			snippets = append(snippets, stmt.ColumnText(1))
			return nil
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("store: vector search: %w", err)
	}
	return ids, snippets, nil
}

// recent returns the newest recordings matching the filters, for a query with
// no text and no vector.
func (s *Store) recent(ctx context.Context, q Query) ([]Result, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	where, args := q.filters(2)
	query := `SELECT ` + prefixed(recordingColumns, "r") + ` FROM recordings r
		WHERE 1 = 1` + where + ` ORDER BY r.recorded_at DESC LIMIT ?1`

	var results []Result
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: append([]any{q.Limit}, args...),
		ResultFunc: func(stmt *sqlite.Stmt) error {
			results = append(results, Result{Recording: scanRecording(stmt)})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: recent: %w", err)
	}
	return s.attachTags(conn, results)
}

// filters renders the shared WHERE clauses. Parameters are numbered from start,
// which lets each caller reserve the low numbers for its own arguments.
func (q Query) filters(start int) (string, []any) {
	var b strings.Builder
	var args []any
	next := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("?%d", start+len(args)-1)
	}

	if !q.Since.IsZero() {
		b.WriteString(" AND r.recorded_at >= " + next(millis(q.Since)))
	}
	if !q.Until.IsZero() {
		b.WriteString(" AND r.recorded_at <= " + next(millis(q.Until)))
	}
	if q.Route != "" {
		b.WriteString(" AND r.route = " + next(q.Route))
	}
	for _, tag := range q.Tags {
		b.WriteString(" AND EXISTS (SELECT 1 FROM tags t WHERE t.recording_id = r.id AND t.tag = " + next(tag) + ")")
	}
	if q.ToolUsed != nil {
		clause := " AND EXISTS (SELECT 1 FROM tool_calls c WHERE c.recording_id = r.id)"
		if !*q.ToolUsed {
			clause = " AND NOT EXISTS (SELECT 1 FROM tool_calls c WHERE c.recording_id = r.id)"
		}
		b.WriteString(clause)
	}
	return b.String(), args
}

func (s *Store) getConn(conn *sqlite.Conn, id int64) (Recording, error) {
	var rec Recording
	found := false
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT `+recordingColumns+` FROM recordings WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rec = scanRecording(stmt)
				found = true
				return nil
			},
		}); err != nil {
		return Recording{}, fmt.Errorf("store: load recording %d: %w", id, err)
	}
	if !found {
		return Recording{}, ErrNotFound
	}
	return rec, nil
}

func (s *Store) attachTags(conn *sqlite.Conn, results []Result) ([]Result, error) {
	for i := range results {
		var tags []string
		if err := sqlitex.ExecuteTransient(conn,
			`SELECT tag FROM tags WHERE recording_id = ?1 ORDER BY tag`,
			&sqlitex.ExecOptions{
				Args: []any{results[i].Recording.ID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					tags = append(tags, stmt.ColumnText(0))
					return nil
				},
			}); err != nil {
			return nil, fmt.Errorf("store: load tags: %w", err)
		}
		results[i].Tags = tags
	}
	return results, nil
}

// ftsQuery turns user text into an FTS5 match expression. Each word becomes a
// quoted term so that punctuation and FTS5 operators in speech cannot produce a
// syntax error, and a trailing wildcard makes the last word a prefix match.
func ftsQuery(text string) string {
	fields := strings.Fields(text)
	terms := make([]string, 0, len(fields))
	for i, f := range fields {
		f = strings.Trim(f, `"`)
		f = strings.ReplaceAll(f, `"`, "")
		if f == "" {
			continue
		}
		term := `"` + f + `"`
		if i == len(fields)-1 {
			term += "*"
		}
		terms = append(terms, term)
	}
	return strings.Join(terms, " OR ")
}

// prefixed qualifies a comma-separated column list with a table alias.
func prefixed(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
