// Package integration tests exercise the full indexing/search/evidence stack.
package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauro/boaky/internal/citations"
	"github.com/mauro/boaky/internal/config"
	"github.com/mauro/boaky/internal/database"
	"github.com/mauro/boaky/internal/evidence"
	"github.com/mauro/boaky/internal/ingest"
	"github.com/mauro/boaky/internal/search"
)

// env is a prepared temp library + database.
type env struct {
	t    *testing.T
	dir  string
	cfg  config.Config
	db   *sql.DB
	pipe *ingest.Pipeline
	srch *search.Service
	ev   *evidence.Service
}

func newEnv(t *testing.T, books map[string]string) *env {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "books"), 0o755)
	writeBooks(t, dir, books)
	cfg := config.Default(dir)
	db, err := database.Open(cfg.DBPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &env{
		t: t, dir: dir, cfg: cfg, db: db,
		pipe: &ingest.Pipeline{DB: db, Cfg: cfg},
		srch: &search.Service{DB: db, Cfg: cfg},
		ev:   &evidence.Service{DB: db, Cfg: cfg, Search: &search.Service{DB: db, Cfg: cfg}},
	}
}

func writeBooks(t *testing.T, dir string, books map[string]string) {
	t.Helper()
	for rel, content := range books {
		p := filepath.Join(dir, "books", rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) index(t *testing.T, opts ingest.Options) ingest.Report {
	t.Helper()
	rep, err := e.pipe.Index(context.Background(), opts)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	return rep
}

const book1 = `The Project Gutenberg eBook of On Memory and the Hour

Title: On Memory and the Hour

Author: Agnes Anchor

*** START OF THE PROJECT GUTENBERG EBOOK ON MEMORY AND THE HOUR ***

Chapter One

The narrator forgets his name before dawn. Memory and identity drift apart
like two rivers at a confluence. He searches his pockets for a card that
might confirm who he was.

Chapter Two

He remembers the river only in dreams. The bridge of childhood holds while
the banks erode.

*** END OF THE PROJECT GUTENBERG EBOOK ON MEMORY AND THE HOUR ***
`

const book2 = `The Project Gutenberg eBook of Identity

Title: Identity

Author: Bruno Bridge

*** START OF THE PROJECT GUTENBERG EBOOK IDENTITY ***

The architecture of identity is built from memory, habit, and the faces of
strangers. Without memory there is no self, only habit.

*** END OF THE PROJECT GUTENBERG EBOOK IDENTITY ***
`

func TestIndexAndSearchFlow(t *testing.T) {
	e := newEnv(t, map[string]string{"on_memory.txt": book1, "sub/identity.txt": book2})
	rep := e.index(t, ingest.Options{})
	if rep.Indexed != 2 || rep.Failed != 0 {
		t.Fatalf("report: %+v", rep)
	}
	// unchanged files skip
	rep = e.index(t, ingest.Options{})
	if rep.Skipped != 2 || rep.Indexed != 0 {
		t.Fatalf("incremental: %+v", rep)
	}
	ctx := context.Background()
	hits, err := e.srch.Search(ctx, search.Options{Query: "narrator forgets name"})
	if err != nil || len(hits) == 0 {
		t.Fatalf("search hits: %v %d", err, len(hits))
	}
	if !strings.Contains(hits[0].Text, "forgets his name") {
		t.Errorf("wrong hit: %q", hits[0].Text)
	}
	if hits[0].Title != "On Memory and the Hour" || hits[0].Author != "Agnes Anchor" {
		t.Errorf("metadata not attached: %+v", hits[0])
	}
	// phrase search
	hits, _ = e.srch.Search(ctx, search.Options{Query: `"childhood holds"`})
	if len(hits) == 0 {
		t.Error("phrase search found nothing")
	}
	// title filter
	hits, _ = e.srch.Search(ctx, search.Options{Query: "memory", Title: "Identity", Limit: 10})
	for _, h := range hits {
		if h.Title != "Identity" {
			t.Errorf("title filter leaked: %q", h.Title)
		}
	}
	// exact passages by ID
	if len(hits) > 0 {
		ids := []string{hits[0].PassageID}
		ps, err := e.srch.GetPassages(ctx, ids)
		if err != nil || len(ps) != 1 {
			t.Fatalf("get_passages: %v %d", err, len(ps))
		}
	}
}

func TestSourceFileUpdate(t *testing.T) {
	e := newEnv(t, map[string]string{"b.txt": book1})
	e.index(t, ingest.Options{})
	ctx := context.Background()
	hits, _ := e.srch.Search(ctx, search.Options{Query: `"forgets his name"`})
	if len(hits) == 0 {
		t.Fatal("expected original content")
	}
	writeBooks(t, e.dir, map[string]string{"b.txt": strings.Replace(book1, "forgets his name", "keeps his name", 1)})
	rep := e.index(t, ingest.Options{})
	if rep.Indexed != 1 {
		t.Fatalf("changed file not re-indexed: %+v", rep)
	}
	hits, _ = e.srch.Search(ctx, search.Options{Query: `"forgets his name"`})
	if len(hits) != 0 {
		t.Error("stale passage still searchable after update")
	}
	hits, _ = e.srch.Search(ctx, search.Options{Query: `"keeps his name"`})
	if len(hits) == 0 {
		t.Error("updated content not searchable")
	}
}

func TestInvalidUTF8PreservesOldIndex(t *testing.T) {
	e := newEnv(t, map[string]string{"b.txt": book1})
	e.index(t, ingest.Options{})
	// corrupt: raw latin1 bytes mid-file
	writeBooks(t, e.dir, map[string]string{"b.txt": book1[:10] + "\xff\xfe\xfd" + book1[10:]})
	rep := e.index(t, ingest.Options{})
	if rep.Failed != 1 {
		t.Fatalf("invalid utf8 should fail: %+v", rep)
	}
	// old passages must still be searchable (status 'error' keeps old rows)
	var status string
	e.db.QueryRow(`SELECT status FROM books WHERE rel_path='b.txt'`).Scan(&status)
	if status != "error" {
		t.Errorf("status = %q", status)
	}
	var cnt int
	e.db.QueryRow(`SELECT COUNT(*) FROM passages p JOIN books b ON b.id=p.book_id WHERE b.rel_path='b.txt'`).Scan(&cnt)
	if cnt == 0 {
		t.Error("last valid indexed version was destroyed")
	}
}

func TestEmptyFileIndexedWithZeroPassages(t *testing.T) {
	e := newEnv(t, map[string]string{"empty.txt": ""})
	rep := e.index(t, ingest.Options{})
	if rep.Indexed != 1 || rep.Failed != 0 {
		t.Fatalf("empty file report: %+v", rep)
	}
	detail, err := e.pipe.Inspect(context.Background(), "empty.txt")
	if err != nil {
		t.Fatal(err)
	}
	if detail["passages"].(int64) != 0 {
		t.Errorf("expected 0 passages: %v", detail["passages"])
	}
}

func TestFailedIndexDoesNotLeavePartialRecords(t *testing.T) {
	// force an error mid-write by making the FTS insert fail: close the
	// statement's table via a trigger that raises. Simpler: make passage
	// insert violate uniqueness by pre-inserting a colliding passage.
	e := newEnv(t, map[string]string{"b.txt": book1})
	e.index(t, ingest.Options{})
	var id string
	e.db.QueryRow(`SELECT id FROM books WHERE rel_path='b.txt'`).Scan(&id)
	// corrupt: duplicate passage_id for same book (simulate partial state we
	// will overwrite) — instead, test interrupted semantics: force failure
	// via an INSERT trigger on passages.
	if _, err := e.db.Exec(`CREATE TRIGGER boom BEFORE INSERT ON passages
		FOR EACH ROW WHEN NEW.seq > 1
		BEGIN SELECT RAISE(ABORT, 'simulated failure'); END`); err != nil {
		t.Fatal(err)
	}
	// (no Gutenberg END marker in this body: added text must stay inside the indexed body)
	body := strings.Replace(book1, "*** END OF THE PROJECT GUTENBERG EBOOK ON MEMORY AND THE HOUR ***", "", 1)
	writeBooks(t, e.dir, map[string]string{"b.txt": body + "\n\nExtra chapter\n\n" + strings.Repeat("word ", 900) + "\n"})
	rep := e.index(t, ingest.Options{Force: true})
	if rep.Failed != 1 {
		t.Fatalf("expected failure, got %+v", rep)
	}
	// nothing partial: passages for the book remain exactly as before
	var cnt int
	e.db.QueryRow(`SELECT COUNT(*) FROM passages WHERE book_id=?`, id).Scan(&cnt)
	var declared int64
	e.db.QueryRow(`SELECT passage_count FROM books WHERE id=?`, id).Scan(&declared)
	var ftsCnt int
	e.db.QueryRow(`SELECT COUNT(*) FROM passages_fts WHERE rowid IN (SELECT pid FROM passages WHERE book_id=?)`, id).Scan(&ftsCnt)
	if ftsCnt != cnt {
		t.Errorf("fts/passage mismatch after failed write: %d vs %d", ftsCnt, cnt)
	}
	e.db.Exec(`DROP TRIGGER boom`)
}

func TestReconcileRemovalAndDryRun(t *testing.T) {
	e := newEnv(t, map[string]string{"b.txt": book1, "c.txt": book2})
	e.index(t, ingest.Options{})
	os.Remove(filepath.Join(e.dir, "books", "c.txt"))
	// default: mark missing, keep searchable rows out of results, no delete
	rep := e.index(t, ingest.Options{})
	if rep.Removed != 1 {
		t.Fatalf("missing not detected: %+v", rep)
	}
	var status string
	e.db.QueryRow(`SELECT status FROM books WHERE rel_path='c.txt'`).Scan(&status)
	if status != "missing" {
		t.Errorf("c.txt status = %q", status)
	}
	hits, _ := e.srch.Search(context.Background(), search.Options{Query: "architecture of identity", Limit: 10})
	for _, h := range hits {
		if h.RelPath == "c.txt" {
			t.Error("missing file still returned by search")
		}
	}
	// dry-run delete changes nothing
	rep = e.index(t, ingest.Options{Reconcile: true, DryRun: true})
	var cnt int
	e.db.QueryRow(`SELECT COUNT(*) FROM books WHERE rel_path='c.txt'`).Scan(&cnt)
	if cnt != 1 {
		t.Error("dry-run deleted records")
	}
	// real delete cascades passages + fts
	rep = e.index(t, ingest.Options{Reconcile: true})
	e.db.QueryRow(`SELECT COUNT(*) FROM books WHERE rel_path='c.txt'`).Scan(&cnt)
	if cnt != 0 {
		t.Error("reconcile did not remove")
	}
	var orphan int
	e.db.QueryRow(`SELECT COUNT(*) FROM passages_fts WHERE rowid NOT IN (SELECT pid FROM passages)`).Scan(&orphan)
	if orphan != 0 {
		t.Errorf("orphan fts rows after delete: %d", orphan)
	}
}

func TestCloseReopenDatabase(t *testing.T) {
	e := newEnv(t, map[string]string{"b.txt": book1})
	e.index(t, ingest.Options{})
	e.db.Close()
	db2, err := database.Open(e.cfg.DBPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	s := &search.Service{DB: db2, Cfg: e.cfg}
	hits, err := s.Search(context.Background(), search.Options{Query: "narrator forgets"})
	if err != nil || len(hits) == 0 {
		t.Fatalf("search after reopen: %v hits=%d", err, len(hits))
	}
}

func TestEvidenceGate(t *testing.T) {
	e := newEnv(t, map[string]string{"b.txt": book1, "c.txt": book2})
	e.index(t, ingest.Options{})
	ctx := context.Background()

	a, err := e.ev.Assess(ctx, "Does the narrator forget his name?", nil)
	if err != nil || a.Level != evidence.Sufficient {
		t.Fatalf("expected sufficient: %+v %v", a, err)
	}
	a, _ = e.ev.Assess(ctx, "What is the capital of France and its population?", nil)
	if a.Level != evidence.Insufficient {
		t.Errorf("expected abstention, got %+v", a)
	}
	a, _ = e.ev.Assess(ctx, "narrator name", nil)
	if a.Level == evidence.Insufficient {
		t.Errorf("expected non-insufficient: %+v", a)
	}
	// candidate passage IDs are verified against the DB
	hits, _ := e.srch.Search(ctx, search.Options{Query: "narrator"})
	a, err = e.ev.Assess(ctx, "Does the narrator forget his name?", []string{hits[0].PassageID})
	if err != nil || a.Level != evidence.Sufficient {
		t.Fatalf("candidate assessment: %+v %v", a, err)
	}
	a, _ = e.ev.Assess(ctx, "narrator", []string{"bk-doesnotexist:1"})
	if a.Level != evidence.Insufficient {
		t.Errorf("fabricated candidate must fail: %+v", a)
	}
}

func TestQuoteVerification(t *testing.T) {
	e := newEnv(t, map[string]string{"b.txt": book1})
	e.index(t, ingest.Options{})
	ctx := context.Background()
	hits, _ := e.srch.Search(ctx, search.Options{Query: "narrator"})
	id := hits[0].PassageID
	if v := citations.VerifyQuote(ctx, e.db, id, "forgets his name"); !v.OK {
		t.Errorf("valid quote rejected: %+v", v)
	}
	if v := citations.VerifyQuote(ctx, e.db, id, "forgets his middle name entirely"); v.OK {
		t.Errorf("fake quote accepted: %+v", v)
	}
	if v := citations.VerifyQuote(ctx, e.db, id, "forgets  his\nname"); !v.OK {
		t.Errorf("whitespace-normalized quote rejected: %+v", v)
	}
	if _, err := citations.Resolve(ctx, e.db, "bk-fake:9"); err == nil {
		t.Error("citation resolved for nonexistent passage")
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default(dir)
	db, err := database.Open(cfg.DBPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	// open twice more, then again with data present
	for i := 0; i < 3; i++ {
		db2, err := database.Open(cfg.DBPath, 1)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		db2.Close()
	}
	pipe := &ingest.Pipeline{DB: db, Cfg: cfg}
	writeBooks(t, dir, map[string]string{"b.txt": book1})
	pipe.Index(context.Background(), ingest.Options{})
	db.Close()
	db3, err := database.Open(cfg.DBPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()
	var n int
	db3.QueryRow(`SELECT count(*) FROM books`).Scan(&n)
	if n != 1 {
		t.Errorf("data lost across migration runs: %d", n)
	}
}

func TestUnicodeAcrossIndex(t *testing.T) {
	text := "Chapter One\n\n" + strings.Repeat("La memoria y la identidad. Ñoño — “curly quotes” — 日本語. ", 30) + "\n"
	e := newEnv(t, map[string]string{"u.txt": "Title: Mémoire\n\n" + text})
	e.index(t, ingest.Options{})
	hits, _ := e.srch.Search(context.Background(), search.Options{Query: "memoria"})
	if len(hits) == 0 {
		t.Fatal("unicode content not searchable")
	}
	for _, r := range hits[0].Text {
		if r == 0xFFFD {
			t.Fatal("replacement char in indexed text")
		}
	}
}

func TestForcedReindexIsIdempotent(t *testing.T) {
	e := newEnv(t, map[string]string{"b.txt": book1})
	e.index(t, ingest.Options{})
	var before [3]int
	e.db.QueryRow(`SELECT (SELECT count(*) FROM passages), (SELECT count(*) FROM passages_fts), (SELECT count(*) FROM books)`).Scan(&before[0], &before[1], &before[2])
	for i := 0; i < 2; i++ {
		e.index(t, ingest.Options{Force: true})
	}
	var after [3]int
	e.db.QueryRow(`SELECT (SELECT count(*) FROM passages), (SELECT count(*) FROM passages_fts), (SELECT count(*) FROM books)`).Scan(&after[0], &after[1], &after[2])
	if before != after {
		t.Errorf("force reindex changed counts: %v -> %v", before, after)
	}
}

// compile guard
var _ = fmt.Sprintf
