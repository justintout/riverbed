---
name: setup-riverbed
description: Install and configure Riverbed, the webhook receiver for a Pebble Index 01 voice recorder. Use when the user wants to set up, install, configure or deploy Riverbed, connect an Index 01 to a webhook, or add an agent, a Home Assistant integration or an MCP server to an existing Riverbed installation.
license: MIT
compatibility: Requires Go 1.25 or later to build, or Docker or Podman to run the image. Needs network access to fetch Go modules and, if embedding is enabled, the embedding model.
---

# Set up Riverbed

Riverbed receives recordings from a Pebble Index 01, stores them in SQLite, routes
the ones that ask for something to an agent, and makes all of them searchable.

`riverbed init` does the deterministic work: it generates tokens, writes the
configuration, and creates the database. Your job is to collect the few decisions
only the user can make, run `init` with them, verify the result, and then tell the
user the settings to enter on the device.

## Rules

1. **Never write a secret into `riverbed.toml`.** Configuration values refer to the
   environment as `${VAR}`. `init` writes generated tokens to `riverbed.env` with
   mode 0600 and adds it to `.gitignore`. Keep API keys in the environment too.
2. **An OAuth MCP server needs `store.secret_key`.** It encrypts the tokens in the
   database. `riverbed init` generates one into `riverbed.env`; `riverbed key` prints
   another. Riverbed refuses to start without it, and losing it means re-running
   `riverbed auth`.
3. **Do not hand-write the configuration.** Use `riverbed init` flags. If the user
   needs an option `init` does not cover, run `init` first, then edit the file and
   confirm with `riverbed migrate -config <file>`, which fails if the result is
   invalid.
4. **Do not invent configuration keys.** `riverbed.example.toml` in the repository
   root is the complete reference. Read it before you edit anything.
5. **Stop and ask** if a step fails twice. Do not try other commands in the hope
   that one works.

## Step 1: Ask the user

Ask these together, in one message, and offer the defaults. Do not ask anything you
can determine yourself.

| Question | Default | Why it is needed |
| --- | --- | --- |
| Which agent should handle spoken requests? | none | Claude, Gemini, an OpenAI-compatible endpoint such as DeepSeek or a local model, or an agent of their own over HTTP. "None" stores every recording and calls nothing. |
| Search by meaning as well as by keyword? | yes | Enables embedding. The default model is 31 MB and downloads once. |
| Control Home Assistant? | no | Needs the Home Assistant MCP Server integration and a long-lived access token. |
| Keep the audio, or only the transcription? | keep audio | Audio is stored in the database. |
| Is Riverbed reachable from the internet, and at which URL? | not reachable | Needed only for MCP servers that use OAuth. |
| Run from source, or in a container? | source | Both are supported. |

If the user says "set it up with sensible defaults", use: no agent, embedding on,
no Home Assistant, keep audio, and run from source. Then tell them how to add an
agent later.

## Step 2: Build

From the repository root:

```sh
make build
```

This produces `./riverbed`. If Go is missing, use the container route in
[references/DEPLOY.md](references/DEPLOY.md) instead.

## Step 3: Run init

Build the command from the answers. Every flag is optional; these are the ones that
matter.

```sh
./riverbed init \
  -agent-kind claude -agent-name shelley -agent-model claude-sonnet-5 \
  -agent-key-env ANTHROPIC_API_KEY \
  -embed-model potion-base-8M \
  -home-assistant-url http://homeassistant.local:8123/mcp_server/sse \
  -retain-audio
```

| Flag | Use |
| --- | --- |
| `-agent-kind` | `claude`, `gemini`, `openai` or `http`. Omit for no agent. |
| `-agent-name` | Defaults to the kind. This becomes the wake prefix, "hey <name>". |
| `-agent-model` | Required for `claude`, `gemini` and `openai`. |
| `-agent-base-url` | Required for `http`, and for `openai` against anything but OpenAI. |
| `-agent-key-env` | The environment variable holding the key. Never pass the key itself. |
| `-embed-model` | Pass `-embed-model ""` to keep retrieval keyword only. |
| `-home-assistant-url` | The SSE URL of the Home Assistant MCP Server integration. |
| `-base-url` | Required only if an MCP server will use OAuth. |
| `-print` | Show what would be written without writing it. Use this to preview. |
| `-force` | Overwrite an existing configuration. This generates new tokens. |

Run with `-print` first and show the user the configuration. Then run it for real.

`init` prints the tokens and the next steps. Keep that output; step 6 needs it.

## Step 4: Set the keys

If the user chose an agent that needs a key, or Home Assistant, add those to
`riverbed.env` so everything loads together:

```sh
printf 'ANTHROPIC_API_KEY=%s\n' "$KEY" >> riverbed.env
```

Ask the user to paste the key, or tell them to append it themselves if they would
rather not share it. Never print a key back to them, and never commit one.

## Step 5: Verify

Start the server, then prove the whole path works. Do not report success until this
passes.

```sh
set -a; . ./riverbed.env; set +a
./riverbed serve -config riverbed.toml &

curl -sf http://localhost:8080/healthz

curl -s -o /dev/null -w '%{http_code}\n' -X POST http://localhost:8080/webhook/recording \
  -H "Authorization: Bearer $RIVERBED_WEBHOOK_TOKEN" \
  -F "transcription=Note, this is a setup test" \
  -F "recordedAt=$(date +%s)000" \
  -F "client=ring"

./riverbed search -config riverbed.toml setup test
```

Expect `ok` from healthz, `202` from the webhook, and the note in the search output.

If embedding is on, test it with two notes rather than one. A hybrid query ranks
every stored note, so with a single note in the database it comes back whatever you
search for, which proves nothing.

```sh
for text in "Note, the back gate hinge is squeaking" "Note, renew the passport next month"; do
  curl -s -o /dev/null -X POST http://localhost:8080/webhook/recording     -H "Authorization: Bearer $RIVERBED_WEBHOOK_TOKEN"     -F "transcription=$text" -F "recordedAt=$(date +%s)000" -F "client=ring"
done
sleep 2
./riverbed search -config riverbed.toml squeaky door repair
```

The gate note must come first, although it shares no words with the query. If the
passport note comes first, embedding is not working. Check the startup log for
`embedding enabled`.

An unauthorized request must be refused. Check that too:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://localhost:8080/webhook/recording \
  -H "Authorization: Bearer wrong" -F "client=ring" -F "recordedAt=0"
```

Expect `401`.

## Step 6: Tell the user the device settings

You cannot configure the device. Give the user these values, taken from the `init`
output, and say where they go. On the phone, open the Pebble app, then the Index tab
settings.

- Webhook URL: `https://<their-host>/webhook/recording`
- Header: `Authorization: Bearer <the webhook token>`
- Send: transcription, or both audio and transcription
- Trigger: which gesture fires the webhook, which is their choice

If `mcp_serve` is enabled, the device can also search its own past notes. Under MCP
and Tool Settings, they create a sandbox group and add a server:

- URL: `https://<their-host>/mcp`
- Type: Streamable
- Authorization: `Bearer <the MCP token>`

Tell them the webhook must be reachable over HTTPS from the device, so a host behind
a home router needs a reverse proxy or a tunnel. See
[references/DEPLOY.md](references/DEPLOY.md).

## Step 7: Report

Summarize in a few lines: which files were written, whether embedding is on and with
which model, which agent is configured, what the verification showed, and what the
user still has to do themselves. List anything you could not verify.

## Adding to an existing installation

Do not re-run `init` on a configured system; it would replace the configuration and
the tokens. Edit `riverbed.toml` instead, using `riverbed.example.toml` as the
reference, then run `riverbed migrate -config riverbed.toml` to confirm it loads.

- Another agent: add an `[[agent]]` block, then a `[[router.rule]]` that routes to
  it. Use the `riverbed-routing` skill for the rule itself.
- An MCP server needing OAuth: add the server with `auth = "oauth"` and
  `transport = "streamable"`, set `server.base_url`, then run
  `riverbed auth <server-name>`, which prints a URL the user must open.
- Embedding on an existing database: set `[embedding]`, then run
  `riverbed backfill` to embed what is already stored.

## Further reading

- [references/DEPLOY.md](references/DEPLOY.md): systemd, Docker, Podman, reverse proxies.
- [references/TROUBLESHOOTING.md](references/TROUBLESHOOTING.md): what each failure means.
- The `riverbed-routing` skill: adding routing rules, including rules that match a
  note by meaning, and choosing a similarity threshold by measuring it.
