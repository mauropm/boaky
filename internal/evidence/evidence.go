// Package evidence enforces the book-only answer policy: before any answer
// generation, Boaky checks that the local collection actually contains
// relevant passages. The check is lexical (FTS relevance + token coverage),
// a strong signal but NOT semantic proof — this limitation is documented.
package evidence

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/mauro/boaky/internal/config"
	"github.com/mauro/boaky/internal/search"
)

// Level classifies the gate outcome.
type Level string

const (
	Sufficient   Level = "sufficient"
	Partial      Level = "partial"
	Insufficient Level = "insufficient"
)

// Assessment is the structured gate decision returned to callers.
type Assessment struct {
	Level         Level        `json:"level"`
	Question      string       `json:"question"`
	Reason        string       `json:"reason"`
	Coverage      float64      `json:"coverage"`
	MissingTokens []string     `json:"missing_tokens,omitempty"`
	CoveredTokens []string     `json:"covered_tokens,omitempty"`
	TopScore      float64      `json:"top_score"`
	ResultCount   int          `json:"result_count"`
	PassageIDs    []string     `json:"passage_ids,omitempty"`
	Hits          []search.Hit `json:"hits,omitempty"`
}

// Service evaluates evidence against the index.
type Service struct {
	DB     *sql.DB
	Cfg    config.Config
	Search *search.Service
}

// Assess searches the index for question and scores evidence sufficiency.
// If candidate passage IDs are supplied, they are verified against the DB
// and the check uses them instead of a fresh search (used by check_evidence).
func (s *Service) Assess(ctx context.Context, question string, candidates []string) (Assessment, error) {
	a := Assessment{Question: question}
	if strings.TrimSpace(question) == "" {
		a.Level = Insufficient
		a.Reason = "empty question"
		return a, nil
	}
	tokens := search.TokenizeWords(question)
	if len(tokens) == 0 {
		a.Level = Insufficient
		a.Reason = "question contains no searchable content words"
		return a, nil
	}

	var hits []search.Hit
	limit := s.Cfg.SearchLimit
	if limit < 1 {
		limit = 10
	}
	if len(candidates) == 0 {
		var err error
		hits, err = s.Search.Search(ctx, search.Options{Query: question, Limit: limit})
		if err != nil {
			return a, err
		}
		a.Hits = hits
	} else {
		if len(candidates) > 32 {
			candidates = candidates[:32]
		}
		var err error
		hits, err = s.Search.GetPassages(ctx, candidates)
		if err != nil {
			return a, err
		}
		if len(hits) != len(candidates) {
			found := map[string]bool{}
			for _, h := range hits {
				found[h.PassageID] = true
			}
			var missing []string
			for _, id := range candidates {
				if !found[id] {
					missing = append(missing, id)
				}
			}
			a.Reason = fmt.Sprintf("citation check: %d requested passage(s) not in index: %s",
				len(missing), strings.Join(missing, ", "))
			a.Level = Insufficient
			return a, nil
		}
		// Score candidates against the question via a real search restricted
		// conceptually: run question search, intersect with candidates.
		qHits, err := s.Search.Search(ctx, search.Options{Query: question, Limit: 200})
		if err != nil {
			return a, err
		}
		inSet := map[string]bool{}
		for _, id := range candidates {
			inSet[id] = true
		}
		var inter []search.Hit
		for _, h := range qHits {
			if inSet[h.PassageID] {
				inter = append(inter, h)
			}
		}
		if len(inter) > 0 {
			hits = inter
		}
		a.Hits = hits
	}

	a.ResultCount = len(hits)
	a.PassageIDs = nil
	if len(hits) > 0 {
		a.TopScore = hits[0].Score
		for _, h := range hits {
			a.PassageIDs = append(a.PassageIDs, h.PassageID)
		}
	}
	var sb strings.Builder
	for _, h := range hits {
		sb.WriteString(h.Text)
		sb.WriteString(" ")
		sb.WriteString(h.Title)
		sb.WriteString(" ")
		sb.WriteString(h.Heading)
		sb.WriteString(" ")
	}
	corpus := strings.ToLower(sb.String())
	for _, t := range tokens {
		if strings.Contains(corpus, t) {
			a.CoveredTokens = append(a.CoveredTokens, t)
		} else {
			a.MissingTokens = append(a.MissingTokens, t)
		}
	}
	if len(tokens) > 0 {
		a.Coverage = float64(len(a.CoveredTokens)) / float64(len(tokens))
	}

	// BM25's idf term is only meaningful once the corpus has real document
	// statistics. On tiny corpora every idf collapses toward zero, so an
	// absolute score floor there is meaningless; presence plus token
	// coverage is the honest signal. Above the corpus floor, the configured
	// score threshold must also pass.
	scoreOK := true
	var npassages int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM passages`).Scan(&npassages); err == nil &&
		npassages >= s.Cfg.EvidenceMinPassagesForScore {
		scoreOK = a.TopScore >= s.Cfg.EvidenceMinScore
	}

	switch {
	case len(hits) >= s.Cfg.EvidenceMinResults && scoreOK &&
		a.Coverage >= s.Cfg.EvidenceMinCoverage:
		a.Level = Sufficient
		a.Reason = "relevant indexed passages found"
	case len(hits) > 0 && a.Coverage >= s.Cfg.EvidencePartialCover:
		a.Level = Partial
		a.Reason = fmt.Sprintf("only some question concepts are covered (%d of %d tokens); missing: %s",
			len(a.CoveredTokens), len(tokens), strings.Join(a.MissingTokens, ", "))
	case len(hits) == 0:
		a.Level = Insufficient
		a.Reason = "no passages match the question"
	default:
		a.Level = Insufficient
		a.Reason = fmt.Sprintf("relevance or coverage below threshold (score %.2f, coverage %.0f%%)",
			a.TopScore, a.Coverage*100)
	}
	return a, nil
}
