# AGENTS.md

Instructions for coding agents working on Riverbed. For installing and configuring
Riverbed rather than changing it, use the `setup-riverbed` skill in
`.agents/skills/setup-riverbed/`.

## What this project is

Riverbed receives webhook deliveries from a Pebble Index 01 voice recorder, stores
each recording in SQLite, routes transcriptions that ask for something to an agent,
and makes every recording searchable by keyword and by meaning.

## Commands

```sh
make help       # every target, with a description
make            # vet, test, build
make check      # vet, test, and fail if anything is unformatted
```

Run `make check` before you report a change as done. Tests use `t.Context()` and
temporary directories, so they need no setup and no network, apart from
`config.TestExampleConfigIsValid`, which reads the checked-in example file.

## Constraints that are not negotiable

- **CGo stays off.** `CGO_ENABLED=0` everywhere. Every dependency must be pure Go,
  because Riverbed ships as one static binary for four platforms. Do not add a
  dependency that needs C, a shared library, or a runtime download of a binary.
- **No fallback or legacy paths.** If a configuration is wrong, fail at startup with
  a message that names the problem. Do not guess a default to keep going, and do not
  keep an old code path working beside a new one.
- **No secrets in configuration files.** Values are referenced as `${VAR}` and read
  from the environment. `config.expand` refuses to start on an unset reference.
- **Minimum Go version is 1.25**, set by `github.com/modelcontextprotocol/go-sdk`.

## Layout

| Path | Contents |
| --- | --- |
| `cmd/riverbed/` | The binary. Subcommands dispatch from `main.go`; `init.go` holds `riverbed init`. |
| `riverbed.go` | `App`, which wires every package together. |
| `config/` | Configuration loading, validation and the `init` scaffolding. |
| `store/` | SQLite. Schema in `migrations/`, embedded with `go:embed`. |
| `webhook/` | The device receiver, an `http.Handler`. |
| `embedding/` | The `Embedder` interface and its three implementations. |
| `route/` | Prefix, regular expression, semantic and classifier routing. |
| `agent/` | The `Agent` interface and the four providers, one file each. |
| `tool/` | MCP client registry and OAuth. |
| `mcpserve/` | Riverbed's own MCP server. |
| `pipeline/` | The worker that claims, routes, embeds and runs. |

## Conventions

- Comment only what is unclear or surprising. A comment that restates the code, or
  that states a general fact about Go or the toolchain, is noise: delete it. Doc
  comments on exported identifiers should add information the name does not.
- Errors are wrapped with context that names the subject: `fmt.Errorf("store: read
  token for %s: %w", server, err)`. A provider error already names its agent, so
  callers do not repeat it.
- Each package has one purpose and a package comment saying what it is. Keep
  packages small, and prefer adding a file over growing one.
- Use `log/slog` through the logger passed into a type. Do not call the package-level
  `slog` functions in library code.
- Tests state the expected behaviour in the failure message, so a failure reads as a
  sentence about the system rather than a value mismatch.

## Database changes

Add a new file under `store/migrations/`, numbered in sequence, such as
`0002_add_x.sql`. Migrations are applied in lexical order and recorded in the `meta`
table. Never edit a migration that has been released, because existing databases
have already applied it.

A migration must end with a statement rather than a comment. The runner strips
trailing comments for this reason, but do not rely on it.

## Adding an agent provider

Add one file to `agent/`, implement `Agent`, and register the kind in `agent.New`
and in `config.Validate`. The provider owns its own tool-use loop: call
`invoke(ctx, req, name, args)` for each tool call, append the result, and stop at
`opts.maxTurns()`. Return a tool failure to the model as text so it can recover, and
do not end the turn on one. Test it against `httptest`, because every provider
supports a base URL override.

## Pitfalls found the hard way

- `sqlitex.ExecuteScript` wraps statements in a transaction, so `PRAGMA synchronous`
  cannot run inside it. Connection pragmas run one at a time in `PrepareConn`.
- `vector.Register` is per connection, so it runs in `PrepareConn` for every pooled
  connection, not once at open.
- FTS5 is compiled into `modernc.org/sqlite`, so hybrid search needs no build tag
  and no extra dependency.
- Keyword and vector rankings are combined with reciprocal rank fusion. Do not add
  `bm25` scores to vector distances; the two use unrelated scales.
- Text reaching FTS5 goes through `store.ftsQuery`, because spoken punctuation and
  words such as `OR` are FTS5 syntax and would otherwise be a query error.
- Routing tries exact matchers before semantic ones, rather than walking rules in
  configuration order throughout. An exact wake word has to beat a fuzzy match.
- `route.New` embeds every semantic rule's utterances, so it takes a context and
  does I/O. Routing a recording then embeds only the transcription.
- Semantic thresholds are model dependent. Do not hard-code one in a test fixture
  and assume it transfers; `route` tests use a deterministic fake embedder.

## Git

- Work on a branch named `feature/`, `bug/` or `doc/`. Rebase rather than merge.
- Commit messages say why the change was made. Describe the behaviour that changes,
  not the files touched.
- Do not commit `riverbed.toml`, `riverbed.env`, `*.db` or `dist/`. They are ignored
  already.

## Writing prose

This applies to the README, the example configuration, comments and commit messages.
Write plain declarative sentences. Do not use em dashes, sentence fragments stacked
for rhythm, "no X, no Y, just Z" constructions, "it is not X, it is Y" contrasts, or
bold-term-and-explanation bullet lists. State what the software does.
