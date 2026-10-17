package citations

import (
	"strings"
	"testing"

	"github.com/mauro/boaky/internal/search"
)

func TestNormalizeQuote(t *testing.T) {
	q := `  the   quick
	brown   fox  `
	if got := NormalizeQuote(q); got != "the quick brown fox" {
		t.Errorf("got %q", got)
	}
}

func TestFromHit(t *testing.T) {
	long := "some long text that will be truncated for the excerpt beyond limits for sure here"
	c := FromHit(search.Hit{
		PassageID: "bk-01:2", BookID: "bk-01", Title: "T", Author: "A",
		RelPath: "b.txt", Heading: "H", CharStart: 10, CharEnd: 20, Text: long,
	})
	if c.Excerpt == "" || len([]rune(c.Excerpt)) > 161 {
		t.Errorf("bad excerpt %q", c.Excerpt)
	}
	if !strings.Contains(c.Excerpt, "some long text") {
		t.Errorf("excerpt content wrong: %q", c.Excerpt)
	}
	if c.PassageID != "bk-01:2" || c.CharStart != 10 {
		t.Errorf("mapping wrong: %+v", c)
	}
}
