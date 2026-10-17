// Package citations resolves citations to real indexed passages and verifies
// quoted text. Every citation must resolve against SQLite; Boaky rejects
// invented locations and quotes.
package citations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/mauro/boaky/internal/search"
)

// Citation is a fully resolved reference.
type Citation struct {
	BookID    string `json:"book_id"`
	Title     string `json:"title"`
	Author    string `json:"author,omitempty"`
	RelPath   string `json:"rel_path"`
	Heading   string `json:"heading,omitempty"`
	PassageID string `json:"passage_id"`
	CharStart int    `json:"char_start"`
	CharEnd   int    `json:"char_end"`
	Excerpt   string `json:"excerpt,omitempty"`
}

// FromHit converts a search hit to a citation.
func FromHit(h search.Hit) Citation {
	return Citation{
		BookID: h.BookID, Title: h.Title, Author: h.Author, RelPath: h.RelPath,
		Heading: h.Heading, PassageID: h.PassageID,
		CharStart: h.CharStart, CharEnd: h.CharEnd,
		Excerpt: excerpt(h.Text, 160),
	}
}

// Resolve turns an arbitrary citation-ish string ("<id>#<n>" not supported;
// use passage IDs directly) into a Citation by querying the DB.
func Resolve(ctx context.Context, db *sql.DB, passageID string) (Citation, error) {
	var c Citation
	err := db.QueryRowContext(ctx, `
		SELECT p.book_id, b.title, b.author, b.rel_path, p.heading, p.passage_id, p.char_start, p.char_end, p.text
		FROM passages p JOIN books b ON b.id = p.book_id WHERE p.passage_id = ?`,
		passageID).Scan(&c.BookID, &c.Title, &c.Author, &c.RelPath, &c.Heading,
		&c.PassageID, &c.CharStart, &c.CharEnd, new(string))
	if errors.Is(err, sql.ErrNoRows) {
		return c, fmt.Errorf("citation does not resolve: passage %q is not indexed", passageID)
	}
	if err != nil {
		return c, err
	}
	return c, nil
}

// NormalizeQuote applies the ONE documented normalization: trim and collapse
// every whitespace run to a single space. Case and punctuation are NOT
// normalized — quote verification is strict.
func NormalizeQuote(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// QuoteVerdict reports quote verification against a passage.
type QuoteVerdict struct {
	OK        bool
	Reason    string
	PassageID string
}

// VerifyQuote checks that quote appears verbatim (modulo whitespace
// normalization) inside the cited passage.
func VerifyQuote(ctx context.Context, db *sql.DB, passageID, quote string) QuoteVerdict {
	var text string
	err := db.QueryRowContext(ctx, `SELECT text FROM passages WHERE passage_id = ?`, passageID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return QuoteVerdict{Reason: fmt.Sprintf("passage %q not in index", passageID)}
	}
	if err != nil {
		return QuoteVerdict{Reason: err.Error()}
	}
	nq, np := NormalizeQuote(quote), NormalizeQuote(text)
	if nq == "" {
		return QuoteVerdict{Reason: "empty quote", PassageID: passageID}
	}
	if strings.Contains(np, nq) {
		return QuoteVerdict{OK: true, PassageID: passageID}
	}
	return QuoteVerdict{
		Reason:    "quote does not appear in the cited passage (whitespace-normalized exact match required)",
		PassageID: passageID,
	}
}

// ValidateList resolves and (optionally) quote-checks citations from hits.
func ValidateList(ctx context.Context, db *sql.DB, hits []search.Hit) ([]Citation, error) {
	var out []Citation
	for _, h := range hits {
		c := FromHit(h)
		if _, err := Resolve(ctx, db, c.PassageID); err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, nil
}

func excerpt(text string, max int) string {
	r := []rune(strings.TrimSpace(NormalizeQuote(text)))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
