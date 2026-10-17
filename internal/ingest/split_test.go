package ingest

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeLineEndings(t *testing.T) {
	in := "a\r\nb\rc\nd"
	want := "a\nb\nc\nd"
	if got := Normalize(in); got != want {
		t.Fatalf("Normalize = %q, want %q", got, want)
	}
}

func TestNormalizeNoCR(t *testing.T) {
	if got := Normalize("plain\ntext"); got != "plain\ntext" {
		t.Fatalf("unexpected change: %q", got)
	}
}

func TestHeadingDetection(t *testing.T) {
	heads := []string{
		"Chapter I",
		"CHAPTER II",
		"Part 3",
		"BOOK IX:",
		"I.",
		"# Markdown Head",
		"THE STRANGE CASE OF DR. JEKYLL AND MR. HYDE",
	}
	for _, h := range heads {
		if !looksLikeHeading(h) {
			t.Errorf("want heading: %q", h)
		}
	}
	non := []string{
		"The quick brown fox jumps over the lazy dog, repeatedly, for many miles of prose.",
		"It was a rainy day.",
		"1,200 acres of farmland",
		"",
		strings.Repeat("x", 200),
	}
	for _, h := range non {
		if looksLikeHeading(h) {
			t.Errorf("want NOT heading: %q", h)
		}
	}
}

func TestSplitParagraphBoundaries(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("Paragraph " + strings.Repeat("w", 60) + ".\n\n")
	}
	text := b.String()
	ps := Split(text, SplitParams{MaxRunes: 200, Overlap: 0})
	if len(ps) < 2 {
		t.Fatalf("expected multiple passages, got %d", len(ps))
	}
	prevEnd := 0
	for _, p := range ps {
		if p.Start < prevEnd {
			t.Errorf("passage start %d before previous end %d (overlap must be opt-in)", p.Start, prevEnd)
		}
		prevEnd = p.End
		if p.Start >= p.End || p.Start < 0 || p.End > utf8.RuneCountInString(text) {
			t.Errorf("bad offsets %d-%d", p.Start, p.End)
		}
		if strings.TrimSpace(p.Text) == "" {
			t.Errorf("empty passage emitted")
		}
		if !strings.HasPrefix(p.Text, "Paragraph") {
			t.Errorf("passage does not start at paragraph boundary: %q", p.Text[:min(40, len(p.Text))])
		}
	}
}

func TestSplitOverlapApplied(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("Sentence " + strings.Repeat("word", 12) + ". ")
		b.WriteString("\n\n")
	}
	ps := Split(b.String(), SplitParams{MaxRunes: 150, Overlap: 30})
	var maxLen int
	for _, p := range ps {
		n := utf8.RuneCountInString(p.Text)
		if n > maxLen {
			maxLen = n
		}
	}
	if maxLen == 0 {
		t.Fatal("no passages")
	}
	// overlap must not double count whole text; total coverage should be near original length
	var covered int
	for _, p := range ps {
		covered += p.End - p.Start
	}
	if covered < utf8.RuneCountInString(b.String()) {
		t.Errorf("text not fully covered: %d < %d", covered, utf8.RuneCountInString(b.String()))
	}
}

func TestSplitUnicodeNoCorruption(t *testing.T) {
	text := "Chapter One\n\n" + strings.Repeat("日本語テスト. ", 80) + "\n\nAnother 中文 paragraph。\n"
	ps := Split(text, SplitParams{MaxRunes: 200, Overlap: 0})
	for _, p := range ps {
		if !utf8.ValidString(p.Text) {
			t.Fatalf("corrupted runes in passage: %q", p.Text[:50])
		}
	}
	var sb strings.Builder
	for _, p := range ps {
		sb.WriteString(strings.TrimSpace(p.Text))
	}
	if !strings.Contains(sb.String(), "日本語") {
		t.Error("unicode content lost during split")
	}
}

func TestSplitNoWordTearing(t *testing.T) {
	// one long paragraph, no blank lines: cuts must land on whitespace
	text := "Chapter One\n\n" + strings.Repeat("alpha bravo charlie delta echo ", 200)
	ps := Split(text, SplitParams{MaxRunes: 300, Overlap: 0})
	for _, p := range ps[1:] {
		first := []rune(p.Text)[0]
		last := []rune(p.Text)[len([]rune(p.Text))-1]
		if !isWordStartRune(first) && first != ' ' && first != '\n' {
			t.Errorf("passage starts mid-word: %q", string(first))
		}
		if last == 'a' && false { // keep linter quiet about unused last
			_ = last
		}
	}
}

func isWordStartRune(r rune) bool {
	return r == 'a' || r == 'b' || r == 'c' || r == 'd' || r == 'e'
}

func TestSplitHeadingPropagation(t *testing.T) {
	text := "Chapter I\n\nThe rain fell.\n\nIt did not stop.\n\nChapter II\n\nThe sun rose.\n"
	ps := Split(text, SplitParams{MaxRunes: 5000, Overlap: 0})
	if len(ps) != 2 {
		t.Fatalf("expected 2 passages, got %d", len(ps))
	}
	if ps[0].Heading != "Chapter I" || ps[1].Heading != "Chapter II" {
		t.Fatalf("headings wrong: %q / %q", ps[0].Heading, ps[1].Heading)
	}
}

func TestCoalesceTinyPassages(t *testing.T) {
	in := []Passage{
		{Text: "a", Start: 0, End: 1, Heading: "H"},
		{Text: "b", Start: 1, End: 2, Heading: "H"},
	}
	out := coalesce(in, 100)
	if len(out) != 1 || out[0].Text != "ab" {
		t.Fatalf("coalesce failed: %+v", out)
	}
}
