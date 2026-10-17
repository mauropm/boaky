package library

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Metadata holds bibliographic info for a book. Empty strings mean unknown;
// Boaky never invents values.
type Metadata struct {
	Title  string
	Author string
	// BodyOffset is the byte-ish rune offset where the actual book text
	// begins (after the Project Gutenberg header, when one is detected).
	BodyStart int
	// BodyEnd is where the book text ends (before the Gutenberg license
	// trailer, when detected). 0 means "to end of file".
	BodyEnd int
	// Gutenberg indicates a Project Gutenberg structured header was found.
	Gutenberg bool
}

const (
	pgStartMarker = "START OF THE PROJECT GUTENBERG EBOOK"
	pgEndMarker   = "END OF THE PROJECT GUTENBERG EBOOK"
)

// ExtractMetadata reads only the head and tail of a normalized text and
// returns conservative metadata. It recognizes Project Gutenberg headers
// (`Title:`, `Author:`) and locates the body between the START/END markers,
// so license boilerplate is not indexed. Everything else falls back to the
// filename for a provisional title and an unknown author.
func ExtractMetadata(text string) Metadata {
	var m Metadata
	limit := len(text)
	if limit > 16384 {
		limit = 16384
	}
	head := text[:limit]
	startLines := 0
	if i := strings.Index(head, pgStartMarker); i >= 0 {
		m.Gutenberg = true
		// body starts at the end of the marker line
		nl := strings.IndexByte(head[i:], '\n')
		if nl >= 0 {
			startLines = i + nl + 1
		}
	}
	if e := strings.Index(text[max(0, len(text)-65536):], pgEndMarker); e >= 0 {
		idx := max(0, len(text)-65536) + e
		nl := strings.LastIndexByte(text[:idx], '\n')
		if nl > startLines {
			m.BodyEnd = nl
		}
	}
	m.BodyStart = startLines
	if m.BodyEnd == 0 {
		m.BodyEnd = len(text)
	}
	if m.BodyEnd < m.BodyStart {
		m.BodyStart, m.BodyEnd = 0, len(text)
	}
	if m.Gutenberg {
		m.Title, m.Author = parsePGHeader(text[:min(len(text), startLines+16384)])
		if m.Title == "" && strings.Contains(head, pgStartMarker) {
			m.Title = titleFromMarker(head)
		}
		return m
	}
	return m
}

// parsePGHeader extracts Title/Author from a Gutenberg-style header block.
func parsePGHeader(header string) (title, author string) {
	for _, line := range strings.Split(header, "\n") {
		t := strings.TrimSpace(line)
		if v, ok := cutFold(t, "Title:"); ok {
			if v != "" {
				title = v
			}
		} else if v, ok := cutFold(t, "Author:"); ok {
			if v != "" {
				author = firstAuthor(v)
			}
		}
		if title != "" && author != "" {
			break
		}
	}
	return title, author
}

// firstAuthor normalizes "A, B, and C" or "A and B" author lines by joining
// them conservatively as written; we do not attempt to pick one or fabricate.
func firstAuthor(v string) string {
	v = strings.TrimSpace(strings.TrimSuffix(v, "."))
	return v
}

// titleFromMarker handles "The Project Gutenberg eBook of TITLE" openings
// and "*** START OF THE PROJECT GUTENBERG EBOOK TITLE ***" markers.
func titleFromMarker(head string) string {
	for _, line := range strings.Split(head, "\n") {
		t := strings.TrimSpace(line)
		if v, ok := cutFold(t, "The Project Gutenberg eBook of"); ok && v != "" {
			return strings.TrimSpace(strings.TrimSuffix(v, "***"))
		}
		if i := strings.Index(t, pgStartMarker); i >= 0 {
			rest := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t[i+len(pgStartMarker):]), "***"))
			if rest != "" {
				return rest
			}
		}
	}
	return ""
}

// FallbackTitle derives a provisional title from a relative path:
// "books/pg345.txt" -> "pg345". No linguistic invention, just cleanup.
func FallbackTitle(relPath string) string {
	base := filepath.Base(relPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = strings.NewReplacer("_", " ", "-", " ").Replace(base)
	base = strings.TrimSpace(base)
	if base == "" {
		return relPath
	}
	return base
}

// IsGutenberg returns true if the head looks like a Project Gutenberg file.
func IsGutenberg(r io.Reader) bool {
	br := bufio.NewReader(io.LimitReader(r, 4096))
	s, _ := br.ReadString(0)
	return strings.Contains(s, "Project Gutenberg")
}

func cutFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) {
		return "", false
	}
	if strings.EqualFold(s[:len(prefix)], prefix) {
		return strings.TrimSpace(s[len(prefix):]), true
	}
	return "", false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// OpenFile opens a book file for reading.
func OpenFile(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%q: %w", path, ErrNotRegular)
	}
	return f, nil
}
