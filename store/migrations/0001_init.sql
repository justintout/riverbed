-- Recordings received from the device, and everything derived from them.

CREATE TABLE recordings (
    id            INTEGER PRIMARY KEY,
    client        TEXT    NOT NULL,
    recorded_at   INTEGER NOT NULL, -- unix milliseconds, from the device
    received_at   INTEGER NOT NULL, -- unix milliseconds, from this host
    transcription TEXT,
    audio_mime    TEXT,
    audio_bytes   INTEGER NOT NULL DEFAULT 0,
    status        TEXT    NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'running', 'done', 'failed')),
    attempts      INTEGER NOT NULL DEFAULT 0,
    route         TEXT,   -- agent chosen by the router
    route_reason  TEXT,   -- which rule, classifier, or default decided it
    prompt        TEXT,   -- transcription after any prefix was stripped
    error         TEXT,
    started_at    INTEGER,
    finished_at   INTEGER
);

CREATE INDEX recordings_status ON recordings (status, id);
CREATE INDEX recordings_recorded_at ON recordings (recorded_at DESC);
CREATE INDEX recordings_route ON recordings (route);

-- Audio is kept apart so scanning recordings never pages blobs in.
CREATE TABLE recording_audio (
    recording_id INTEGER PRIMARY KEY REFERENCES recordings (id) ON DELETE CASCADE,
    audio        BLOB NOT NULL
);

-- Keyword retrieval. External content: the text lives in recordings.
CREATE VIRTUAL TABLE recordings_fts USING fts5 (
    transcription,
    content='recordings',
    content_rowid='id',
    tokenize='porter unicode61'
);

CREATE TRIGGER recordings_fts_insert AFTER INSERT ON recordings
WHEN new.transcription IS NOT NULL BEGIN
    INSERT INTO recordings_fts (rowid, transcription) VALUES (new.id, new.transcription);
END;

CREATE TRIGGER recordings_fts_delete AFTER DELETE ON recordings
WHEN old.transcription IS NOT NULL BEGIN
    INSERT INTO recordings_fts (recordings_fts, rowid, transcription)
    VALUES ('delete', old.id, old.transcription);
END;

CREATE TRIGGER recordings_fts_update AFTER UPDATE OF transcription ON recordings BEGIN
    INSERT INTO recordings_fts (recordings_fts, rowid, transcription)
    SELECT 'delete', old.id, old.transcription WHERE old.transcription IS NOT NULL;
    INSERT INTO recordings_fts (rowid, transcription)
    SELECT new.id, new.transcription WHERE new.transcription IS NOT NULL;
END;

-- Vector retrieval. Rows exist only while an embedder is configured.
CREATE TABLE embeddings (
    recording_id INTEGER NOT NULL REFERENCES recordings (id) ON DELETE CASCADE,
    chunk_index  INTEGER NOT NULL,
    text         TEXT    NOT NULL,
    vector       BLOB    NOT NULL,
    PRIMARY KEY (recording_id, chunk_index)
) WITHOUT ROWID;

-- Agent replies.
CREATE TABLE responses (
    id           INTEGER PRIMARY KEY,
    recording_id INTEGER NOT NULL REFERENCES recordings (id) ON DELETE CASCADE,
    agent        TEXT    NOT NULL,
    text         TEXT    NOT NULL,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
);

CREATE INDEX responses_recording ON responses (recording_id);

-- Every tool an agent invoked, so "was a tool used" is queryable.
CREATE TABLE tool_calls (
    id           INTEGER PRIMARY KEY,
    recording_id INTEGER NOT NULL REFERENCES recordings (id) ON DELETE CASCADE,
    server       TEXT    NOT NULL,
    tool         TEXT    NOT NULL,
    arguments    TEXT,
    result       TEXT,
    is_error     INTEGER NOT NULL DEFAULT 0,
    started_at   INTEGER NOT NULL,
    ended_at     INTEGER NOT NULL
);

CREATE INDEX tool_calls_recording ON tool_calls (recording_id);
CREATE INDEX tool_calls_tool ON tool_calls (server, tool);

CREATE TABLE tags (
    recording_id INTEGER NOT NULL REFERENCES recordings (id) ON DELETE CASCADE,
    tag          TEXT    NOT NULL,
    PRIMARY KEY (recording_id, tag)
) WITHOUT ROWID;

CREATE INDEX tags_tag ON tags (tag);

-- OAuth tokens for MCP servers that need them.
CREATE TABLE oauth_tokens (
    server        TEXT PRIMARY KEY,
    access_token  TEXT NOT NULL,
    refresh_token TEXT NOT NULL DEFAULT '',
    token_type    TEXT NOT NULL DEFAULT 'Bearer',
    expiry        INTEGER NOT NULL DEFAULT 0, -- unix milliseconds, 0 = no expiry
    updated_at    INTEGER NOT NULL
) WITHOUT ROWID;

-- The meta table is created by the migration runner before any migration
-- applies, because it holds the schema version itself.
