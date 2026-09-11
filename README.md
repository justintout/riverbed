# Riverbed

> A system to receive webhooks from a [Pebble Index 01](https://repebble.com/index) and react

Riverbed is the self-hosted other end of the Index 01's webhook. It receives every
recording, keeps the audio and transcription in one SQLite file, decides what each
utterance was for, and hands the ones that ask for something to an agent holding
tools from your own MCP servers. What it files away stays searchable by wording and
by meaning.

One static binary, no CGo, no sidecar services. Embeddings run inside the process.

## What it does

- **Receives** the device webhook exactly as the Index 01 sends it: a
  `multipart/form-data` POST carrying `audio`, `transcription`, `recordedAt` and
  `client`, guarded by a bearer token.
- **Stores** everything in one SQLite database: recordings, audio, agent replies,
  every tool call, and tags. One file to back up.
- **Routes** each transcription by spoken prefix, by regular expression, or with a
  small local model, because the webhook payload does not say which button was
  pressed.
- **Runs agents** with tools from your MCP servers. Claude, Gemini, any
  OpenAI-compatible endpoint (DeepSeek, llama.cpp, LM Studio, Ollama), or a
  self-hosted harness over plain HTTP.
- **Authenticates to MCP servers** with a static token or with full OAuth, which
  the device itself cannot do.
- **Retrieves** notes by keyword (FTS5), by meaning (vector search), or both fused
  together, filtered by time, route, tag, or whether a tool actually ran.
- **Serves its own journal over MCP**, so the device, or any agent, can search the
  notes it produced.

## Quick start

```sh
git clone https://github.com/justintout/riverbed
cd riverbed
make build

cp riverbed.example.toml riverbed.toml
$EDITOR riverbed.toml

export RIVERBED_WEBHOOK_TOKEN=$(openssl rand -hex 32)
export RIVERBED_MCP_TOKEN=$(openssl rand -hex 32)
./riverbed serve -config riverbed.toml
```

Then on the device, under the Index tab settings, set the webhook URL to
`https://your-host/webhook/recording`, add the header
`Authorization: Bearer <RIVERBED_WEBHOOK_TOKEN>`, and choose whether to send audio,
text, or both.

Confirm it end to end without the device:

```sh
curl -X POST http://localhost:8080/webhook/recording \
  -H "Authorization: Bearer $RIVERBED_WEBHOOK_TOKEN" \
  -F "transcription=Note, the back gate hinge is squeaking again" \
  -F "recordedAt=$(date +%s)000" \
  -F "client=ring"

./riverbed search -config riverbed.toml gate hinge
```

## Commands

| Command | Purpose |
| --- | --- |
| `riverbed serve` | Receive recordings and process them |
| `riverbed search [query...]` | Search notes; `-since`, `-tag`, `-route`, `-tool-used`, `-limit` |
| `riverbed auth <mcp-server>` | Run the OAuth flow for one MCP server and store the token |
| `riverbed backfill` | Embed recordings stored before embedding was enabled |
| `riverbed migrate` | Create or migrate the database, then exit |
| `riverbed version` | Print the version |

Every command takes `-config`, `-log-level` and `-log-format`.

## Routing

The webhook payload carries no button information, so the text decides. Rules are
tried in order and the first match wins; anything unmatched goes to the classifier
if one is configured, and otherwise to `router.default`.

```toml
[router]
default = "journal"      # store and embed, call nothing
classifier = "local"     # optional; a small local model resolves the rest

[[router.rule]]
prefix = "hey shelley"
agent = "shelley"
strip = true             # the agent sees the request without the wake words
tags = ["shelley"]

[[router.rule]]
regex = "^(turn|switch|set|dim) "
agent = "claude"
tags = ["home"]
```

Prefixes match against a normalized transcription, so casing and punctuation do not
matter and `note` does not match `nothing`. `journal` is a reserved route meaning
store without calling anything. A classifier failure falls through to the default
rather than losing a recording.

## Embedding and retrieval

Retrieval is keyword-only until an embedding model is named. Naming one turns on
vector storage and hybrid search.

| `kind` | Where it runs | Notes |
| --- | --- | --- |
| `potion` | In process | Static embeddings, sub-millisecond, 8–131 MB models. The default. |
| `goformer` | In process | BERT embeddings from a HuggingFace safetensors directory. Slower, more contextual. |
| `remote` | Over HTTP | Any OpenAI-compatible `/v1/embeddings` endpoint. |

```toml
[embedding]
kind = "potion"
model = "potion-base-8M"
```

Keyword matching uses FTS5 with `bm25` ranking. Vector matching uses
[go-sqlite-vector](https://github.com/justintout/go-sqlite-vector). When a query has
both, the two rankings are fused with reciprocal rank fusion rather than by mixing
scores, because `bm25` values and vector distances share no scale. A hybrid query
therefore ranks every embedded note; use `-limit` to control how many come back.

The model and its dimension are pinned in the database on first use. Pointing a
populated database at a different model is refused, because the stored vectors
would no longer be comparable. After enabling embedding on an existing database,
run `riverbed backfill`.

`potion` downloads and caches its model on first use. Set `GO_POTION_HOME` to keep
that cache on a persistent volume.

## MCP servers

Each server's tools are offered to the agents you list, under the qualified name
`server.tool`, so two servers may offer the same tool name. An unreachable server
is logged and skipped rather than breaking the run.

```toml
[[mcp]]
name = "homeassistant"
url = "http://homeassistant.example.lan:8123/mcp_server/sse"
transport = "sse"
auth = "bearer"
token = "${HOMEASSISTANT_TOKEN}"
agents = ["claude"]
```

Home Assistant's own MCP Server integration speaks SSE with a long-lived access
token, so controlling your devices needs no special support here.

### OAuth

The device supports only a static `Authorization` header. Riverbed handles OAuth
itself: discovery, dynamic client registration and PKCE come from the MCP Go SDK,
and Riverbed stores the tokens and refreshes them.

```toml
[server]
base_url = "https://riverbed.example.com"   # the redirect is built under this

[[mcp]]
name = "example-oauth"
url = "https://mcp.example.com/mcp"
transport = "streamable"                     # oauth requires streamable
auth = "oauth"
scopes = ["read", "write"]
agents = ["claude"]
```

Authorize once, interactively:

```sh
riverbed auth example-oauth
```

It prints a URL, serves the redirect at `/oauth/callback/<server>`, and stores the
token. Refreshed tokens are written back, so a restart does not need a browser
again. The daemon never blocks on a browser: a server with no usable token logs
which `riverbed auth` command to run.

## Serving the journal over MCP

With `mcp_serve` enabled, Riverbed is itself an MCP server offering
`search_journal` and `recent_notes`. Point the device's MCP sandbox at
`https://your-host/mcp` with the token as its `Authorization` header, and the
assistant can search the notes it recorded.

```toml
[mcp_serve]
enabled = true
path = "/mcp"
token = "${RIVERBED_MCP_TOKEN}"
```

## Agents

```toml
[[agent]]
name = "claude"
kind = "claude"
model = "claude-sonnet-5"
api_key = "${ANTHROPIC_API_KEY}"
```

`kind` is one of:

- `claude` — the Anthropic Messages API.
- `gemini` — the Gemini API.
- `openai` — any OpenAI-compatible endpoint. Set `base_url` for DeepSeek,
  llama.cpp, LM Studio, Ollama or vLLM.
- `http` — your own harness. Riverbed posts
  `{"prompt": ..., "system": ..., "source": "riverbed"}` and reads the reply from
  plain text or from a `reply`, `text`, `response`, `content` or `message` field.
  The harness owns its own tools; Riverbed does not drive a tool loop for it.

The first three run the tool loop themselves, capped by `max_turns`.

## Docker and Podman

```sh
docker build -t riverbed .
docker run --rm -p 8080:8080 \
  -v riverbed-data:/var/lib/riverbed \
  -v "$PWD/riverbed.toml:/etc/riverbed/riverbed.toml:ro" \
  -e RIVERBED_WEBHOOK_TOKEN -e RIVERBED_MCP_TOKEN \
  riverbed
```

Or `docker compose up` / `podman-compose up` with the included `compose.yaml`. The
image is `distroless/static:nonroot`: no shell, no libc, runs as a non-root user.
Every command above works unchanged with `podman`.

Keep `/var/lib/riverbed` on a volume. It holds the database and the cached
embedding model.

## Configuration

The full reference with every option is [`riverbed.example.toml`](riverbed.example.toml).

Any double-quoted value may reference an environment variable as `${VAR}`, which
keeps secrets out of the file. A reference to an unset variable is a startup error
rather than an empty value. References in comments and in single-quoted strings are
left alone.

These environment variables override the file, which is enough to run a container
without one: `RIVERBED_CONFIG`, `RIVERBED_ADDR`, `RIVERBED_BASE_URL`,
`RIVERBED_DB`, `RIVERBED_WEBHOOK_TOKEN`, `RIVERBED_MCP_TOKEN`,
`RIVERBED_EMBED_KIND`, `RIVERBED_EMBED_MODEL`, `RIVERBED_EMBED_BASE_URL`,
`RIVERBED_EMBED_API_KEY`, `RIVERBED_LOG_LEVEL`, `RIVERBED_LOG_FORMAT`.

The whole configuration is validated at startup and every problem is reported at
once. There are no fallback paths: a misconfiguration stops the process.

## Building

```sh
make            # vet, test, build
make test
make dist       # static binaries for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
```

Because nothing links libc, one Linux binary per architecture covers Debian,
Ubuntu, Fedora, Alpine and the rest. Building for another platform needs no
toolchain for it.

## Packages

Riverbed is usable as a library. Each package stands on its own:

| Package | Purpose |
| --- | --- |
| `config` | Load and validate the configuration |
| `store` | SQLite schema, migrations, the work queue, hybrid retrieval |
| `webhook` | The device's multipart receiver as an `http.Handler` |
| `embedding` | `Embedder` with in-process and remote implementations |
| `route` | Prefix, regex and classifier routing |
| `agent` | The `Agent` interface and its four providers |
| `tool` | MCP client registry, bearer and OAuth authentication |
| `mcpserve` | The journal as an MCP server |
| `pipeline` | Claim, route, embed, run, persist |

## How recordings are processed

The receiver stores the recording and answers `202` immediately, because the device
is waiting. A worker then picks it up: the recordings table is the queue, so a
claim is one transaction, several workers share it, and a restart recovers work in
flight instead of losing it. A failed recording is retried, and a recording that
keeps failing is marked failed with the reason kept. A failed embedding costs
retrieval quality, not the recording.

## License

MIT. See [LICENSE](LICENSE).
