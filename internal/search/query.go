// Package search turns natural-language questions into SQLite FTS5 queries
// and returns ranked passages. The user never needs to know FTS5 syntax;
// everything is quoted and escaped before it reaches the MATCH clause.
package search

import (
	"regexp"
	"strings"
	"unicode"
)

// Tokenize splits text into lowercased word tokens. Tokens containing
// apostrophes or hyphens stay intact ("don't", "well-known").
var wordRe = regexp.MustCompile(`[\p{L}\p{N}'’\-]*[\p{L}\p{N}][\p{L}\p{N}'’\-]*`)

// Common English stopwords: dropped from AND queries because they would
// otherwise dominate coverage scoring.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "been": true, "but": true, "by": true, "can": true, "could": true,
	"did": true, "do": true, "does": true, "for": true, "from": true, "had": true,
	"has": true, "have": true, "he": true, "her": true, "his": true, "how": true,
	"i": true, "if": true, "in": true, "is": true, "it": true, "its": true,
	"me": true, "my": true, "not": true, "of": true, "on": true, "or": true,
	"our": true, "she": true, "so": true, "than": true, "that": true, "the": true,
	"their": true, "them": true, "then": true, "there": true, "these": true,
	"they": true, "this": true, "to": true, "too": true, "up": true, "us": true,
	"was": true, "we": true, "were": true, "what": true, "when": true, "where": true,
	"which": true, "who": true, "why": true, "will": true, "with": true, "would": true,
	"you": true, "your": true,
}

// TokenizeWords returns content tokens (stopwords removed, length >= 2).
func TokenizeWords(s string) []string {
	raw := wordRe.FindAllString(strings.ToLower(s), -1)
	var out []string
	seen := map[string]bool{}
	for _, w := range raw {
		w = strings.Trim(w, "'’-")
		if len([]rune(w)) < 2 || stopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// phraseRe captures double-quoted phrases in a user query.
var phraseRe = regexp.MustCompile(`"([^"]+)"`)

// BuildQuery converts free text into an FTS5 MATCH string that is always
// syntactically valid: words become quoted prefix terms, quoted phrases
// become exact phrases, all combined with AND. Returns the MATCH string and
// the content tokens used (for coverage scoring).
func BuildQuery(q string) (match string, tokens []string) {
	tokens = TokenizeWords(q)
	var terms []string
	// Phrases: exact, in order.
	for _, m := range phraseRe.FindAllStringSubmatch(q, -1) {
		phrase := strings.TrimSpace(strings.ToLower(m[1]))
		if phrase == "" {
			continue
		}
		terms = append(terms, `"`+escapeFTS(phrase)+`"`)
	}
	// Words: prefix matches, capped so one query cannot build a monster.
	taken := 0
	for _, t := range tokens {
		if taken >= 12 {
			break
		}
		terms = append(terms, `"`+escapeFTS(t)+`"*`)
		taken++
	}
	return strings.Join(terms, " AND "), tokens
}

// BuildORQuery is the fallback when AND yields nothing. Phrase queries do
// not fall back to OR: a phrase is already a precision request, and ORing
// its terms would match anything containing any single word.
func BuildORQuery(q string) (string, []string) {
	match, tokens := BuildQuery(q)
	if !strings.Contains(match, " AND ") || strings.Contains(match, `" `) {
		return match, tokens
	}
	return strings.ReplaceAll(match, " AND ", " OR "), tokens
}

// HasPhrase reports whether the built match contains an exact phrase term.
func HasPhrase(q string) bool {
	return strings.Contains(q, `"`)
}

func escapeFTS(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '"' {
			b.WriteString(`""`)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// EnsureTokenShape keeps unicode helpers referenced by tests.
var _ = unicode.IsLetter
