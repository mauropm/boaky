package search

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mauro/boaky/internal/config"
	"github.com/mauro/boaky/internal/database"
)

// Hit is one ranked passage. Score is -bm25() (higher is better) and is a
// relevance signal only; it never proves that a passage supports a claim.
type Hit struct {
	PassageID   string  `json:"passage_id"`
	BookID      string  `json:"book_id"`
	Title       string  `json:"title"`
	Author      string  `json:"author,omitempty"`
	RelPath     string  `json:"rel_path"`
	Heading     string  `json:"heading,omitempty"`
	CharStart   int     `json:"char_start"`
	CharEnd     int     `json:"char_end"`
	Text        string  `json:"text"`
	ContentHash string  `json:"content_hash"`
	Score       float64 `json:"score"`
}

// Options parameterizes one search.
type Options struct {
	Query  string
	Title  string // substring filter on title
	Author string // substring filter on author
	Limit  int    // 0 = config default
}

// Service searches passages.
type Service struct {
	DB  *sql.DB
	Cfg config.Config
}

// SchemaVersion exposes the db migration version (used by doctor/status).
func SchemaVersion(db *sql.DB) int { return database.MustVersion(db) }

// Search executes the query with BM25 ranking, dedup, per-book caps, and a
// context-size budget. It never errors on an empty result — ErrNoMatch style
// empty slices are how "nothing found" is returned.
func (s *Service) Search(ctx context.Context, opt Options) ([]Hit, error) {
	limit := opt.Limit
	if limit <= 0 {
		limit = s.Cfg.SearchLimit
	}
	if limit > 200 {
		limit = 200
	}
	match, _ := BuildQuery(opt.Query)
	if match == "" {
		return nil, nil
	}
	hits, err := s.run(ctx, match, opt, limit)
	if err != nil {
		// Malformed FTS input should be impossible thanks to escaping, but a
		// driver quirk must not crash the caller.
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(hits) == 0 {
		// Retry as OR when AND was too strict (multi-token natural queries).
		or, _ := BuildORQuery(opt.Query)
		if or != match {
			hits, err = s.run(ctx, or, opt, limit)
			if err != nil {
				return nil, err
			}
		}
	}
	return s.post(hits, limit), nil
}

func (s *Service) run(ctx context.Context, match string, opt Options, limit int) ([]Hit, error) {
	q := `SELECT p.passage_id, p.book_id, b.title, b.author, b.rel_path, p.heading,
	p.char_start, p.char_end, p.text, p.content_hash,
	-bm25(passages_fts, 1.0, 2.0, 3.0) AS score
	FROM passages_fts
	JOIN passages p ON p.pid = passages_fts.rowid
	JOIN books b ON b.id = p.book_id
	WHERE passages_fts MATCH ? AND b.status = 'indexed'`
	if opt.Title != "" {
		q += " AND lower(b.title) LIKE ?"
	}
	if opt.Author != "" {
		q += " AND lower(b.author) LIKE ?"
	}
	q += " ORDER BY score DESC LIMIT ?"
	rows, err := s.DB.QueryContext(ctx, q, queryArgs(match, opt, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.PassageID, &h.BookID, &h.Title, &h.Author, &h.RelPath,
			&h.Heading, &h.CharStart, &h.CharEnd, &h.Text, &h.ContentHash, &h.Score); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// queryArgs assembles []any in SQL placeholder order: match, [title], [author], limit.
func queryArgs(match string, opt Options, limit int) []any {
	args := []any{match}
	if opt.Title != "" {
		args = append(args, "%"+strings.ToLower(opt.Title)+"%")
	}
	if opt.Author != "" {
		args = append(args, "%"+strings.ToLower(opt.Author)+"%")
	}
	return append(args, limit*4)
}

// post applies near-duplicate reduction, per-book caps, and the context budget.
func (s *Service) post(hits []Hit, limit int) []Hit {
	perBook := map[string]int{}
	seenHash := map[string]bool{}
	var out []Hit
	total := 0
	maxCtx := s.Cfg.MaxContextChars
	if maxCtx <= 0 {
		maxCtx = 12000
	}
	for _, h := range hits {
		if len(out) >= limit {
			break
		}
		if seenHash[h.ContentHash] {
			continue
		}
		if s.Cfg.MaxHitsPerBook > 0 && perBook[h.BookID] >= s.Cfg.MaxHitsPerBook {
			continue
		}
		// Drop near-duplicates overlapping a kept passage of the same book.
		dup := false
		for _, k := range out {
			if k.BookID == h.BookID && overlapRunes(k, h) > 3 {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		n := utf8.RuneCountInString(h.Text)
		if len(out) > 0 && total+n > maxCtx {
			continue
		}
		seenHash[h.ContentHash] = true
		perBook[h.BookID]++
		total += n
		out = append(out, h)
	}
	return out
}

// overlapRunes estimates shared text length between two same-book passages.
func overlapRunes(a, b Hit) int {
	as, ae := a.CharStart, a.CharEnd
	bs, be := b.CharStart, b.CharEnd
	lo, hi := max(as, bs), min(ae, be)
	if hi <= lo {
		return 0
	}
	return hi - lo
}

// GetPassages returns exact passages by ID (parameters, not interpolation).
func (s *Service) GetPassages(ctx context.Context, ids []string) ([]Hit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 64 {
		ids = ids[:64]
	}
	qs := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := `SELECT p.passage_id, p.book_id, b.title, b.author, b.rel_path, p.heading,
	p.char_start, p.char_end, p.text, p.content_hash, 0
	FROM passages p JOIN books b ON b.id = p.book_id
	WHERE p.passage_id IN (` + qs + `) ORDER BY p.passage_id`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.PassageID, &h.BookID, &h.Title, &h.Author, &h.RelPath,
			&h.Heading, &h.CharStart, &h.CharEnd, &h.Text, &h.ContentHash, &h.Score); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
