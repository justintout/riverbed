# Troubleshooting Riverbed

## The server will not start

**`unset environment variables: X`**
The configuration refers to `${X}` and the variable is not set. Load the secrets
file: `set -a; . ./riverbed.env; set +a`. Riverbed refuses to start rather than use
an empty secret.

**`webhook.token is required`**
The token is empty. Either the variable is unset, or the configuration was edited by
hand. Regenerate with `riverbed init -force`, or set the variable.

**`database holds X embeddings of N dimensions but Y of M dimensions are configured`**
The embedding model changed. Stored vectors from two models cannot be compared.
Either restore the original model in the configuration, or start a new database file
and re-embed with `riverbed backfill`.

**`agent "X" is not configured` at startup**
`router.default` or a rule names an agent with no `[[agent]]` block. Agent names are
case sensitive. The name `journal` is reserved and needs no block.

## The device sends recordings but nothing is stored

Check what reaches Riverbed:

```sh
journalctl -u riverbed -f          # systemd
docker logs -f riverbed           # container
```

**No log line at all.** The request is not arriving. Check the public URL, the
reverse proxy, and that the device has network access. Confirm from outside the
network: `curl -sv https://your-host/healthz`.

**`401`.** The token does not match. The device header must be
`Authorization: Bearer <token>`, and the token must equal `RIVERBED_WEBHOOK_TOKEN`.
A trailing space or a newline in the environment file breaks it.

**`400`.** The body is not what Riverbed expects. The message names the field. The
device always sends `recordedAt` and `client`, so a missing one means something
between the device and Riverbed rewrote the request. Check that the proxy does not
buffer or alter `multipart/form-data`.

**`413` or a size error.** The audio is larger than `audio.max_bytes`. Raise it, or
set `audio.retain = false`.

## Recordings are stored but not acted on

Look at the route each recording took:

```sh
riverbed search -config riverbed.toml -limit 5
```

Each result shows the route in brackets. `[journal]` means Riverbed stored it and
called no agent.

- **Everything goes to `journal`.** No rule matched and no classifier is set. Check
  the spoken prefix against the rule. Prefixes match after normalization, so case and
  punctuation do not matter, but the words must be right, and they must be at the
  start.
- **The status is `failed`.** The agent could not be reached, or returned an error.
  The stored error says which. Riverbed tries three times before it fails a
  recording.
- **The transcription is empty.** The device sent audio only, or transcription
  failed on the device. Riverbed stores the audio and marks the recording done,
  because there is no text to route.

## Search returns nothing useful

- **Keyword search misses an obvious word.** FTS5 matches whole words with a prefix
  match on the last word only. Search for fewer, more distinctive words.
- **Search by meaning does not work.** Confirm embedding is on. The startup log
  says `embedding enabled` with the model and dimension, or `embedding disabled`.
  Recordings stored before you enabled it have no vectors until you run
  `riverbed backfill`.
- **Every note comes back for any query.** Expected with a hybrid query: vector
  search ranks everything, so results are ordered by relevance rather than filtered.
  Use `-limit`.

## MCP servers

- **`list tools` fails in the log.** Riverbed logs the server and the reason, then
  continues without it. Check the URL and the transport. A server that expects SSE
  will not answer a streamable request.
- **`authorization required, run "riverbed auth X"`.** An OAuth server has no stored
  token. Run `riverbed auth X` where a person can open a browser. The daemon will not
  wait for one.
- **OAuth completes but fails again after a restart.** The refreshed token could not
  be stored. Check that the database is writable by the service user.
- **The device cannot use the Riverbed MCP server.** `mcp_serve.enabled` must be
  true, the type on the device must be Streamable, and the token must equal
  `RIVERBED_MCP_TOKEN`.

## Useful checks

```sh
riverbed version
riverbed migrate -config riverbed.toml     # loads the configuration and exits
riverbed search -config riverbed.toml -limit 3
curl -s localhost:8080/healthz
```

`riverbed migrate` is the quickest way to find out whether a configuration is valid.
It reports every problem at once.
