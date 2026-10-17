# Boaky — Local-First Book Assistant

Boaky is a lightweight, local-first book research engine. It indexes plain-text
(`.txt`) books into SQLite **FTS5** and answers questions **only** from evidence
found in your own collection. If the collection can't support an answer, Boaky
abstains instead of letting the LLM guess.

The guiding principle: **Boaky owns the books and the evidence. Hermes owns the
conversation. DeepSeek generates answers only from retrieved local passages.**

## Architecture (short version)

```
User → Hermes Agent ── (OpenCode Go / DeepSeek for generation)
              │
              └─ MCP (stdio or streamable HTTP) ──→ Boaky (Go)
                                                     ├─ catalog + ingestion
                                                     ├─ incremental indexing
                                                     ├─ SQLite FTS5 search
                                                     ├─ evidence sufficiency gate
                                                     └─ citation validation
                                                        ↓
                                          ./data/index/boaky.db (persistent)
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for details.

## Prerequisites

- Go 1.26+ (only for building; the binary is self-contained, pure-Go SQLite with FTS5, no CGO)
- Docker + Docker Compose (for the containerized setup)
- A folder of `.txt` books (UTF-8; Project Gutenberg files are well supported)

## Install / build

```bash
make build            # → bin/boaky
./bin/boaky doctor    # verifies SQLite, FTS5, paths
```

## Adding books and indexing

Drop UTF-8 `.txt` files into `./books/` (nested subdirectories are supported;
hidden files ignored). Boaky never modifies, renames, or deletes source files.

```bash
./bin/boaky index                 # incremental: new/changed files only
./bin/boaky index --dry-run       # show what would happen
./bin/boaky index --force         # rebuild records deterministically
./bin/boaky index --file books/pg345.txt   # one file only
./bin/boaky status                # counts, sizes, errors
./bin/boaky list                  # catalog table
./bin/boaky inspect bk-ab12…      # one book's metadata + passage map
./bin/boaky doctor                # health + FTS consistency checks
```

Indexing skips files whose SHA-256 is unchanged, so re-runs are near-free.
Interrupted runs leave no partial records (per-book transactions).

## Searching

```bash
./bin/boaky search "memory and identity"            # natural language, no FTS syntax needed
./bin/boaky search '"exact phrase"' --title Dracula
./bin/boaky search "vampire" --author "Stoker" --limit 5
./bin/boaky answer "Does the narrator forget his name?"   # gated Q&A (see below)
```

Results include: book title/author, relative path, passage ID, section heading,
character offsets, exact text, and a BM25 score (relevance only — never proof).

## Citation & abstention behavior

- Citations resolve to **real indexed passages**: `bk-<id>:<seq>` with character
  offsets into the normalized text. No page numbers are ever fabricated; plain
  text has no reliable pagination.
- Quoted text is verified against the cited passage (whitespace-normalized
  exact match). Invented quotes/citations fail validation and the answer is
  rejected after one bounded repair attempt.
- `boaky answer` runs the **evidence gate first**: if the index can't support
  the question (relevance + token coverage thresholds), it abstains **before**
  any LLM call. Partial coverage → partial answer with gaps declared.
- `BOAKY_EVIDENCE_*` env knobs tune the thresholds (see `.env.example`). These
  are lexical signals, not semantic proof — documented in ARCHITECTURE.md.

## MCP integration with Hermes

Boaky exposes five read-only MCP tools: `list_books`, `search_books`,
`get_passages`, `get_book_metadata`, `check_evidence`. No write, filesystem,
shell, or SQL access.

**Stdio** (single machine, Hermes runs locally):

```yaml
# ~/.hermes/config.yaml
mcp_servers:
  boaky:
    command: "/path/to/boaky/bin/boaky"
    args: ["serve"]
    env:
      BOAKY_BOOKS_DIR: "/path/to/boaky/books"
      BOAKY_DB_PATH: "/path/to/boaky/data/index/boaky.db"
```

**Streamable HTTP** (containerized — what `docker compose up` uses):

```yaml
# .hermes/config.yaml
mcp_servers:
  boaky:
    url: "http://boaky:8000/mcp"   # internal Docker network
    timeout: 30
```

Then verify from inside Hermes: ask it to `list_books`. Registered tools appear
as `mcp_boaky_search_books` etc.

## Hermes + OpenCode Go (DeepSeek) setup

The `.env` file under `hermes/` holds the OpenCode Go key
(`OPENCODE_GO_API_KEY`). `hermes/config.yaml` (copied into the container's
`HERMES_HOME=/opt/data` at boot) points Hermes at Boaky and at the DeepSeek
model through the OpenAI-compatible OpenCode Go endpoint:

```yaml
# hermes/config.yaml
model:
  provider: custom:opencode-go
  default: deepseek-v4.1-flash     # must be an exact OpenCode Go model id
providers:
  opencode-go:
    base_url: "https://opencode.ai/zen/go/v1"
    api_key: ${OPENCODE_GO_API_KEY}
mcp_servers:
  boaky:
    url: http://boaky:8000/mcp
```

`default:` takes a **model id**, not a display name. Get the authoritative list
from the endpoint itself — ids such as `deepseek-v4.1-flash` and
`deepseek-v4-pro` are valid, while `DeepSeek-Flash-V4.1` or any `*-free` variant
is rejected with `Model is unavailable`:

```bash
curl -s https://opencode.ai/zen/go/v1/models \
  -H "Authorization: Bearer $OPENCODE_GO_API_KEY" | jq -r '.data[].id'
```

Also make sure `OPENCODE_GO_API_KEY` is a **Go** key (it authenticates against
`/zen/go/v1`). A Zen or other key fails with `HTTP 401: Upstream request failed:
Invalid credential`.

Config and secrets are staged through a directory mount
(`./hermes:/opt/hermes-config:ro`) and copied into the container by
`hermes/entrypoint.sh` on every boot. Because Hermes loads them at startup, edits
to `hermes/config.yaml` or `hermes/.env` require a restart to take effect:

```bash
docker compose up -d --force-recreate hermes
```

`hermes/config.yaml` in the repo is the source of truth: a `hermes config set`
run inside the container writes to the container's copy under `/opt/data` and is
overwritten on the next boot, so edit the repo file and recreate instead.

Keep the book-research profile limited to Boaky tools (no web search when
answering book questions).

> **Privacy note:** when Hermes calls DeepSeek through OpenCode Go, only the
> retrieved *passages* (bounded by `BOAKY_MAX_CONTEXT_CHARS`) and the question
> are sent to the provider — never whole books. Boaky itself makes zero network
> calls for indexing or search.

Verify the active model from inside a Hermes session (e.g. ask
`what model are you?`, or check the Hermes dashboard/session banner).

## Docker

```bash
docker compose build                    # multi-stage, distroless, non-root
docker compose up -d                    # boaky + hermes
docker compose run --rm boaky index     # (re)index your books
docker compose logs -f boaky
```

- `./books` is mounted read-only; `./data/index` and `./data/metadata` persist
  across `up --force-recreate`, rebuilds, and image updates.
- `docker compose down` is safe. **Never `docker compose down -v`** — there are
  no named volumes to remove anyway; all data lives in bind mounts.
- No published ports are exposed to the host; Hermes reaches Boaky on an
  internal network and OpenCode Go over the normal bridge network.
- Chat with Hermes interactively, or ask one-shot questions:

  ```bash
  docker compose exec -it hermes hermes chat
  docker compose run --rm hermes hermes -z "What does Dracula say about memory?"
  ```

- **Hermes MCP verification is independent of the LLM**: tools/list and tool
  calls over the internal network work with zero provider credentials (the
  evidence gate runs before any model call). If OpenCode Go returns
  `Upstream request failed: Invalid credential`, that's a provider-account
  issue, not a Boaky problem.

## Measured performance

2026-10-16, Apple Silicon + OrbStack, corpus = 45 Project Gutenberg TXTs
(19 MB, 6,872 passages, 58 MB DB). See [ARCHITECTURE.md](ARCHITECTURE.md#8-measured-performance)
and `scripts/benchmark.sh` for how each number was taken.

| Measurement | Result |
| --- | --- |
| Full cold index (45 books) | 1.4 s, peak RSS 42 MB |
| Incremental pass (unchanged) | 0.03 s, 45 skipped / 0 written |
| Search latency (20 queries, CLI end-to-end) | median 10 ms · p95 100 ms · max 180 ms |
| Idle serving container | 16.4 MiB RSS |

## Backup & restore

`./data/index/boaky.db` is the whole index (WAL mode). Backup:

```bash
# quiesced / after checkpoint (safe at any time, but do this when idle):
docker compose run --rm boaky sh -c '/usr/local/bin/boaky doctor >/dev/null 2>&1 || true'  # optional
cp -a data/index/boaky.db data/index/boaky.db-wal backup/ 2>/dev/null || cp -a data/index/boaky.db backup/
```

For a crash-safe snapshot without stopping the service, use SQLite Online
Backup (`sqlite3 boaky.db ".backup backup/boaky.db"`) or checkpoint first:

```bash
sqlite3 data/index/boaky.db "PRAGMA wal_checkpoint(TRUNCATE);"
cp data/index/boaky.db backup/boaky-$(date +%F).db
```

Restore: stop containers → copy the `.db` (and any `-wal`/`-shm`) back into
`data/index/` → `docker compose up -d`. Then run `boaky status` to verify.
Restoring from backup does not require reindexing; books themselves are your
source of truth and should be backed up separately.

## Example questions

Works: “What does Dracula say about memory?” · “List passages mentioning fog
in *A Thin Ghost*.” · “Which books discuss identity and forgetting?”

Abstains (correctly): “Who won the 1974 World Cup?” · “What does the book say
about quantum computing?” (not in collection) · questions whose premise the
texts contradict.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `index` shows `error` on a file | Usually invalid UTF-8 or a >64 MB file. Fix/convert the source copy; the previous good index is preserved. |
| No results for an obvious phrase | Re-run `boaky index`. Check `boaky status` for `error`/`missing` rows. |
| `boaky doctor` flags orphan FTS rows / index drift | `boaky index --force`. |
| Hermes can't connect to `http://boaky:8000/mcp` | Ensure both containers share the `boaky-internal` network (`docker compose up -d`) and Boaky is healthy (`docker compose ps`). |
| Everything slow / wrong paths | All settings come from `BOAKY_*` env vars; defaults resolve relative to the current directory. Set `BOAKY_BOOKS_DIR`/`BOAKY_DB_PATH` explicitly. |

## Tests

```bash
make check    # gofmt + vet + tests + race
```
