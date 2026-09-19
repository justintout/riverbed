# Riverbed

> A system to receive webhooks from my [Pebble Index 01](https://repebble.com/index) and react

Riverbed receives the recordings that a Pebble Index 01 sends to a webhook. It
stores each recording and its transcription in a SQLite database, decides which
transcriptions need an agent, and sends those to the agent you configure. The agent
can use tools from your own MCP servers. You can search the stored transcriptions by
keyword, and by meaning if you configure an embedding model.

Riverbed builds to a single static binary. It does not use CGo, and it does not
require another service at run time.

## Features

- Receives the Index 01 webhook in the format the device sends: a
  `multipart/form-data` POST with the fields `audio`, `transcription`, `recordedAt`
  and `client`. A bearer token protects the endpoint.
- Keeps recordings, audio, agent replies, tool calls and tags in one SQLite
  database. The database is a single file, which makes backup simple.
- Receives everything the Index sends, and selects a route for each transcription
  with a spoken prefix, a regular expression, or by meaning. Matching by meaning
  compares the note to example phrases you supply, so it needs no model call. A
  small local model can resolve whatever is left.
- Runs agents with tools from your MCP servers. Supported agents are Claude,
  Gemini, any OpenAI-compatible endpoint such as DeepSeek, llama.cpp, LM Studio or
  Ollama, and your own agent over HTTP.
- Authenticates to MCP servers with a static token or with OAuth. The device
  supports static tokens only.
- Searches transcriptions with keywords (FTS5). If you configure an embedding
  model, it also searches by meaning, and combines the two rankings. You can filter
  by time, route, tag, or whether a tool ran.
- Serves its own stored transcriptions as an MCP server, so the device or another
  agent can search them.

## Requirements

Go 1.25 or later to build. No other dependency is needed at run time.

## Quick start

### With a coding agent

The repository ships a setup skill, so you can clone it and ask your agent to do the
work:

```sh
git clone https://github.com/justintout/riverbed
cd riverbed
```

Then tell the agent: **"set up Riverbed"**. It asks which agent should handle spoken
requests, whether to enable search by meaning, and whether to connect Home
Assistant. It then runs `riverbed init`, verifies the result with a test recording,
and tells you the settings to enter on the device.

Two skills ship with the repository, both in the
[Agent Skills](https://agentskills.io) format, so any client that reads skills can
use them:

| Skill | Ask for |
| --- | --- |
| `setup-riverbed` | Installing and configuring Riverbed, and adding an agent or an MCP server later |
| `riverbed-routing` | Adding routing rules, matching notes by meaning, and choosing a similarity threshold |

They live in `.agents/skills/`, which is the cross-client location.
`.claude/skills/` holds a symlink to each one, because Claude Code reads only its
own directory.

### By hand

```sh
git clone https://github.com/justintout/riverbed
cd riverbed
make build

./riverbed init -embed-model potion-base-8M

set -a; . ./riverbed.env; set +a
./riverbed serve -config riverbed.toml
```

`riverbed init` writes `riverbed.toml`, writes the generated tokens to
`riverbed.env` with mode 0600, adds that file to `.gitignore`, and creates the
database. Run it with `-print` first to see what it would write. Pass `-agent-kind`
to configure an agent, and see `riverbed init -h` for the rest.

To write the configuration yourself instead, copy `riverbed.example.toml` to
`riverbed.toml` and edit it.

Then configure the device. In the Index tab settings, set the webhook URL to
`https://your-host/webhook/recording` and add the header
`Authorization: Bearer <RIVERBED_WEBHOOK_TOKEN>`. Select whether the device sends
audio, text, or both.

To test the system without the device, send an equivalent request:

```sh
curl -X POST http://localhost:8080/webhook/recording \
  -H "Authorization: Bearer $RIVERBED_WEBHOOK_TOKEN" \
  -F "transcription=Note, the back gate hinge is squeaking again" \
  -F "recordedAt=$(date +%s)000" \
  -F "client=ring"

./riverbed search -config riverbed.toml gate hinge
```

## Commands

| Command | Function |
| --- | --- |
| `riverbed init` | Write a configuration file, generate the tokens, and create the database |
| `riverbed serve` | Receive recordings and process them |
| `riverbed search [query...]` | Search transcriptions. Flags: `-since`, `-tag`, `-route`, `-tool-used`, `-limit` |
| `riverbed route <text>` | Show how a transcription would be routed, with every semantic score. Stores nothing |
| `riverbed auth <mcp-server>` | Do the OAuth flow for one MCP server and store the token |
| `riverbed backfill` | Embed recordings that were stored before you enabled embedding |
| `riverbed migrate` | Create or migrate the database, then stop |
| `riverbed key` | Print a new secret key for `store.secret_key` |
| `riverbed version` | Print the version |

All commands accept `-config`, `-log-level` and `-log-format`.

## Routing

Riverbed receives everything the Index sends, and the payload carries only the
transcription, the timestamp and the client name. The text is therefore the only
thing available to route on.

Three mechanisms are available, and Riverbed tries them in increasing order of cost:

1. **Prefix and regular expression rules**, in configuration order. These are exact
   and free, so they are tried first and the first match wins.
2. **Semantic rules**, which carry example utterances. Riverbed embeds the
   transcription once and compares it to every example by cosine similarity. The
   highest scoring rule wins if it reaches its threshold. This needs no model call.
3. **A classifier agent**, if you configure one, for whatever the rules did not
   match.

If none of them decides, `router.default` applies.

```toml
[router]
default = "journal"      # store and embed, call no agent
classifier = "local"     # optional small local model for unmatched text

[[router.rule]]
prefix = "hey shelley"
agent = "shelley"
strip = true             # remove the prefix before the agent sees the text
tags = ["shelley"]

[[router.rule]]
regex = "^(turn|switch|set|dim) "
agent = "claude"
tags = ["home"]
```

Riverbed normalizes the transcription before it applies a rule. Letter case and
punctuation therefore have no effect, and the prefix `note` does not match the word
`nothing`. The route name `journal` is reserved. It stores and embeds the recording
and calls no agent. If the classifier fails, Riverbed uses the default route and
keeps the recording.

### Routing by meaning

A semantic rule lists examples of what its route handles. Any transcription close
enough to one of them takes that route, so you do not have to predict the exact
words you will say, and no model is called.

```toml
[router]
default = "journal"
semantic_threshold = 0.45      # the similarity a rule needs, unless it sets its own

[[router.rule]]
utterances = [
  "turn on the kitchen lights",
  "dim the lamp in the bedroom",
  "switch off the porch light",
  "close the blinds",
]
agent = "home"
tags = ["home"]
threshold = 0.5                # optional, overrides semantic_threshold
```

Semantic rules require an embedding model, because the comparison is between
embeddings. Riverbed embeds the utterances once at startup, so routing a recording
costs one embedding of the transcription, which is well under a millisecond with
`potion`.

**The right threshold depends on the model**, so measure it rather than guess.
`riverbed route` reports the decision and the score of every semantic rule without
storing anything:

```
$ riverbed route -config riverbed.toml "kill the lights in the kitchen please"
"kill the lights in the kitchen please"
  route:  home
  reason: rule 1 semantic 0.717 "turn on the kitchen lights"
  tags:   home
  semantic scores:
    * home         0.717  (threshold 0.45, closest "turn on the kitchen lights")
      assistant    0.033  (threshold 0.45, closest "what is on my calendar today")
```

Pass `-` to read lines from standard input, which is the quickest way to check a
batch of real phrasings at once.

With `potion-base-8M`, phrasings that should match score around 0.58 to 0.79 and
unrelated notes score between 0.03 and 0.20, so a threshold near 0.45 separates them.
A contextual model such as `goformer` scores unrelated text higher, so its threshold
has to be higher. Measure your own.

When a phrasing you expect to match falls short, the usual fix is another utterance
rather than a lower threshold. `"when am I meeting the contractor"` scored 0.407
against a calendar rule and fell through to the journal; adding the example
`"when am I meeting someone"` took the same phrase to 0.639.

An exact prefix or regular expression match always wins over a semantic match, even
when the semantic rule also clears its threshold, so a wake word stays reliable.

For help writing utterances and calibrating a threshold, ask your agent to use the
`riverbed-routing` skill.

## Embedding and retrieval

Retrieval uses keywords only until you configure an embedding model. A configured
model also enables vector storage and hybrid search.

The `potion` and `goformer` embedders run inside the Riverbed process. There is
therefore no embedding service to install and keep running next to Riverbed, no API
key to hold, and the transcriptions stay on the host. Use `remote` if you would
rather call a model you already run elsewhere.

| `kind` | Location | Description |
| --- | --- | --- |
| `potion` | In process | Static embeddings. Less than one millisecond for each note. Models are 8 MB to 131 MB. This is the default `kind`. |
| `goformer` | In process | BERT embeddings from a HuggingFace safetensors directory. Slower, and better on longer text. |
| `remote` | HTTP | Any OpenAI-compatible `/v1/embeddings` endpoint. |

```toml
[embedding]
kind = "potion"
model = "potion-base-8M"
```

The `potion` embedder downloads its model from HuggingFace at first use and caches
it, so the first start needs network access. Set `GO_POTION_HOME` to keep the cache
on a persistent volume. For `goformer`, you supply the model directory yourself.

Keyword search uses FTS5 with `bm25` ranking. Vector search uses
[go-sqlite-vector](https://github.com/justintout/go-sqlite-vector). If a query has
text and a vector, Riverbed combines the two rankings with reciprocal rank fusion.
It does not add the two scores together, because `bm25` values and vector distances
use different scales. A hybrid query ranks every embedded note, so use `-limit` to
control how many results you get.

Riverbed records the model name and the vector dimension in the database when you
first use them. If you then configure a different model, Riverbed stops with an
error, because the stored vectors are no longer comparable. To embed recordings that
you stored before you enabled embedding, run `riverbed backfill`.


## MCP servers

Riverbed offers the tools of each MCP server to the agents that you list. Tool names
are qualified as `server__tool`, so two servers can offer a tool with the same name.
If a server is unavailable, Riverbed writes a log entry and continues with the
remaining servers.

```toml
[[mcp]]
name = "homeassistant"
url = "http://homeassistant.example.lan:8123/mcp_server/sse"
transport = "sse"
auth = "bearer"
token = "${HOMEASSISTANT_TOKEN}"
agents = ["claude"]
```

The Home Assistant MCP Server integration uses SSE with a long-lived access token.
Riverbed needs no additional configuration to control Home Assistant devices.

### OAuth

The device can send a static `Authorization` header only. Riverbed does the OAuth
flow itself. The MCP Go SDK does the discovery, the dynamic client registration and
PKCE. Riverbed stores the tokens and refreshes them.

```toml
[server]
base_url = "https://riverbed.example.com"   # Riverbed builds the redirect under this URL

[[mcp]]
name = "example-oauth"
url = "https://mcp.example.com/mcp"
transport = "streamable"                     # OAuth requires the streamable transport
auth = "oauth"
scopes = ["read", "write"]
agents = ["claude"]
```

Authorize the server once:

```sh
riverbed auth example-oauth
```

The command prints a URL, serves the redirect at `/oauth/callback/<server>`, and
stores both the token and what registration produced: the client credentials and the
token endpoint.

Storing the registration is what lets a restart refresh on its own. Riverbed rebuilds
an OAuth client from it, so an expired access token is exchanged for a new one
against the token endpoint without a browser, and the refreshed token is written back.
It also means Riverbed registers itself once with an authorization server rather than
once per authorization.

The daemon never waits for a browser. If a server has no usable token, it logs the
`riverbed auth` command to run and carries on with the other servers.

The access token, the refresh token and the client secret are encrypted before they
are written, with AES-256-GCM under `store.secret_key`. A copy of the database
therefore does not hand over a working credential. `riverbed init` generates a key,
and `riverbed key` prints a new one:

```toml
[store]
secret_key = "${RIVERBED_SECRET_KEY}"
```

The key is required as soon as an MCP server uses OAuth, and Riverbed refuses to
start without it. Keep it out of the database and out of the configuration file, which
is why it is read from the environment. Losing it costs you the stored tokens: run
`riverbed auth <server>` again.

Encryption here protects the file, not the host. The daemon is unattended, so the key
has to be readable by the process, which means anyone who can run code as the service
user can read the credentials. It defends a stolen disk, a copied backup and a
discarded drive. For the rest of the database, which holds your transcriptions and
audio in the clear, use full disk encryption and encrypt your backups.

If an authorization server changes and the stored registration stops working, discard
it with `riverbed auth -reset-client <server>`, which registers a new client. Plain
`-reset` discards only the token and keeps the registration.

## Serving the transcriptions over MCP

If you enable `mcp_serve`, Riverbed also operates as an MCP server. It offers the
tools `search_journal` and `recent_notes`. Set the device MCP sandbox to
`https://your-host/mcp` and use the token as its `Authorization` header. The
assistant on the device can then search the transcriptions it recorded.

```toml
[mcp_serve]
enabled = true
path = "/mcp"
token = "${RIVERBED_MCP_TOKEN}"
```

## Web interface

Set `ui.enabled` and Riverbed serves a small web page at the root of its listener.
It lists recent notes and searches them by keyword and by meaning. A note's page
plays the audio when Riverbed kept it, and shows the agent replies and tool calls.
From there you can:

- edit the text, and optionally replay the note after saving;
- replay a finished note, or retry a failed one;
- add or remove tags, or delete the note.

The note list has a box for adding a note by typing it. A typed note has no audio, and it goes
through routing and embedding like a spoken one. Replaying runs the route and the
agent again, so an agent that acts on the world may act twice. Earlier replies and
tool calls stay on the note as history.

The Status page shows queue counts and the routing, agents and MCP servers in
effect. The Config page edits `riverbed.toml` as text. Riverbed checks the text as
it would at startup and refuses to save it if it is invalid. Saving does not change
the running system: restart Riverbed to apply it. The Config page needs a password
and a configuration file, because the file decides where credentials are sent.

```toml
[ui]
enabled = true
password = "${RIVERBED_UI_PASSWORD}"
```

With no `password`, anyone who can reach the listener can read and delete notes.
Riverbed logs a warning at startup in that case. Set one when the listener is
reachable beyond a private network. A login lasts 30 days and ends when Riverbed
restarts. Serve the page over HTTPS, because the password travels in the login
request.

## Agents

```toml
[[agent]]
name = "claude"
kind = "claude"
model = "claude-sonnet-5"
api_key = "${ANTHROPIC_API_KEY}"
```

Set `kind` to one of these values:

| `kind` | Description |
| --- | --- |
| `claude` | The Anthropic Messages API. |
| `gemini` | The Gemini API. |
| `openai` | Any OpenAI-compatible endpoint. Set `base_url` for DeepSeek, llama.cpp, LM Studio, Ollama or vLLM. |
| `http` | Your own agent. See below. |

The `claude`, `gemini` and `openai` agents do their own tool-use loop. The
`max_turns` option limits the number of turns.

For the `http` agent, Riverbed sends
`{"prompt": ..., "system": ..., "source": "riverbed"}` to the configured URL. It
reads the reply from plain text, or from a `reply`, `text`, `response`, `content` or
`message` field. This agent keeps its own tools, and Riverbed does not run a tool
loop for it.

## Docker and Podman

```sh
docker build -t riverbed .
docker run --rm -p 8080:8080 \
  -v riverbed-data:/var/lib/riverbed \
  -v "$PWD/riverbed.toml:/etc/riverbed/riverbed.toml:ro" \
  -e RIVERBED_WEBHOOK_TOKEN -e RIVERBED_MCP_TOKEN \
  riverbed
```

You can also use `docker compose up` or `podman-compose up` with the supplied
`compose.yaml`. The image is based on `distroless/static:nonroot`. It contains no
shell and no libc, and the container runs as a non-root user. All of these commands
also work with `podman`.

Keep `/var/lib/riverbed` on a volume. It contains the database and the cached
embedding model.

## Configuration

[`riverbed.example.toml`](riverbed.example.toml) documents every option.

In a double-quoted value, you can refer to an environment variable as `${VAR}`. This
keeps secrets out of the file. If the variable is not set, Riverbed stops with an
error and does not substitute an empty value. Riverbed ignores these references in
comments and in single-quoted strings.

These environment variables replace the values in the file. They are sufficient to
run a container without a configuration file: `RIVERBED_CONFIG`, `RIVERBED_ADDR`,
`RIVERBED_BASE_URL`, `RIVERBED_DB`, `RIVERBED_WEBHOOK_TOKEN`, `RIVERBED_MCP_TOKEN`, `RIVERBED_UI_PASSWORD`,
`RIVERBED_EMBED_KIND`, `RIVERBED_EMBED_MODEL`, `RIVERBED_EMBED_BASE_URL`,
`RIVERBED_EMBED_API_KEY`, `RIVERBED_SECRET_KEY`, `RIVERBED_LOG_LEVEL`,
`RIVERBED_LOG_FORMAT`.

Riverbed validates all configuration at start up and reports every problem
together. It contains no fallback paths. If the configuration is not valid, the
process stops.

## Building

```sh
make help       # list every target
make            # vet, test, build
make test
make dist       # static binaries for linux and darwin, amd64 and arm64
```

The binaries do not link libc, so one Linux binary for each architecture runs on
Debian, Ubuntu, Fedora, Alpine and other distributions. To build for a different
platform, you do not need a toolchain for that platform.

## Packages

You can also use Riverbed as a library. Each package is usable on its own.

| Package | Function |
| --- | --- |
| `config` | Load and validate the configuration |
| `store` | SQLite schema, migrations, the work queue and retrieval |
| `webhook` | The device webhook receiver, as an `http.Handler` |
| `embedding` | The `Embedder` interface, with in-process and remote implementations |
| `route` | Prefix, regular expression and classifier routing |
| `agent` | The `Agent` interface and the four agent types |
| `tool` | The MCP client registry, with token and OAuth authentication |
| `mcpserve` | The stored transcriptions, as an MCP server |
| `pipeline` | Claim, route, embed, run and store |

## How Riverbed processes a recording

The receiver stores the recording and answers with status 202 immediately, because
the device waits for the response. A worker then processes the recording. The
recordings table is also the work queue. A worker claims a recording in a single
transaction, so several workers can share the queue, and a restart recovers the
recordings that were in progress. If processing fails, Riverbed tries again. If it
continues to fail, Riverbed marks the recording as failed and keeps the error
message. If embedding fails, Riverbed writes a log entry and keeps the recording.
Retrieval quality decreases, but no data is lost.

## License

MIT. See [LICENSE](LICENSE).
