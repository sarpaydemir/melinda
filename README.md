# Melinda

[![ci](https://github.com/sarpaydemir/melinda/actions/workflows/ci.yml/badge.svg)](https://github.com/sarpaydemir/melinda/actions/workflows/ci.yml)

Melinda: a self-hosted AI teammate for your team chat.

Melinda is a single Go binary that answers direct messages and mentions in your team chat. It uses any
OpenAI-compatible model, keeps conversation history and long-term memory in PostgreSQL with pgvector, and offers the
model tools: web search, page fetch, image generation and Plane. Mattermost is supported today; other chat platforms
plug in through the `chat.Platform` port (`internal/port/chat`). The agent's name, persona, language and prompts are
configuration, so one binary serves any number of differently configured agents.

It is self-hosted so that your conversations, memory and knowledge live in your own database. The engine contacts
only the services you configure: the chat server, the model API, a SearXNG instance, Plane, and the pages the model
asks to read.

Status: pre-release. License: Apache-2.0.

## Features

- Conversation history per user and thread, and long-term facts about each user. Facts are extracted from the author's
  turns (every fifth turn) and recalled by embedding similarity (`internal/memory`).
- Shared knowledge learned from public conversations in batches of 25 queued turns, retrievable by the model through
  the `search_public_kb` tool and preloaded into replies (`internal/kb`).
- Tools: `search_web` (SearXNG), `visit_page`, `generate_image`, `search_public_kb`, `send_progress_update`, the
  Mattermost read tools (`search_mattermost`, `mattermost_get_teams`, `mattermost_get_channels`,
  `mattermost_get_channel_posts`) and the Plane tools (`plane_list_projects`, `plane_get_project_metadata`,
  `plane_list_issues`, `plane_get_work_item`, `plane_list_comments`, `plane_list_cycle_issues`, `plane_create_issue`,
  `plane_update_issue`, `plane_create_comment`). `enabled_tools` restricts the set.
- Automations: a regular expression on the message runs a built-in handler instead of the normal reply. The only
  handler today is `brand_analysis`.
- Attachments: images, PDFs, Office and text files go to the vision model; if that fails, the text of docx, xlsx and
  xlsm files is extracted instead.
- Prompts are a catalog of IDs with built-in defaults, overridable per deployment from a directory
  (`MELINDA_PROMPTS_DIR`) or the `prompts` settings key. `melinda prompts export <dir>` writes the effective set.
- Settings reload at runtime (`MELINDA_SETTINGS_TTL`, default 5s); a reload that fails validation keeps the last good
  settings.

## Architecture

```
chat server ──websocket──▶ gateway ──▶ dispatch ──▶ pipeline ──▶ chat server
                              ▲          │ buffer      │ gate, automations, context,
                              │          │ flusher     │ tool loop, delivery, persistence
                           recovery      │ journal     ▼
                                         ▼         model / tools / search / Plane
                                  PostgreSQL + pgvector ◀── kb-learner
```

`melinda run` starts six components under a supervisor (`internal/app`): `gateway`, `recovery`, `dispatcher`,
`flusher`, `pipeline` and `kb-learner`. `MELINDA_COMPONENTS` selects a subset; a component whose consumer is not
selected is rejected at startup. The supervisor starts them in order, stops them in reverse order, and stops the
process if one returns or panics before shutdown. Shutdown waits up to `MELINDA_SHUTDOWN_TIMEOUT` for in-flight work
before cancelling it.

**Intake.** `internal/gateway` keeps the platform's websocket session open and reconnects with backoff (1s doubling to
1m). Events go to `internal/dispatch`, which decodes them, applies the gate (the pipeline applies it again at flush), and appends accepted
posts to a per-conversation buffer in the key-value store. A buffer is flushed when its debounce window
(`debounce_seconds`, default 3) closes without new posts. The flusher takes the buffer and records it in an in-flight
journal in one transaction, then submits it to the pipeline. The journal entry is released only when the run finishes;
a run left behind by a crash is resubmitted on startup and by a periodic sweep, up to three starts per entry. A
database advisory lock keeps a second instance from flushing.

**Pipeline.** `internal/pipeline` answers one flushed conversation: runs are ordered per conversation and bounded by
`MELINDA_PIPELINE_WORKERS` and `MELINDA_PIPELINE_RUN_TIMEOUT`. For each run:

1. Gate (`internal/routing`): ignore system events and the agent's own posts; answer direct messages, channels listed
   in `name_excluded_channels`, and messages that mention the agent's username (or name it inside a thread).
2. Automations: the first matching pattern in `automations` runs its handler in place of the reply.
3. Context: the last 10 history items, the author's facts, shared knowledge, the author's profile, admin status, and
   an analysis of attachments.
4. Tool loop (`internal/llm`): the model calls tools until it answers or a budget is spent.
5. Delivery: the reply is posted in the thread (the conversation root for DMs) with files the tools uploaded. If
   posting with files fails, it is posted once more without them.
6. Persistence: the turn is appended to history, facts are extracted, and the turn is queued for knowledge learning.

**Recovery.** `internal/dispatch/recovery.go` records the last-seen marker before the first connection and at every
disconnect, and after reconnecting replays the platform's messages created since the earliest recorded point. The
dispatcher remembers the last 10,000 buffered post IDs, so a message delivered twice is buffered once. A soak test (`make test-soak`) drives websocket drops and
settings reloads and checks that no addressed message goes unanswered or answered twice.

**Knowledge learner.** The pipeline queues answered turns in the key-value store. When 25 are pending, `kb-learner`
claims them, asks the fact model for knowledge entries, and stores those that pass the checks described below.
Claimed items return to pending if the run fails; items stuck in processing for an hour are taken back.

**Ports and adapters.** The engine depends only on the interfaces in `internal/port/*`: `chat` (platform), `model`
(chat completions, embeddings, images), `tool`, and `web` (search and fetch). Implementations live in
`internal/adapters/*` and are registered by name in one table, `builtin` in `internal/app/adapters.go`. The settings
select them by name (`chat_platform`, `model_provider`, `web_search`, `web_fetch`). `internal/app/layout_test.go`
enforces the layering: ports depend only on the standard library and a few shared helpers (`internal/textfmt`,
`internal/clock`), adapters may not import stores, domain packages or components, gateway, dispatch and pipeline may
not import each other, and only `internal/store` (and the test kit) imports the database driver.

**Store.** One PostgreSQL database with the pgvector extension. Vector rows (facts and shared knowledge) live in one
embeddings table, 1536-dimension embeddings, filtered by JSON metadata. Searches are exact: the filter runs
first and the ranking is a cosine-distance sort over the filtered rows, never an approximate index scan, so a filter
cannot silently drop matches. Schema is created and verified at startup; migrations are additive.

## Design choices

**Shadow mode.** `MELINDA_DRY_RUN=true` runs the full pipeline against the real chat server but records writes instead
of performing them: chat posts and uploads, and every non-GET/HEAD request of tool sources (Plane). Records are JSON
lines in `MELINDA_DRY_RUN_LOG`, or in the log output when unset. Reads, model calls and database writes still happen,
so shadow mode uses the same database as a live agent only if you intend that. `/readyz` reports the mode.

**Leak guard.** Anything the model sends out of the engine can carry text it read elsewhere. Tools marked outbound
(`search_web`, `visit_page`, `plane_create_issue`, `plane_update_issue`, `plane_create_comment`) are refused for the
rest of a reply once a tool has read a conversation other than the current one. From then on, the reply and each
progress update are posted as fenced code blocks, so a client renders them literally and fetches no image or link.
Controlled by `restrict_outbound_after_cross_reads` (default `true`). Known limit: only reads of other conversations trip
it (`search_mattermost`, `mattermost_get_channel_posts`). Untrusted pages fetched with `visit_page`, attached files,
`search_public_kb` and preloaded shared knowledge do not, so injected instructions in them can still steer the model.

**Direct-message facts stay out of channels.** Facts are stored with scope `private` for DMs and `public` for
everything else. Outside a DM, recall filters on `scope = public`; in a DM, all of that user's facts are eligible.
Direct messages are never queued for shared knowledge.

**Code decides what enters shared knowledge.** The model only proposes entries: for a batch of queued turns it returns
JSON with `store`, a summary, memory type, confidence, source IDs and a rationale. Code decides:

- Queueing skips direct messages, private and group conversations (unless `kb_learn_from_private_channels`), error
  replies and blank text or replies.
- An entry is stored only if `store` is true, confidence is at least 0.65 (0.75 for `external_fact`), the type is one
  of the known memory types, the summary and rationale are non-empty, and every source ID is a post that was in the
  batch.
- An entry within cosine distance 0.15 of a stored one is dropped as a duplicate.

This bounds what enters the store; it does not verify that an accepted entry is true.

**Budgets, retries, fallback.** The tool loop stops at `max_tool_iterations` model turns (default 15) or
`max_tool_calls` calls (default 20), then asks the model for an answer without tools. A malformed tool call is retried
once with a corrective instruction; if it is still malformed, `tool_fallback_model` (when set) is asked for valid calls.
A failed request is retried on `tool_fallback_model`. If the model returns nothing after tools ran, up to two final
requests are made, and then the reply falls back to the last successful tool output. The model and Plane adapters
retry transient HTTP failures.

**Failures are shown.** When no useful answer can be made, the reply is the `error.generic` text followed by a code
such as `[ERR_LLM_INVOKE_FAILED_INITIAL]` or `[ERR_TOOL_BUDGET_EXHAUSTED_FINAL_EMPTY]`. Error replies are never queued
for learning or used as facts. A tool error is returned to the model as `Error: ...` text, tool panics are caught and
logged with a stack, and a reply that cannot be posted is logged rather than dropped silently.

## Adapters

Built in: Mattermost (`chat_platform: mattermost`), Plane tools, OpenAI-compatible APIs (`model_provider: openai`),
SearXNG search (`web_search: searxng`) and page fetch (`web_fetch: httpfetch`). Page fetch connects only to public
addresses, checked at dial time on every redirect; `web_fetch_allow_private` lifts that.

**A chat adapter** implements `chat.Platform`:

| Method | Contract |
|---|---|
| `Listen(ctx, connected, deliver)` | One intake session: call `connected` when events flow, pass each raw event to `deliver` in order, return the error that ended it, or nil after `ctx` is cancelled. |
| `Replay(ctx, after, deliver)` | Deliver messages created after `after` that a session may have missed, oldest first per conversation. |
| `Decode(event)` | Parse an event or a stored `Message.Payload` into a `chat.Message`; report false for non-messages; no I/O. |
| `Identity()` | The configured agent account; `ID` always set. |
| `Self(ctx)` | The account as the platform reports it, used to verify the configured ID at startup. |
| `User(ctx, id)` | A user's profile. |
| `Attachment(ctx, id)` | Attachment metadata. |
| `Download(ctx, id, max)` | Content, stopping with `chat.ErrTooLarge` past `max` bytes. |
| `Send(ctx, Outgoing)` | Post a message and return its ID. |
| `Upload(ctx, conversationID, File)` | Store a file for a later `Send` and return its ID. |

`Message.Kind` must report `Public`, `Private` or `Direct`; routing, learning and fact scoping depend on it. Return
`chat.ErrUnauthorized` for rejected credentials.

To register it, add an entry to `platforms` in `builtin` (`internal/app/adapters.go`) with:

- `open`: builds the platform from a `*config.Settings` snapshot without I/O, reading its own keys with `Decode`;
- `tools`: optional read-only tools offered only on that platform;
- `prompts`: catalog entries for those tools' descriptions;
- `secrets`: settings keys that hold secrets, each also settable as `<key>_secret: "env:NAME"` or `"file:/path"`
  (Mattermost declares `mattermost_token`);
- `restart`: keys whose startup values hold until a restart (Mattermost declares `mattermost_bot_id` and
  `mattermost_token`).

Select it with `chat_platform`. Use `internal/testkit/conformance` and the fakes in `internal/testkit` for tests.

**A tool adapter** returns `tool.Tool` values (`Name`, `Parameters` as JSON schema, `Call`). Mark tools that send
model-chosen content out of the process with `Outbound()` so the leak guard covers them. Call `c.ReadFrom(conversationID)`
for every conversation whose content a result contains. Add it as a `toolSource` in `builtin`; each tool's description
is the catalog entry `tools.desc.<name>`. **A model adapter** implements `model.Provider` (`Chat`, `Embed`, `Image`) and
**search and fetch adapters** implement `web.Searcher` and `web.Fetcher`; each is one entry in the matching table.

## Settings

A JSON document read from `MELINDA_SETTINGS_FILE` (or inline from `MELINDA_SETTINGS_JSON`). Only `model` is required
by the core; the adapters you select require their own keys. Without `embedding_model`, facts and shared knowledge are
off. Secrets are referenced, never written inline:

```json
{
  "model": "gpt-4o",
  "embedding_model": "text-embedding-3-small",
  "vision_model": "gpt-4o",
  "image_generation_model": "gpt-image-1",
  "persona": "You are Melinda, a helpful teammate.",
  "admin_ids": ["abc123"],

  "api_base": "https://api.openai.com/v1",
  "key_secret": "env:OPENAI_API_KEY",

  "mattermost_endpoint": "https://chat.example.com",
  "mattermost_bot_id": "bot-user-id",
  "mattermost_bot_username": "melinda",
  "mattermost_token_secret": "env:MATTERMOST_TOKEN",

  "plane_endpoint": "https://plane.example.com",
  "plane_workspace_slug": "acme",
  "plane_api_secret": "env:PLANE_API_KEY",

  "tool_fallback_model": "gpt-4o-mini",
  "max_tool_iterations": 15,
  "max_tool_calls": 20,
  "restrict_outbound_after_cross_reads": true
}
```

Embeddings must be 1536-dimensional. Other core keys, with defaults, are in `internal/config/settings.go`.
`mattermost_bot_id` must be the ID of the token's account; startup fails otherwise.

## Run

Docker Compose, with PostgreSQL (pgvector) and an optional SearXNG:

```bash
make image                                  # builds melinda:latest
cd deploy
cp .env.example .env                        # set POSTGRES_PASSWORD and SEARXNG_SECRET (required even without search)
$EDITOR settings.json                       # as above
docker compose up -d                        # add --profile search for SearXNG
```

Bare binary:

```bash
make build                                  # static binary at bin/melinda
MELINDA_SETTINGS_FILE=settings.json \
MELINDA_DATABASE_URL=postgres://user:pass@host:5432/melinda \
bin/melinda run
```

Required environment (`internal/config/env.go`, `internal/store/pg/pg.go`): a settings source
(`MELINDA_SETTINGS_FILE` or `MELINDA_SETTINGS_JSON`) and a database, either `MELINDA_DATABASE_URL` or
`MELINDA_DB_USER` and `MELINDA_DB_NAME` (with `MELINDA_DB_PASSWORD`, `MELINDA_DB_HOST` default `localhost`,
`MELINDA_DB_PORT` default 5432). Notable optional variables: `MELINDA_AGENT_ID` (default `melinda`; it namespaces
keys, collections and the journal), `MELINDA_TIMEZONE`, `MELINDA_LOG_LEVEL`, `MELINDA_LOG_FORMAT`,
`MELINDA_HTTP_ADDR` (`/healthz`, `/readyz`; default `:8080`), `MELINDA_SEARXNG_URL`, `MELINDA_PROMPTS_DIR`.
The database needs the `vector` extension (`deploy/postgres-init.sql`).

Commands: `melinda run`, `config check [--db]`, `prompts export <dir> [--force]`, `health`, `version`.

## Migrating an existing deployment

Melinda can take over an agent whose data already uses its PostgreSQL schema. Key and collection names are settings
(`buffer_prefix`, `last_seen_key`, `bot_identity_key`, `memory_key_prefix`, `kb_batch_key`, `facts_collection`,
`kb_collection`, `assistant_label`) and prompts come from a profile directory (`MELINDA_PROMPTS_DIR`), so the stored
rows and texts are used as they are.

1. **Verify against a copy.** Restore a copy of the database and run the read-only compat suite on it:
   `MELINDA_COMPAT_DSN=<copy> MELINDA_SETTINGS_FILE=<settings> go test ./internal/store/vector ./internal/kb -run Snapshot`.
   Every stored row must decode. With `MELINDA_COMPAT_SEARCHES` (one recorded search per line) the searches must
   return the same rows in the same order. Every session on the copy is read-only.
2. **Shadow mode.** Run Melinda on the copy with `MELINDA_DRY_RUN=true` next to the running agent, with the same chat
   token. It answers every message but records instead of posting; compare the records with the live replies.
3. **Switch.** Stop the old agent and start Melinda on the live database. Messages posted in between are replayed
   from the last-seen marker.

## Development

```bash
make check             # gofmt, vet, staticcheck, tests with -race
make test-integration  # SQL tests against a throwaway pgvector container (Docker)
make test-e2e          # the race-enabled binary against fake chat, model, search and Plane servers (Docker)
make test-soak         # sustained traffic with websocket drops and reloads: make test-soak DURATION=2h OUT=dir
make test-load         # worst-case concurrent replies under the default container memory limit
make vuln              # govulncheck
```

Golden files live in `testdata` directories; `go test <package> -update` rewrites them, so review the diff.

## License

Apache-2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
