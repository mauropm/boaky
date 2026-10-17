-- Boaky initial schema.
-- All timestamps are RFC3339 UTC strings. All SQL is parameterized in Go.

CREATE TABLE books (
    id              TEXT PRIMARY KEY,                 -- stable book ID (survives re-indexing)
    rel_path        TEXT NOT NULL UNIQUE,             -- path relative to the books root, '/' separators
    title           TEXT NOT NULL DEFAULT '',         -- extracted title ('' if unknown)
    author          TEXT NOT NULL DEFAULT '',         -- extracted author ('' if unknown)
    size_bytes      INTEGER NOT NULL,
    sha256          TEXT NOT NULL,                    -- sha256 of raw file bytes
    status          TEXT NOT NULL DEFAULT 'indexed',  -- indexed | error | missing
    passage_count   INTEGER NOT NULL DEFAULT 0,
    error           TEXT NOT NULL DEFAULT '',
    first_indexed_at TEXT NOT NULL,
    last_indexed_at  TEXT NOT NULL
);

CREATE TABLE passages (
    pid           INTEGER PRIMARY KEY AUTOINCREMENT,  -- internal rowid, mirrored by FTS
    passage_id    TEXT NOT NULL UNIQUE,               -- stable ID "<book-id>:<seq>"
    book_id       TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    text          TEXT NOT NULL,
    heading       TEXT NOT NULL DEFAULT '',           -- last recognized section heading, if any
    char_start    INTEGER NOT NULL,                   -- rune offsets into normalized text
    char_end      INTEGER NOT NULL,
    locator       TEXT NOT NULL,                      -- human-stable source locator
    content_hash  TEXT NOT NULL,                      -- sha256 of passage text
    UNIQUE (book_id, seq)
);

CREATE INDEX idx_passages_book ON passages (book_id);

-- FTS5 index; rowids mirror passages.pid. Columns:
--   body    (weight 1.0) passage text
--   heading (weight 2.0) section heading at passage start
--   title   (weight 3.0) book title
CREATE VIRTUAL TABLE passages_fts USING fts5(
    body,
    heading,
    title
);
