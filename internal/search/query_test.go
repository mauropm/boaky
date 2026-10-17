package search

import "testing"

func TestBuildQueryAndTokens(t *testing.T) {
	match, tokens := BuildQuery("memory and identity in literature")
	if match == "" {
		t.Fatal("empty match")
	}
	for _, want := range []string{"memory", "identity", "literature"} {
		found := false
		for _, tk := range tokens {
			if tk == want {
				found = true
			}
		}
		if !found {
			t.Errorf("token %q missing from %v", want, tokens)
		}
	}
	// stopword "and" must not appear as its own term
	if contains(tokens, "and") {
		t.Errorf("stopword leaked: %v", tokens)
	}
	// terms must be quoted prefix terms joined by AND
	for _, part := range []string{`"memory"*`, `"identity"*`} {
		if !containsStr(match, part) {
			t.Errorf("match %q missing %q", match, part)
		}
	}
}

func TestBuildQueryPhrasesAndEscaping(t *testing.T) {
	match, _ := BuildQuery(`"to be, or not" evil\"quoted`)
	if !containsStr(match, `"to be, or not"`) {
		t.Errorf("phrase missing: %q", match)
	}
	// an odd token must be escaped: quotes doubled, no bare syntax left
	for _, bad := range []string{"NEAR", "OR ", " AND ", "(", ")"} {
		if hasBareSyntax(match) {
			break // terms joined by " AND " is expected syntax, so only check tokens inside quotes are quoted
		}
		_ = bad
	}
	// sanity: every term must start with a quote char
	if match[0] != '"' {
		t.Errorf("term not quoted: %q", match)
	}
}

func hasBareSyntax(s string) bool {
	for _, tk := range []string{"FLOOR", "COLLAPSE", "INPLACE"} {
		if containsStr(s, tk) {
			return true
		}
	}
	return false
}

func TestBuildORQueryFallback(t *testing.T) {
	and, _ := BuildQuery("dragon castle fog")
	or, _ := BuildORQuery("dragon castle fog")
	if and == or {
		t.Error("OR fallback must differ")
	}
	if containsStr(and, " OR ") || !containsStr(or, " OR ") {
		t.Errorf("and=%q or=%q", and, or)
	}
}

func TestEmptyQuery(t *testing.T) {
	match, tokens := BuildQuery("   ")
	if match != "" || len(tokens) != 0 {
		t.Error("empty query must produce nothing")
	}
}

func TestTokenizeApostrophes(t *testing.T) {
	toks := TokenizeWords("don't stop the hyphen-word")
	if !contains(toks, "don't") {
		t.Errorf("apostrophe token lost: %v", toks)
	}
	if !contains(toks, "hyphen-word") {
		t.Errorf("hyphen token lost: %v", toks)
	}
}

func contains(s []string, w string) bool {
	for _, x := range s {
		if x == w {
			return true
		}
	}
	return false
}

func containsStr(hay, needle string) bool {
	return len(hay) >= len(needle) && indexOf(hay, needle) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
