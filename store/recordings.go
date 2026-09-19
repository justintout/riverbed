package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	vector "github.com/justintout/go-sqlite-vector"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Status values for a recording.
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// Recording is one received recording.
type Recording struct {
	ID            int64
	Client        string
	RecordedAt    time.Time
	ReceivedAt    time.Time
	Transcription string
	AudioMIME     string
	AudioBytes    int64
	Status        string
	Attempts      int
	Route         string
	RouteReason   string
	Prompt        string
	Error         string
}

// NewRecording is an inbound recording, before storage.
type NewRecording struct {
	Client        string
	RecordedAt    time.Time
	Transcription string
	AudioMIME     string
	Audio         []byte
}

// Insert stores a recording and returns its id. A recording with no
// transcription is stored as done, since there is nothing to route.
func (s *Store) Insert(ctx context.Context, r NewRecording) (int64, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return 0, err
	}
	defer release()

	status := StatusPending
	if r.Transcription == "" {
		status = StatusDone
	}

	var id int64
	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return 0, fmt.Errorf("store: insert recording: %w", err)
	}
	err = func() error {
		if err := sqlitex.Execute(conn,
			`INSERT INTO recordings (client, recorded_at, received_at, transcription, audio_mime, audio_bytes, status)
			 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)`,
			&sqlitex.ExecOptions{Args: []any{
				r.Client, millis(r.RecordedAt), millis(time.Now()),
				nullable(r.Transcription), nullable(r.AudioMIME), int64(len(r.Audio)), status,
			}}); err != nil {
			return err
		}
		id = conn.LastInsertRowID()
		if len(r.Audio) > 0 {
			return sqlitex.Execute(conn,
				`INSERT INTO recording_audio (recording_id, audio) VALUES (?1, ?2)`,
				&sqlitex.ExecOptions{Args: []any{id, r.Audio}})
		}
		return nil
	}()
	end(&err)
	if err != nil {
		return 0, fmt.Errorf("store: insert recording: %w", err)
	}
	return id, nil
}

// Claim marks the oldest pending recording as running and returns it. It
// returns ErrNotFound when the queue is empty. Claiming in one immediate
// transaction is what lets several workers share the queue.
func (s *Store) Claim(ctx context.Context) (Recording, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return Recording{}, err
	}
	defer release()

	var rec Recording
	found := false
	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return Recording{}, fmt.Errorf("store: claim: %w", err)
	}
	err = func() error {
		if err := sqlitex.ExecuteTransient(conn,
			`SELECT `+recordingColumns+`
			 FROM recordings WHERE status = ?1 ORDER BY id LIMIT 1`,
			&sqlitex.ExecOptions{
				Args: []any{StatusPending},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					rec = scanRecording(stmt)
					found = true
					return nil
				},
			}); err != nil {
			return err
		}
		if !found {
			return nil
		}
		return sqlitex.Execute(conn,
			`UPDATE recordings
			 SET status = ?1, attempts = attempts + 1, started_at = ?2
			 WHERE id = ?3`,
			&sqlitex.ExecOptions{Args: []any{StatusRunning, millis(time.Now()), rec.ID}})
	}()
	end(&err)
	if err != nil {
		return Recording{}, fmt.Errorf("store: claim: %w", err)
	}
	if !found {
		return Recording{}, ErrNotFound
	}
	rec.Status = StatusRunning
	rec.Attempts++
	return rec, nil
}

// Requeue returns a claimed recording to the queue, so a restart or a transient
// failure does not lose it.
func (s *Store) Requeue(ctx context.Context, id int64) error {
	return s.exec(ctx, `UPDATE recordings SET status = ?1, started_at = NULL WHERE id = ?2`,
		StatusPending, id)
}

// RequeueRunning returns every running recording to the queue. It runs at
// startup to recover work interrupted by a restart.
func (s *Store) RequeueRunning(ctx context.Context) (int, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	if err := sqlitex.Execute(conn,
		`UPDATE recordings SET status = ?1, started_at = NULL WHERE status = ?2`,
		&sqlitex.ExecOptions{Args: []any{StatusPending, StatusRunning}}); err != nil {
		return 0, fmt.Errorf("store: requeue running: %w", err)
	}
	return conn.Changes(), nil
}

// SetRoute records the router's decision and the prompt the agent will see.
func (s *Store) SetRoute(ctx context.Context, id int64, route, reason, prompt string) error {
	return s.exec(ctx,
		`UPDATE recordings SET route = ?1, route_reason = ?2, prompt = ?3 WHERE id = ?4`,
		route, reason, nullable(prompt), id)
}

// Finish marks a recording done, or failed when err is not nil.
func (s *Store) Finish(ctx context.Context, id int64, failure error) error {
	status, message := StatusDone, any(nil)
	if failure != nil {
		status, message = StatusFailed, failure.Error()
	}
	return s.exec(ctx,
		`UPDATE recordings SET status = ?1, error = ?2, finished_at = ?3 WHERE id = ?4`,
		status, message, millis(time.Now()), id)
}

// Get returns one recording.
func (s *Store) Get(ctx context.Context, id int64) (Recording, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return Recording{}, err
	}
	defer release()

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
		return Recording{}, fmt.Errorf("store: get recording %d: %w", id, err)
	}
	if !found {
		return Recording{}, ErrNotFound
	}
	return rec, nil
}

// Audio returns the stored audio for a recording, or ErrNotFound.
func (s *Store) Audio(ctx context.Context, id int64) ([]byte, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var audio []byte
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT audio FROM recording_audio WHERE recording_id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				audio = make([]byte, stmt.ColumnLen(0))
				stmt.ColumnBytes(0, audio)
				return nil
			},
		}); err != nil {
		return nil, fmt.Errorf("store: audio %d: %w", id, err)
	}
	if audio == nil {
		return nil, ErrNotFound
	}
	return audio, nil
}

// AddTags attaches tags to a recording, ignoring duplicates.
func (s *Store) AddTags(ctx context.Context, id int64, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	conn, release, err := s.conn(ctx)
	if err != nil {
		return err
	}
	defer release()

	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return fmt.Errorf("store: add tags: %w", err)
	}
	err = func() error {
		for _, tag := range tags {
			if tag == "" {
				continue
			}
			if err := sqlitex.Execute(conn,
				`INSERT INTO tags (recording_id, tag) VALUES (?1, ?2) ON CONFLICT DO NOTHING`,
				&sqlitex.ExecOptions{Args: []any{id, tag}}); err != nil {
				return err
			}
		}
		return nil
	}()
	end(&err)
	if err != nil {
		return fmt.Errorf("store: add tags: %w", err)
	}
	return nil
}

// SetTranscription replaces a recording's text and drops its embeddings, which
// describe the old text. The keyword index follows by trigger.
func (s *Store) SetTranscription(ctx context.Context, id int64, text string) error {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return err
	}
	defer release()

	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return fmt.Errorf("store: set transcription: %w", err)
	}
	err = func() error {
		if err := sqlitex.Execute(conn, `UPDATE recordings SET transcription = ?1 WHERE id = ?2`,
			&sqlitex.ExecOptions{Args: []any{nullable(text), id}}); err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return ErrNotFound
		}
		return sqlitex.Execute(conn, `DELETE FROM embeddings WHERE recording_id = ?1`,
			&sqlitex.ExecOptions{Args: []any{id}})
	}()
	end(&err)
	if errors.Is(err, ErrNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("store: set transcription %d: %w", id, err)
	}
	return nil
}

// Replay puts a recording back in the queue as if it had just arrived, with a
// fresh allowance of attempts. Earlier replies and tool calls stay as history.
func (s *Store) Replay(ctx context.Context, id int64) error {
	return s.exec(ctx,
		`UPDATE recordings SET status = ?1, attempts = 0, started_at = NULL, error = NULL WHERE id = ?2`,
		StatusPending, id)
}

// RemoveTag detaches a tag from a recording. Removing a tag it does not carry
// is not an error.
func (s *Store) RemoveTag(ctx context.Context, id int64, tag string) error {
	return s.exec(ctx, `DELETE FROM tags WHERE recording_id = ?1 AND tag = ?2`, id, tag)
}

// Delete removes a recording with its audio, embeddings, replies, tool calls and
// tags. It returns ErrNotFound when the recording does not exist.
func (s *Store) Delete(ctx context.Context, id int64) error {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := sqlitex.Execute(conn, `DELETE FROM recordings WHERE id = ?1`,
		&sqlitex.ExecOptions{Args: []any{id}}); err != nil {
		return fmt.Errorf("store: delete recording %d: %w", id, err)
	}
	if conn.Changes() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountByStatus returns how many recordings hold each status.
func (s *Store) CountByStatus(ctx context.Context) (map[string]int, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	counts := map[string]int{}
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT status, COUNT(*) FROM recordings GROUP BY status`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			counts[stmt.ColumnText(0)] = int(stmt.ColumnInt64(1))
			return nil
		}}); err != nil {
		return nil, fmt.Errorf("store: count by status: %w", err)
	}
	return counts, nil
}

// Tags returns a recording's tags in alphabetical order.
func (s *Store) Tags(ctx context.Context, id int64) ([]string, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var tags []string
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT tag FROM tags WHERE recording_id = ?1 ORDER BY tag`,
		&sqlitex.ExecOptions{
			Args: []any{id},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				tags = append(tags, stmt.ColumnText(0))
				return nil
			},
		}); err != nil {
		return nil, fmt.Errorf("store: tags %d: %w", id, err)
	}
	return tags, nil
}

// Response is an agent's reply to a recording.
type Response struct {
	Agent        string
	Text         string
	InputTokens  int64
	OutputTokens int64
}

// AddResponse stores an agent reply.
func (s *Store) AddResponse(ctx context.Context, id int64, r Response) error {
	return s.exec(ctx,
		`INSERT INTO responses (recording_id, agent, text, input_tokens, output_tokens, created_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6)`,
		id, r.Agent, r.Text, r.InputTokens, r.OutputTokens, millis(time.Now()))
}

// Responses returns the replies stored for a recording, oldest first.
func (s *Store) Responses(ctx context.Context, id int64) ([]Response, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var out []Response
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT agent, text, input_tokens, output_tokens FROM responses
		 WHERE recording_id = ?1 ORDER BY id`,
		&sqlitex.ExecOptions{
			Args: []any{id},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				out = append(out, Response{
					Agent:        stmt.ColumnText(0),
					Text:         stmt.ColumnText(1),
					InputTokens:  stmt.ColumnInt64(2),
					OutputTokens: stmt.ColumnInt64(3),
				})
				return nil
			},
		}); err != nil {
		return nil, fmt.Errorf("store: responses %d: %w", id, err)
	}
	return out, nil
}

// ToolCall records one tool invocation made on a recording's behalf.
type ToolCall struct {
	Server    string
	Tool      string
	Arguments string
	Result    string
	IsError   bool
	StartedAt time.Time
	EndedAt   time.Time
}

// AddToolCall stores a tool invocation.
func (s *Store) AddToolCall(ctx context.Context, id int64, c ToolCall) error {
	return s.exec(ctx,
		`INSERT INTO tool_calls (recording_id, server, tool, arguments, result, is_error, started_at, ended_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)`,
		id, c.Server, c.Tool, nullable(c.Arguments), nullable(c.Result),
		c.IsError, millis(c.StartedAt), millis(c.EndedAt))
}

// ToolCalls returns the tool invocations for a recording, oldest first.
func (s *Store) ToolCalls(ctx context.Context, id int64) ([]ToolCall, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var out []ToolCall
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT server, tool, arguments, result, is_error, started_at, ended_at
		 FROM tool_calls WHERE recording_id = ?1 ORDER BY id`,
		&sqlitex.ExecOptions{
			Args: []any{id},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				out = append(out, ToolCall{
					Server:    stmt.ColumnText(0),
					Tool:      stmt.ColumnText(1),
					Arguments: stmt.ColumnText(2),
					Result:    stmt.ColumnText(3),
					IsError:   stmt.ColumnBool(4),
					StartedAt: time.UnixMilli(stmt.ColumnInt64(5)),
					EndedAt:   time.UnixMilli(stmt.ColumnInt64(6)),
				})
				return nil
			},
		}); err != nil {
		return nil, fmt.Errorf("store: tool calls %d: %w", id, err)
	}
	return out, nil
}

// PutEmbeddings replaces a recording's embeddings. Chunks and vectors must be
// the same length.
func (s *Store) PutEmbeddings(ctx context.Context, id int64, chunks []string, vectors [][]float32) error {
	if !s.VectorEnabled() {
		return errors.New("store: embeddings are not enabled")
	}
	if len(chunks) != len(vectors) {
		return fmt.Errorf("store: %d chunks but %d vectors", len(chunks), len(vectors))
	}
	conn, release, err := s.conn(ctx)
	if err != nil {
		return err
	}
	defer release()

	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return fmt.Errorf("store: put embeddings: %w", err)
	}
	err = func() error {
		if err := sqlitex.Execute(conn,
			`DELETE FROM embeddings WHERE recording_id = ?1`,
			&sqlitex.ExecOptions{Args: []any{id}}); err != nil {
			return err
		}
		for i, chunk := range chunks {
			if len(vectors[i]) != s.embedDim {
				return fmt.Errorf("chunk %d has %d dimensions, want %d", i, len(vectors[i]), s.embedDim)
			}
			if err := sqlitex.Execute(conn,
				`INSERT INTO embeddings (recording_id, chunk_index, text, vector) VALUES (?1, ?2, ?3, ?4)`,
				&sqlitex.ExecOptions{Args: []any{id, i, chunk, vector.Float32ToBlob(vectors[i])}}); err != nil {
				return err
			}
		}
		return nil
	}()
	end(&err)
	if err != nil {
		return fmt.Errorf("store: put embeddings: %w", err)
	}
	return nil
}

// PendingEmbedding returns ids of recordings that have a transcription but no
// embedding, oldest first. It is used to backfill after enabling embeddings.
func (s *Store) PendingEmbedding(ctx context.Context, limit int) ([]int64, error) {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var ids []int64
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT r.id FROM recordings r
		 WHERE r.transcription IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.recording_id = r.id)
		 ORDER BY r.id LIMIT ?1`,
		&sqlitex.ExecOptions{
			Args: []any{limit},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				ids = append(ids, stmt.ColumnInt64(0))
				return nil
			},
		}); err != nil {
		return nil, fmt.Errorf("store: pending embedding: %w", err)
	}
	return ids, nil
}

// exec runs one statement that returns no rows.
func (s *Store) exec(ctx context.Context, query string, args ...any) error {
	conn, release, err := s.conn(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

const recordingColumns = `id, client, recorded_at, received_at, transcription, audio_mime,
	audio_bytes, status, attempts, route, route_reason, prompt, error`

func scanRecording(stmt *sqlite.Stmt) Recording {
	return Recording{
		ID:            stmt.ColumnInt64(0),
		Client:        stmt.ColumnText(1),
		RecordedAt:    time.UnixMilli(stmt.ColumnInt64(2)),
		ReceivedAt:    time.UnixMilli(stmt.ColumnInt64(3)),
		Transcription: stmt.ColumnText(4),
		AudioMIME:     stmt.ColumnText(5),
		AudioBytes:    stmt.ColumnInt64(6),
		Status:        stmt.ColumnText(7),
		Attempts:      int(stmt.ColumnInt64(8)),
		Route:         stmt.ColumnText(9),
		RouteReason:   stmt.ColumnText(10),
		Prompt:        stmt.ColumnText(11),
		Error:         stmt.ColumnText(12),
	}
}

// nullable stores an empty string as SQL NULL, keeping "absent" distinct from
// "empty" in the database.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
