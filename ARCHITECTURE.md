# Boaky — Architecture

Boaky is a local-first book evidence engine. It indexes plain-text books
into SQLite FTS5 and answers questions **only** from retrieved local
evidence — when the corpus can't support an answer, it says so instead of
inventing one. The intended deployment pairs Boaky (books & evidence) with
Hermes (conversation), connected over MCP.

## 1. Responsibility split

| | Boaky | Hermes |
|---|---|---|
| Owns | `books/` (read-only), SQLite index, evidence | conversation, memory, generation policy |
| LLM calls | none in the recommended setup (`boaky answer` is an optional standalone path) | yes — OpenCode Go / DeepSeek |
| Network | MCP only, never egress | provider egress |
| Trust boundary | Boaky is the *evidence authority*: nothing is asserted that Boaky hasn't grounded | Hermes may paraphrase, must cite what Boaky returns |

The invariant that ties them together: **the evidence gate runs before any
LLM call.** Insufficient evidence → abstention, zero tokens sent.

```
question ──▶ Hermes ──MCP──▶ search_books / check_evidence
                              │
                    FTS5 (BM25) ──▶ passages + locators
                              │
                 gate: sufficient / partial / insufficient
                              │
          sufficient ◀────────┴─────────▶ insufficient → abstain (no LLM call)
              │
   generate from numbered passages → validate citations → answer with quotes
```

## 2. Components

```
cmd/boaky              CLI + servers (index/status/list/inspect/search/
                       answer/doctor/health/serve/serve-http)
internal/config        defaults ← .env ← BOAKY_* env (precedence order)
internal/database     open/WAL/migrations (embedded SQL, applied in-order)
internal/ingest        scan, hash-compare, passage splitting, atomic writes
internal/search        FTS5 query building, BM25 ranking, snippets
internal/evidence      the gate: coverage + score thresholds
internal/citations     passage-ID validation, verbatim quote checking
internal/generate      OpenAI-compatible client (standalone `answer` only)
internal/answer        gate → generate → validate → repair → abstain
internal/mcp           five read-only tools, stdio + streamable-HTTP
internal/logging       slog, no secrets in any log line
```

## 3. Data model

All timestamps RFC3339 UTC; all SQL parameterized. Migration
`0001_init.sql` (embedded in the binary via `go:embed`):

- **books** — stable `id` (survives re-indexing), `rel_path` (unique),
  extracted `title`/`author`, `sha256` of raw bytes, `status`
  (`indexed` | `error` | `missing`), passage count, timestamps.
- **passages** — `passage_id = "<book-id>:<seq>"` (unique, stable), rune
  offsets `char_start/char_end` into normalized text, `heading` (last
  recognized section heading), `locator` (human-stable source locator),
  `content_hash` per passage.
- **passages_fts** — FTS5 virtual table, rowids mirrored to
  `passages.pid` in Go (delete-then-insert inside each book's transaction).
  Weighted columns: `body` 1.0, `heading` 2.0, `title` 3.0
  (`-bm25(passages_fts, 1.0, 2.0, 3.0)` so title/heading matches rank first).
  Default tokenizer (unicode61, no stemming).

Sidecar JSON metadata lives in `data/metadata/` — human-editable, never
required for search.

## 4. Indexing lifecycle

`boaky index` (and the same path behind MCP/serve):

1. Walk the books root; skip non-`.txt` and files above
   `BOAKY_MAX_FILE_BYTES` (default 50 MiB).
2. Hash each file (sha256 of raw bytes). Same hash + `indexed` status →
   skip entirely — this is what makes re-runs idempotent and cheap
   (measured: 45/45 skipped in 0.03 s on an unchanged library).
3. Split into passages (~1,100 runes, 200-rune overlap) respecting
   paragraph boundaries; record heading transitions for locators.
4. Per-book **single transaction**: delete old passages + FTS rows, insert
   new ones. A crash mid-run leaves the DB consistent for every *other*
   book; there is no partially-updated book.
5. `--reconcile` additionally marks vanished files `missing` (excluded from
   search) and deletes them from the index. Without the flag, vanished
   files are left untouched so a flaky mount can't silently destroy the
   index. `--dry-run` works for either mode.

Boaky never writes to `books/`; the container mounts it read-only.

## 5. Search and the evidence gate

**Query building** (internal/search/query.go): every user term is quoted
and FTS-escaped, multi-word phrases become exact phrases, single terms get
prefix expansion, all combined with **AND**. `MATCH` injection is
impossible by construction. Hits are filtered to `status='indexed'`
books, capped per book (`max_hits_per_book=3`), limited to
`search_limit=10`, with `snippet()` fragments.

**The gate** (internal/evidence) classifies a question as:

- `insufficient` — below `min_results` hits, or coverage of question
  terms < 0.30, or (when the corpus has ≥ 50 passages) below BM25 score
  floor 2.5. **Answering is refused; abstention required.**
- `partial` — coverage between 0.30 and 0.60: answer allowed only with an
  explicit "the index only covers X" caveat.
- `sufficient` — coverage ≥ 0.60 and score above the floor.

The corpus-size condition exists because BM25's IDF component is
meaningless on tiny corpora: with < 50 passages (fresh test libraries,
single-book setups) the score floor is skipped and coverage decides. This
avoids both "everything passes" and "everything fails" on small libraries.

**Citation validation** (internal/citations, used by `boaky answer` and
enforced on every evidence-bearing claim): quoted text must appear
verbatim in the cited passage and the passage ID must exist. A model that
hallucinates a quote or a location fails validation → one repair attempt
with the original evidence re-passed → otherwise abstain.

## 6. MCP surface

Five read-only tools: `search_books`, `list_books`, `get_passages`,
`get_book_metadata`, `check_evidence`. Two transports:

- `boaky serve` — stdio, for embedding as a child process anywhere.
- `boaky serve-http` — streamable HTTP at `POST /mcp`, the compose path.

No write operations exist, so a compromised conversation layer cannot
corrupt the index. Boaky itself never initiates network connections.

## 7. Deployment

```
host: ~/.../boaky/books ──ro──▶ ┌────────────── boaky (distroless) ─┐
data/index, data/metadata ◀──bind──│  read_only, cap_drop ALL,        │
                                  │  no-new-privileges, no secrets    │
                                  └────────────┬───────────────────────┘
                                boaky-internal (internal network)
                                  ┌────────────┴───────────────────────┐
                                  │ hermes (config + .env staged from   │
                                  │ ./hermes/, state in hermes-home vol) │
                                  └────────────┬───────────────────────┘
                                        outbound (bridge) ──▶ OpenCode Go
```

- `boaky-internal` is `internal: true` — boaky is unreachable from the
  host or LAN; only Hermes on the same network can reach port 8000.
- Persistence is via bind mounts under `./data/`; recreate with
  `docker compose up -d --force-recreate`, never `down -v` (that deletes
  the named volumes... which we don't even use for data — bind mounts
  survive any compose operation short of deleting them yourself).
- SQLite opens in WAL, so the served reader never blocks a re-index and
  an interrupted write self-heals on next open. Verified: container
  delete + recreate retains 45 books / 6,872 passages.

## 8. Measured performance

2026-10-16, macOS (Apple Silicon) + OrbStack, corpus = 45 Project Gutenberg
TXT files (19 MB, 6,872 passages, 58 MB DB):

| Measurement | Result |
|---|---|
| Full index (cold DB, 45 books) | 1.40 s wall, peak RSS 42 MB |
| Incremental pass (unchanged library) | 0.03 s — 45 skipped, 0 written |
| Search latency (20 real queries, CLI end-to-end incl. process start) | median 10 ms, p95 100 ms, max 180 ms |
| Idle serving container (memory) | 16.4 MiB resident |
| DB size | 58 MB for 19 MB of text (FTS index ≈ 3× raw; WAL-bound) |

All numbers measured, not projected. The benchmark script
(`scripts/benchmark.sh`) reproduces the index/search numbers.

## 9. Security properties

- **No secrets in code or logs.** Generation keys are read from env only
  in the standalone `answer` path; the server never logs or echoes them.
  All loggers use slog with fixed fields, never free-form error strings
  from env.
- Books mounted read-only; container filesystem read-only; all Linux
  capabilities dropped; no-new-privileges.
- No host port published; internal network only.
- Every SQL statement is parameterized; the only string SQL surface is
  the pre-escaped FTS MATCH, which cannot execute arbitrary syntax.

## 10. Limitations and trade-offs

- **Lexical, not semantic.** No embeddings by design: results are exact
  and explainable, but synonyms that don't share a word form miss
  (`drowsy` ≠ `drowsi`). Prefix expansion and heading weights mitigate;
  coverage-based gating makes "we missed it" visible instead of silent.
- **AND semantics** on free-text questions is deliberately strict — long
  questions can over-constrain. `check_evidence`'s coverage metric
  compensates by measuring term coverage explicitly.
- **Plain text only.** No OCR — scanned books must be OCR'd elsewhere
  before landing in `books/`.
- **Locators are best-effort**, stable for unchanged files but not
  reflow-proof against mid-file edits; only `passage_id` + char offsets
  are machine-authoritative.
- **Single-node.** No replication; SQLite is a good fit for ≤ hundreds of
  thousands of passages and falls outside the design for much more.
- **FTS5 default tokenizer** (unicode61, no stemming) — plurals and
  conjugations only match via prefix expansion.
