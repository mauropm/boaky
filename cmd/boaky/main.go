// boaky: local-first book evidence engine.
//
// Commands:
//
//	boaky index [--force] [--dry-run] [--reconcile] [--file FILE]
//	boaky status
//	boaky list
//	boaky inspect <book-id-or-path>
//	boaky search "query" [--title T] [--author A] [--limit N] [--json]
//	boaky answer "question" [--no-generate] [--json]
//	boaky doctor
//	boaky serve        (MCP over stdio)
//	boaky serve-http   (MCP over streamable HTTP)
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mauro/boaky/internal/answer"
	"github.com/mauro/boaky/internal/config"
	"github.com/mauro/boaky/internal/database"
	"github.com/mauro/boaky/internal/evidence"
	"github.com/mauro/boaky/internal/generate"
	"github.com/mauro/boaky/internal/ingest"
	"github.com/mauro/boaky/internal/logging"
	"github.com/mauro/boaky/internal/mcp"
	"github.com/mauro/boaky/internal/search"
)

const version = "1.0.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("missing command")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd, rest := args[0], args[1:]
	cfg := config.FromEnv(".")
	log := logging.New(cfg.LogLevel)

	db, err := database.Open(cfg.DBPath, cfg.DBPoolSize)
	if err != nil {
		if cmd != "doctor" {
			return fmt.Errorf("open database: %w", err)
		}
	}
	if db != nil {
		defer db.Close()
	}

	switch cmd {
	case "index":
		return cmdIndex(ctx, db, cfg, log, rest)
	case "status":
		return cmdStatus(ctx, db, cfg)
	case "list":
		return cmdList(ctx, db, cfg)
	case "inspect":
		return cmdInspect(ctx, db, cfg, rest)
	case "search":
		return cmdSearch(ctx, db, cfg, rest)
	case "answer":
		return cmdAnswer(ctx, db, cfg, log, rest)
	case "doctor":
		return cmdDoctor(ctx, cfg, db, log)
	case "health":
		return cmdHealth(cfg)
	case "serve":
		return cmdServe(ctx, db, cfg, log)
	case "serve-http":
		return cmdServeHTTP(ctx, db, cfg, log)
	case "version", "--version", "-v":
		fmt.Println("boaky", version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: boaky <command> [flags]

commands:
  index [--force] [--dry-run] [--reconcile] [--file FILE ...]
  status
  list
  inspect <book-id-or-path>
  search "query" [--title T] [--author A] [--limit N] [--json]
  answer "question" [--no-generate] [--json]
  doctor
  serve          MCP server over stdio
  serve-http     MCP server over streamable HTTP (default addr :8000)

configuration (env): BOAKY_BOOKS_DIR, BOAKY_DB_PATH, BOAKY_META_DIR,
BOAKY_PASSAGE_MAX_RUNES, BOAKY_PASSAGE_OVERLAP_RUNES, BOAKY_SEARCH_LIMIT,
BOAKY_MAX_CONTEXT_CHARS, BOAKY_EVIDENCE_MIN_SCORE, BOAKY_EVIDENCE_MIN_COVERAGE,
BOAKY_HTTP_ADDR, BOAKY_GEN_BASE_URL, BOAKY_GEN_API_KEY, BOAKY_GEN_MODEL,
BOAKY_LOG_LEVEL. See .env.example.`)
}

func services(db *sql.DB, cfg config.Config, log interface{ Info(string, ...any) }) *mcp.Deps {
	svc := &search.Service{DB: db, Cfg: cfg}
	pipe := &ingest.Pipeline{DB: db, Cfg: cfg}
	eval := &evidence.Service{DB: db, Cfg: cfg, Search: svc}
	gen := &generate.Client{
		BaseURL: cfg.GenBaseURL, APIKey: cfg.GenAPIKey, Model: cfg.GenModel,
		Timeout: time.Duration(cfg.GenTimeout) * time.Second,
	}
	ans := &answer.Services{Cfg: cfg, Search: svc, Eval: eval, Gen: gen}
	return &mcp.Deps{DB: db, Cfg: cfg, Log: logging.New(cfg.LogLevel), Search: svc, Pipe: pipe, Eval: eval, Answer: ans}
}

func cmdIndex(ctx context.Context, db *sql.DB, cfg config.Config, log interface{ Info(string, ...any) }, args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	var files stringList
	fs.Var(&files, "file", "index only this path (repeatable)")
	force := fs.Bool("force", false, "rebuild index even for unchanged files")
	dry := fs.Bool("dry-run", false, "report what would change, change nothing")
	reconcile := fs.Bool("reconcile", false, "remove records for vanished files")
	fs.Parse(args)

	pipe := &ingest.Pipeline{DB: db, Cfg: cfg}
	rep, err := pipe.Index(ctx, ingest.Options{
		Force: *force, DryRun: *dry, Reconcile: *reconcile, Files: files,
	})
	if err != nil {
		return err
	}
	for _, r := range rep.Results {
		line := fmt.Sprintf("%-40s %-10s %4d passages", trunc(r.RelPath, 40), r.Status, r.Passages)
		if r.Error != "" {
			line += "  ERROR: " + trunc(r.Error, 120)
		}
		fmt.Println(line)
	}
	mode := "indexed"
	if *dry {
		mode = "(dry-run) would index"
	}
	fmt.Printf("\n%d file(s) scanned: %d %s, %d skipped, %d failed, %d removed/missing\n",
		rep.Scanned, rep.Indexed, mode, rep.Skipped, rep.Failed, rep.Removed)
	if rep.Failed > 0 {
		return errors.New("some files failed to index")
	}
	return nil
}

func cmdStatus(ctx context.Context, db *sql.DB, cfg config.Config) error {
	pipe := &ingest.Pipeline{DB: db, Cfg: cfg}
	st, err := pipe.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("books dir     %s\n", st.BooksDir)
	fmt.Printf("database      %s (%.1f MB, schema v%d)\n", st.DBPath, float64(st.IndexSizeBytes)/(1<<20), st.SchemaVersion)
	fmt.Printf("books         %d indexed, %d error, %d missing\n", st.Indexed, st.Errored, st.Missing)
	fmt.Printf("passages      %d\n", st.Passages)
	return nil
}

func cmdList(ctx context.Context, db *sql.DB, cfg config.Config) error {
	pipe := &ingest.Pipeline{DB: db, Cfg: cfg}
	books, err := pipe.ListBooks(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%-16s %-40s %-25s %-8s %8s  %s\n", "ID", "TITLE", "AUTHOR", "STATUS", "PASSAGES", "PATH")
	for _, b := range books {
		t := b.Title
		if len(t) > 40 {
			t = t[:37] + "..."
		}
		a := b.Author
		if len(a) > 25 {
			a = a[:22] + "..."
		}
		fmt.Printf("%-16s %-40s %-25s %-8s %8d  %s\n", b.ID, t, a, b.Status, b.Passages, b.RelPath)
	}
	return nil
}

func cmdInspect(ctx context.Context, db *sql.DB, cfg config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: boaky inspect <book-id-or-path>")
	}
	pipe := &ingest.Pipeline{DB: db, Cfg: cfg}
	detail, err := pipe.Inspect(ctx, args[0])
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(detail, "", "  ")
	fmt.Println(string(out))
	return nil
}

func cmdSearch(ctx context.Context, db *sql.DB, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	title := fs.String("title", "", "filter by title substring")
	author := fs.String("author", "", "filter by author substring")
	limit := fs.Int("limit", 0, "max results (default from config)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Parse(args)
	query := strings.Join(fs.Args(), " ")
	if query == "" {
		return errors.New(`usage: boaky search "query" [flags]`)
	}
	svc := &search.Service{DB: db, Cfg: cfg}
	hits, err := svc.Search(ctx, search.Options{Query: query, Title: *title, Author: *author, Limit: *limit})
	if err != nil {
		return err
	}
	if *asJSON {
		out, _ := json.MarshalIndent(hits, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	if len(hits) == 0 {
		fmt.Println("no passages found.")
		return nil
	}
	for _, h := range hits {
		fmt.Printf("\n── %s | %s | characters %d–%d | score %.2f\n",
			h.Title, h.PassageID, h.CharStart, h.CharEnd, h.Score)
		if h.Author != "" {
			fmt.Printf("   author: %s | %s\n", h.Author, h.RelPath)
		} else {
			fmt.Printf("   %s\n", h.RelPath)
		}
		if h.Heading != "" {
			fmt.Printf("   section: %s\n", h.Heading)
		}
		fmt.Printf("   %s\n", excerpt(h.Text, 300))
	}
	return nil
}

func cmdAnswer(ctx context.Context, db *sql.DB, cfg config.Config, log interface{ Info(string, ...any) }, args []string) error {
	fs := flag.NewFlagSet("answer", flag.ExitOnError)
	noGen := fs.Bool("no-generate", false, "skip generation even if configured (abstain/return evidence)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Parse(args)
	q := strings.Join(fs.Args(), " ")
	if q == "" {
		return errors.New(`usage: boaky answer "question"`)
	}
	d := services(db, cfg, log)
	if *noGen {
		d.Answer.Gen = &generate.Client{} // disabled
	}
	res, err := d.Answer.Ask(ctx, q)
	if err != nil {
		return err
	}
	if *asJSON {
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	fmt.Printf("status: %s\n", res.Status)
	if res.Answer != "" {
		fmt.Printf("\n%s\n", res.Answer)
	}
	if res.Reason != "" {
		fmt.Printf("\nreason: %s\n", res.Reason)
	}
	fmt.Printf("\nevidence: level=%s coverage=%.0f%% top_score=%.2f passages=%d generation_used=%v\n",
		res.Evidence.Level, res.Evidence.Coverage*100, res.Evidence.TopScore, res.Evidence.ResultCount, res.GenerationUsed)
	if len(res.Citations) > 0 {
		fmt.Println("\ncitations:")
		for _, c := range res.Citations {
			fmt.Printf("  [%s] %s", c.PassageID, c.Title)
			if c.Author != "" {
				fmt.Printf(" — %s", c.Author)
			}
			fmt.Printf(" (%s, characters %d–%d)\n", c.RelPath, c.CharStart, c.CharEnd)
		}
	}
	return nil
}

// cmdHealth probes a running serve-http instance (used by the container
// healthcheck). It works without a DB handle so it cannot deadlock.
func cmdHealth(cfg config.Config) error {
	scheme := "http"
	addr := cfg.HTTPAddr
	if addr == "" {
		addr = "127.0.0.1:8000"
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(scheme + "://" + addr + cfg.HealthPath)
	if err != nil {
		return fmt.Errorf("health probe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health probe: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Status != "ok" {
		return fmt.Errorf("health probe: bad response")
	}
	fmt.Println("ok")
	return nil
}

func cmdDoctor(ctx context.Context, cfg config.Config, db *sql.DB, log interface{ Info(string, ...any) }) error {
	ok := true
	fail := func(label, err string) {
		ok = false
		fmt.Printf("✗ %s: %s\n", label, err)
	}
	pass := func(label, detail string) { fmt.Printf("✓ %s: %s\n", label, detail) }

	if fi, err := os.Stat(cfg.BooksDir); err != nil {
		fail("books directory", err.Error())
	} else if !fi.IsDir() {
		fail("books directory", cfg.BooksDir+" is not a directory")
	} else {
		pass("books directory", cfg.BooksDir)
	}
	if db == nil {
		fail("database", "could not open "+cfg.DBPath)
		return finishDoctor(ok)
	}
	pass("database", cfg.DBPath)
	var fts string
	if err := db.QueryRow(`SELECT 'ok' FROM sqlite_master WHERE type='table' AND name='passages_fts'`).Scan(&fts); err != nil {
		fail("FTS5 table", "missing (schema not migrated?)")
	} else {
		pass("FTS5 virtual table", "present")
	}
	if err := execOnly(db, `CREATE VIRTUAL TABLE temp.boaky_probe USING fts5(x)`); err != nil {
		fail("FTS5 runtime", "driver has no FTS5: "+err.Error())
	} else {
		db.Exec(`DROP TABLE temp.boaky_probe`)
		pass("FTS5 runtime", "supported by driver")
	}
	var probe string
	err := db.QueryRow(`SELECT 'ok' WHERE 1`).Scan(&probe)
	if err != nil {
		fail("database query", err.Error())
	}
	var ver string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&ver); err != nil {
		fail("sqlite version", err.Error())
	} else {
		pass("sqlite", ver+" (FTS5: "+ftsOK(db)+")")
	}
	pass("schema", fmt.Sprintf("version %d", database.MustVersion(db)))
	if err := os.MkdirAll(cfg.MetaDir, 0o755); err != nil {
		fail("metadata dir", err.Error())
	} else {
		pass("metadata dir", cfg.MetaDir)
	}
	var total, books int
	db.QueryRow(`SELECT COUNT(*) FROM passages_fts`).Scan(&total)
	db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&books)
	fmt.Printf("  index: %d books, %d indexed passages\n", books, total)
	var orphan int
	db.QueryRow(`SELECT COUNT(*) FROM passages_fts WHERE rowid NOT IN (SELECT pid FROM passages)`).Scan(&orphan)
	if orphan > 0 {
		fail("FTS consistency", fmt.Sprintf("%d orphan FTS row(s) (run boaky index --force)", orphan))
	} else {
		pass("FTS consistency", "no orphan rows")
	}
	var drift int
	db.QueryRow(`SELECT COUNT(*) FROM books WHERE status='indexed' AND passage_count <> (SELECT COUNT(*) FROM passages WHERE passages.book_id = books.id)`).Scan(&drift)
	if drift > 0 {
		fail("index drift", fmt.Sprintf("%d book(s) disagree with passage counts (run boaky index --force)", drift))
	} else {
		pass("index counts", "consistent")
	}
	if cfg.GenBaseURL != "" && cfg.GenAPIKey != "" {
		pass("generation", "configured ("+cfg.GenBaseURL+", model "+cfg.GenModel+")")
	} else {
		fmt.Println("• generation: not configured — 'boaky answer' returns evidence/abstention only")
	}
	return finishDoctor(ok)
}

func finishDoctor(ok bool) error {
	if ok {
		fmt.Println("\nall checks passed")
		return nil
	}
	return errors.New("doctor found problems")
}

func execOnly(db *sql.DB, q string) error {
	_, err := db.Exec(q)
	return err
}

func ftsOK(db *sql.DB) string {
	if err := execOnly(db, `CREATE VIRTUAL TABLE temp.boaky_ok USING fts5(x)`); err != nil {
		return "NO"
	}
	db.Exec(`DROP TABLE temp.boaky_ok`)
	return "yes"
}

func cmdServe(ctx context.Context, db *sql.DB, cfg config.Config, log interface{ Info(string, ...any) }) error {
	d := services(db, cfg, log)
	srv := mcp.NewServer(*d)
	log.Info("boaky MCP server starting", "transport", "stdio")
	return srv.ServeStdio(ctx)
}

func cmdServeHTTP(ctx context.Context, db *sql.DB, cfg config.Config, log interface{ Info(string, ...any) }) error {
	d := services(db, cfg, log)
	srv := mcp.NewServer(*d)
	handler := srv.HTTPHandler()

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutCtx)
	}()
	log.Info("boaky MCP server starting", "addr", cfg.HTTPAddr, "mcp", cfg.MCPPath, "health", cfg.HealthPath)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func excerpt(text string, max int) string {
	r := []rune(strings.TrimSpace(text))
	out := string(r)
	if len(r) > max {
		out = string(r[:max]) + "…"
	}
	return strings.Join(strings.Fields(out), " ")
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

var _ = sort.Strings
