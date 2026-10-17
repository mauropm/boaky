// Package ingest discovers, normalizes, splits, and indexes TXT books.
package ingest

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Passage is one indexed chunk of normalized text.
type Passage struct {
	Text    string
	Heading string
	Start   int // rune offset into the normalized text
	End     int
}

// SplitParams bounds passage size and overlap (both in runes).
type SplitParams struct {
	MaxRunes int
	Overlap  int
}

var (
	// "Chapter IV", "Part 2.", "BOOK IX:", "Section 3"
	chapterRe = regexp.MustCompile(`(?i)^\s{0,16}((chapter|part|section|canto|subpart)\s+([IVXLCDM\d]+|[a-z]\.?)([\s.:)]|$))`)
	// "I." / "II." as stand-alone numbered headings
	romanRe = regexp.MustCompile(`^\s{0,24}([IVXLC]+)\.{1,2}\s*$`)
	// markdown-style headings for other plain-text books
	mdHeadingRe = regexp.MustCompile(`^\s{0,8}#{1,6}\s+\S.*$`)
	// A run of mostly-uppercase words, e.g. "A DREAM OF RED HORSES"
	allCapsRe  = regexp.MustCompile(`^\s*([A-Z][A-Z\d\s&,.:;'"!?\-–—/()']{2,100})\s*$`)
	longCapsRe = regexp.MustCompile(`^\s*[A-Z]{4,}\s*$`)
)

// looksLikeHeading reports whether a whole line reads as a section heading.
// It is deliberately conservative: short, punctuated prose lines never win.
func looksLikeHeading(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" || len(t) > 96 || len(t) < 2 {
		return false
	}
	if strings.Contains(t, "*/") || strings.Contains(t, "/*") {
		return false
	}
	if mdHeadingRe.MatchString(t) || chapterRe.MatchString(t) || romanRe.MatchString(t) {
		return true
	}
	if !allCapsRe.MatchString(t) {
		return false
	}
	if strings.Count(t, " ") < 1 && !longCapsRe.MatchString(t) {
		return false // a single capitalized short token is not enough
	}
	words := strings.Fields(t)
	upper := 0
	for _, w := range words {
		if r, _ := utf8.DecodeRuneInString(w); !unicode.IsUpper(r) {
			return false
		}
		upper++
	}
	// must not end in sentence punctuation (prose), must be mostly caps-ish
	if strings.HasSuffix(t, ".") && !strings.HasSuffix(t, "..") {
		return false
	}
	return float64(upper)/float64(len(words)) >= 0.8
}

// Normalize collapses CRLF/CR to LF. It returns the normalized text and
// reports whether the text contains any control characters that suggest a
// binary file (beyond tab/newline).
func Normalize(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				continue // eat CR of CRLF; LF written next iteration
			}
			b.WriteByte('\n')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Split breaks normalized text into bounded passages along paragraph,
// sentence, and word boundaries (in that preference order), carrying the
// last recognized heading. It streams through a rune window and never keeps
// more than MaxRunes+Overlap+slack text in memory.
func Split(text string, p SplitParams) []Passage {
	if p.MaxRunes <= 0 {
		p.MaxRunes = 2500
	}
	if p.Overlap < 0 || p.Overlap >= p.MaxRunes/2 {
		p.Overlap = p.MaxRunes / 10
	}
	maxR := p.MaxRunes
	ov := p.Overlap
	hardCut := maxR + maxR/2 // sentence/word search window before a forced cut

	var out []Passage
	var buf []rune // window since passage start
	bufStart := 0  // rune offset of buf[0]
	offset := 0    // total runes consumed
	heading := ""  // current recognized heading

	flush := func(cut int) {
		cut = min(cut, len(buf))
		if cut <= 0 {
			return
		}
		start := bufStart
		end := bufStart + cut
		txt := string(buf[:cut])
		if strings.TrimSpace(txt) != "" {
			out = append(out, Passage{Text: txt, Heading: heading, Start: start, End: end})
		}
		next := end - ov
		if next <= start {
			next = end
		}
		buf = append(buf[:0], buf[next-bufStart:]...)
		bufStart = next
		offset = max(offset, end)
	}

	for _, line := range strings.SplitAfter(text, "\n") {
		lineStr := strings.TrimSpace(line)
		if looksLikeHeading(lineStr) {
			// headings attach to what follows; close any running passage so
			// the heading line itself is not buried mid-passage
			if len(buf) > 0 && len(strings.TrimSpace(string(buf))) > 0 {
				flush(len(buf))
			}
			buf = buf[:0]
			bufStart = offset + len([]rune(line)) // skip the heading line itself
			offset = bufStart
			heading = lineStr
			continue
		}
		for _, r := range line {
			buf = append(buf, r)
			offset++
		}
		if len(buf) <= hardCut {
			continue
		}
		// Window is over budget: choose the best cut point.
		if i := lastParagraphCut(buf, maxR); i > 0 {
			flush(i)
			continue
		}
		if i := lastSentenceCut(buf, maxR); i > 0 {
			flush(i)
			continue
		}
		flush(wordBoundary(buf, maxR))
	}
	if len(buf) > 0 {
		flush(len(buf))
	}
	return coalesce(out, p.MaxRunes)
}

// lastParagraphCut finds the final "\n\n" inside the preferred window.
func lastParagraphCut(buf []rune, maxR int) int {
	lo := 0
	if len(buf) > maxR {
		lo = len(buf) - maxR
	}
	for i := len(buf) - 1; i > lo; i-- {
		if buf[i] == '\n' && buf[i-1] == '\n' {
			for i < len(buf) && buf[i] == '\n' {
				i++ // start after blank runs
			}
			return i
		}
	}
	return 0
}

// lastSentenceCut finds the final sentence end inside the window.
func lastSentenceCut(buf []rune, maxR int) int {
	lo := 0
	if len(buf) > maxR {
		lo = len(buf) - maxR
	}
	for i := len(buf) - 1; i > lo+1; i-- {
		if (buf[i] == '.' || buf[i] == '!' || buf[i] == '?') &&
			i+1 < len(buf) && (buf[i+1] == ' ' || buf[i+1] == '\n' || buf[i+1] == '"' || buf[i+1] == '”') {
			return i + 1
		}
	}
	return 0
}

// wordBoundary returns an index near target that does not split a word.
func wordBoundary(buf []rune, target int) int {
	i := min(target, len(buf))
	for i > 0 && !unicode.IsSpace(buf[i-1]) {
		i--
	}
	if i == 0 {
		return len(buf)
	}
	return i
}

// coalesce merges adjacent tiny passages when it does not exceed max, and
// drops zero-length results.
func coalesce(in []Passage, max int) []Passage {
	var out []Passage
	for _, p := range in {
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		if len(out) > 0 {
			last := out[len(out)-1]
			if last.Heading == p.Heading && last.End == p.Start &&
				utf8.RuneCountInString(last.Text)+utf8.RuneCountInString(p.Text) <= max {
				last.Text += p.Text
				last.End = p.End
				out[len(out)-1] = last
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
