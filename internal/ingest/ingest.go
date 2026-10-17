package ingest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mauro/boaky/internal/config"
	"github.com/mauro/boaky/internal/library"
	"github.com/mauro/boaky/internal/search"
)

// Pipeline indexes the book library into SQLite.
type Pipeline struct {
	DB  *sql.DB
	Cfg config.Config
}

// Options controls one indexing run.
type Options struct {
	Force     bool     // re-index everything, ignoring content hashes
	DryRun    bool     // report, change nothing
	Reconcile bool     // remove records for vanished files (still honors DryRun)
	Files     []string // restrict run to these rel_paths (must be inside root)
}

// FileResult describes the outcome for one discovered file.
type FileResult struct {
	RelPath  string
	BookID   string
	Status   string // indexed | skipped | error | missing | removed | pending
	Passages int
	Error    string
}

// Report summarizes an indexing run.
type Report struct {
	Scanned int
	Indexed int
	Skipped int
	Failed  int
	Removed int
	DryRun  bool
	Results []FileResult
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// genID returns "bk-" + 12 hex chars. Book IDs are random so they survive
// renames and re-indexing; rel_path keeps them tied to files.
func genID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "bk-" + hex.EncodeToString(b), nil
}

// Index runs the whole pipeline: discover, hash, skip/index, reconcile.
func (p *Pipeline) Index(ctx context.Context, opts Options) (Report, error) {
	reports := Report{DryRun: opts.DryRun}

	seen := map[string]bool{}
	files, err := library.Walk(p.Cfg.BooksDir)
	if err != nil {
		return reports, err
	}
	filter := map[string]bool{}
	for _, f := range opts.Files {
		rel := strings.TrimPrefix(filepath.ToSlash(f), "./")
		filter[rel] = true
	}
	if len(filter) > 0 {
		var sel []library.File
		for _, f := range files {
			if filter[f.RelPath] {
				sel = append(sel, f)
			}
		}
		files = sel
	}
	for _, f := range files {
		select {
		case <-ctx.Done():
			return reports, ctx.Err()
		default:
		}
		seen[f.RelPath] = true
		reports.Scanned++
		res := p.indexFile(ctx, f, opts.Force, opts.DryRun)
		if res.Error != "" {
			reports.Failed++
		} else if res.Status == "skipped" {
			reports.Skipped++
		} else {
			reports.Indexed++
		}
		reports.Results = append(reports.Results, res)
	}

	// Vanished-file reconciliation.
	reconciled, err := p.reconcileMissing(ctx, seen, opts)
	if err != nil {
		return reports, err
	}
	reports.Removed += reconciled
	return reports, nil
}

// indexFile indexes or skips one file.
func (p *Pipeline) indexFile(ctx context.Context, f library.File, force, dry bool) FileResult {
	res := FileResult{RelPath: f.RelPath, Status: "indexed"}
	if f.Size > p.Cfg.MaxFileBytes {
		res.Status = "error"
		res.Error = fmt.Sprintf("file larger than max_file_bytes (%d > %d)", f.Size, p.Cfg.MaxFileBytes)
		return res
	}
	fh, err := os.Open(f.AbsPath)
	if err != nil {
		res.Status = "error"
		res.Error = err.Error()
		return res
	}
	defer fh.Close()
	sum, err := library.HashFile(fh)
	if err != nil {
		res.Status = "error"
		res.Error = "hash: " + err.Error()
		return res
	}
	oldID, oldSum, oldStatus, err := lookupBookByPath(p.DB, f.RelPath)
	if err != nil {
		res.Status = "error"
		res.Error = err.Error()
		return res
	}
	if !force && oldID != "" && oldSum == sum && oldStatus == "indexed" {
		res.Status = "skipped"
		res.BookID = oldID
		return res
	}
	res.BookID = oldID

	raw, err := io.ReadAll(io.LimitReader(fh, p.Cfg.MaxFileBytes+1))
	if err != nil {
		res.Status = "error"
		res.Error = "read: " + err.Error()
		return res
	}
	if int64(len(raw)) > p.Cfg.MaxFileBytes {
		res.Status = "error"
		res.Error = "file larger than max_file_bytes"
		return res
	}
	if !utf8.Valid(raw) {
		res.Status = "error"
		res.Error = "invalid UTF-8 (convert explicitly; the last valid index was preserved)"
		if !dry {
			p.markError(f.RelPath, oldID, sum, f.Size, res.Error)
		}
		return res
	}
	text := Normalize(string(raw))
	meta := library.ExtractMetadata(text)
	title := meta.Title
	if title == "" {
		title = library.FallbackTitle(f.RelPath)
	}
	body := text[meta.BodyStart:meta.BodyEnd]
	offsetBase := utf8.RuneCountInString(text[:meta.BodyStart])
	passages := Split(body, SplitParams{MaxRunes: p.Cfg.PassageMaxRunes, Overlap: p.Cfg.PassageOverlapRunes})
	res.Passages = len(passages)
	if dry {
		res.Status = "pending"
		return res
	}

	if oldID == "" {
		if oldID, err = genID(); err != nil {
			res.Status = "error"
			res.Error = err.Error()
			return res
		}
	}
	if err := p.writeBook(ctx, oldID, f, title, meta.Author, sum, passages, offsetBase); err != nil {
		res.Status = "error"
		res.Error = err.Error()
		p.markError(f.RelPath, oldID, sum, f.Size, err.Error())
		return res
	}
	writeMetaJSON(p.Cfg.MetaDir, BookMeta{
		ID: oldID, RelPath: f.RelPath, Title: title, Author: meta.Author,
		Size: f.Size, SHA256: sum, Status: "indexed", Passages: len(passages),
		IndexedAt: now(),
	})
	res.BookID = oldID
	return res
}

// writeBook atomically replaces passages + FTS rows + book record.
func (p *Pipeline) writeBook(ctx context.Context, id string, f library.File, title, author, sum string, passages []Passage, offsetBase int) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM passages_fts WHERE rowid IN (SELECT pid FROM passages WHERE book_id = ?)`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM passages WHERE book_id = ?`, id); err != nil {
		return err
	}
	var first, last string
	tx.QueryRowContext(ctx, `SELECT first_indexed_at FROM books WHERE id = ?`, id).Scan(&first)
	if first == "" {
		first = now()
	}
	last = now()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO books (id, rel_path, title, author, size_bytes, sha256, status, passage_count, error, first_indexed_at, last_indexed_at)
		VALUES (?,?,?,?,?,?, 'indexed', ?, '', ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			rel_path=excluded.rel_path, title=excluded.title, author=excluded.author,
			size_bytes=excluded.size_bytes, sha256=excluded.sha256, status='indexed',
			passage_count=excluded.passage_count, error='', last_indexed_at=excluded.last_indexed_at`,
		id, f.RelPath, title, author, f.Size, sum, len(passages), first, last)
	if err != nil {
		return err
	}
	if err := insertPassages(ctx, tx, id, title, passages, offsetBase, p.Cfg.IndexBatchSize); err != nil {
		return err
	}
	return tx.Commit()
}

// insertPassages bulk-inserts passages with prepared statements inside tx.
func insertPassages(ctx context.Context, tx *sql.Tx, bookID, title string, passages []Passage, offsetBase, batch int) error {
	if len(passages) == 0 {
		return nil
	}
	if batch <= 0 {
		batch = 500
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO passages (passage_id, book_id, seq, text, heading, char_start, char_end, locator, content_hash)
		VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, ps := range passages {
		var loc string
		if ps.Heading != "" {
			loc = fmt.Sprintf("Book: %s, %s, passage %d", title, ps.Heading, i+1)
		} else {
			loc = fmt.Sprintf("characters %d–%d", ps.Start+offsetBase, ps.End+offsetBase)
		}
		pid := fmt.Sprintf("%s:%d", bookID, i+1)
		cstart, cend := ps.Start+offsetBase, ps.End+offsetBase
		chash := library.HashBytes([]byte(ps.Text))
		if _, err := stmt.ExecContext(ctx, pid, bookID, i+1, ps.Text, ps.Heading, cstart, cend, loc, chash); err != nil {
			return fmt.Errorf("passage %d: %w", i+1, err)
		}
		var pid64 int64
		if err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&pid64); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO passages_fts(rowid, body, heading, title) VALUES (?,?,?,?)`,
			pid64, ps.Text, ps.Heading, title); err != nil {
			return fmt.Errorf("fts passage %d: %w", i+1, err)
		}
		if (i+1)%batch == 0 {
			// Commit is per-book (single tx); batching here only bounds the
			// prepared-statement loop and keeps the transaction log tidy.
			continue
		}
	}
	return nil
}

// markError records an ingest failure without destroying previous passages.
func (p *Pipeline) markError(relPath, id, sum string, size int64, msg string) {
	if id == "" {
		id, _ = genID()
	}
	if _, err := p.DB.Exec(`
		INSERT INTO books (id, rel_path, title, author, size_bytes, sha256, status, passage_count, error, first_indexed_at, last_indexed_at)
		VALUES (?,?,?,?,?,?,'error',0,?,?,?)
		ON CONFLICT(id) DO UPDATE SET sha256=excluded.sha256, status='error', error=excluded.error, last_indexed_at=excluded.last_indexed_at`,
		id, relPath, library.FallbackTitle(relPath), "", size, sum, truncate(msg, 2000), now(), now()); err != nil {
		// log only; the run continues with other files
	}
}

// reconcileMissing handles files that vanished since the last run.
func (p *Pipeline) reconcileMissing(ctx context.Context, seen map[string]bool, opts Options) (int, error) {
	rows, err := p.DB.QueryContext(ctx, `SELECT id, rel_path, status FROM books`)
	if err != nil {
		return 0, err
	}
	type book struct{ id, rel, status string }
	var vanished []book
	for rows.Next() {
		var b book
		if err := rows.Scan(&b.id, &b.rel, &b.status); err != nil {
			rows.Close()
			return 0, err
		}
		if !seen[b.rel] {
			vanished = append(vanished, b)
		}
	}
	rows.Close()

	n := 0
	for _, b := range vanished {
		abs, _ := library.SafeJoin(p.Cfg.BooksDir, b.rel)
		if _, err := os.Stat(abs); err == nil {
			continue // present but not seen (e.g. --file restricted): fine
		}
		n++
		if opts.DryRun {
			continue
		}
		if opts.Reconcile {
			if _, err := p.DB.ExecContext(ctx, `DELETE FROM books WHERE id = ?`, b.id); err != nil {
				return n, err // cascades to passages and FTS rows are cleaned below
			}
			p.DB.Exec(`DELETE FROM passages_fts WHERE rowid NOT IN (SELECT pid FROM passages)`)
			os.Remove(filepath.Join(p.Cfg.MetaDir, b.id+".json"))
		} else {
			p.DB.Exec(`UPDATE books SET status='missing', last_indexed_at=? WHERE id=?`, now(), b.id)
		}
	}
	return n, nil
}

func lookupBookByPath(db *sql.DB, rel string) (id, sum, status string, err error) {
	err = db.QueryRow(`SELECT id, sha256, status FROM books WHERE rel_path = ?`, rel).Scan(&id, &sum, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", nil
	}
	return id, sum, status, err
}

// BookMeta is what gets mirrored to data/metadata/<id>.json.
type BookMeta struct {
	ID        string `json:"id"`
	RelPath   string `json:"rel_path"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Status    string `json:"status"`
	Passages  int    `json:"passages"`
	Error     string `json:"error,omitempty"`
	IndexedAt string `json:"indexed_at"`
}

func writeMetaJSON(dir string, m BookMeta) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(filepath.Join(dir, m.ID+".json"), data, 0o644)
}

// ListBooks returns all book rows ordered by title then path.
func (p *Pipeline) ListBooks(ctx context.Context) ([]BookMeta, error) {
	rows, err := p.DB.QueryContext(ctx, `
		SELECT id, rel_path, title, author, size_bytes, sha256, status, passage_count, error, first_indexed_at, last_indexed_at
		FROM books ORDER BY CASE WHEN title='' THEN 1 ELSE 0 END, title, rel_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BookMeta
	for rows.Next() {
		var m BookMeta
		var s string
		if err := rows.Scan(&m.ID, &m.RelPath, &m.Title, &m.Author, &m.Size, &m.SHA256, &m.Status, &m.Passages, &m.Error, &s, &s); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Status is the library-wide aggregate shown by `boaky status`.
type Status struct {
	Books          int
	Indexed        int
	Errored        int
	Missing        int
	Passages       int
	DBPath         string
	SchemaVersion  int
	BooksDir       string
	IndexSizeBytes int64
}

func (p *Pipeline) Status(ctx context.Context) (Status, error) {
	var st Status
	st.DBPath = p.Cfg.DBPath
	st.BooksDir = p.Cfg.BooksDir
	st.SchemaVersion = search.SchemaVersion(p.DB)
	err := p.DB.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status='indexed' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN status='missing' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(passage_count),0)
		FROM books`).Scan(&st.Books, &st.Indexed, &st.Errored, &st.Missing, &st.Passages)
	if fi, err := os.Stat(p.Cfg.DBPath); err == nil {
		st.IndexSizeBytes = fi.Size()
	}
	return st, err
}

// Inspect returns one book plus a few sample passage locators.
func (p *Pipeline) Inspect(ctx context.Context, idOrPath string) (map[string]any, error) {
	var id, rel, title, author, status, sum, errS string
	var size, count int64
	var first, last string
	q := `SELECT id, rel_path, title, author, status, sha256, error, size_bytes, passage_count, first_indexed_at, last_indexed_at FROM books WHERE id = ? OR rel_path = ?`
	err := p.DB.QueryRowContext(ctx, q, idOrPath, idOrPath).Scan(&id, &rel, &title, &author, &status, &sum, &errS, &size, &count, &first, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("book not found: %q", idOrPath)
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"id": id, "rel_path": rel, "title": title, "author": author,
		"status": status, "sha256": sum, "size_bytes": size,
		"passages": count, "first_indexed_at": first, "last_indexed_at": last,
	}
	if errS != "" {
		out["error"] = errS
	}
	rows, err := p.DB.QueryContext(ctx,
		`SELECT passage_id, seq, heading, char_start, char_end FROM passages WHERE book_id=? ORDER BY seq LIMIT 12`, id)
	if err != nil {
		return out, nil
	}
	defer rows.Close()
	var ps []map[string]any
	for rows.Next() {
		var pid, h string
		var seq, cs, ce int
		if err := rows.Scan(&pid, &seq, &h, &cs, &ce); err == nil {
			m := map[string]any{"passage_id": pid, "seq": seq, "char_start": cs, "char_end": ce}
			if h != "" {
				m["heading"] = h
			}
			ps = append(ps, m)
		}
	}
	out["first_passages"] = ps
	return out, rows.Err()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
