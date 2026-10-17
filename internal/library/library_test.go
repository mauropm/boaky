package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWalkIgnoresHiddenAndOtherExts(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "nested", ".cache"), 0o755)
	os.MkdirAll(filepath.Join(root, "nested", "sub"), 0o755)
	write := func(p string) { os.WriteFile(filepath.Join(root, p), []byte("text"), 0o644) }
	write("a.txt")
	write("nested/b.txt")
	write("nested/.hidden.txt")
	write("nested/.cache/c.txt")
	write("notes.md")
	write("nested/sub/d.txt")

	files, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.RelPath] = true
	}
	for _, want := range []string{"a.txt", "nested/b.txt", "nested/sub/d.txt"} {
		if !got[want] {
			t.Errorf("missing %s; got %v", want, got)
		}
	}
	for _, bad := range []string{"nested/.hidden.txt", "notes.md", "nested/.cache/c.txt"} {
		if got[bad] {
			t.Errorf("should be ignored: %s", bad)
		}
	}
	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d", len(files))
	}
}

func TestSafeJoinTraversal(t *testing.T) {
	root := t.TempDir()
	if _, err := SafeJoin(root, "../evil"); err == nil {
		t.Error("expected traversal to be rejected")
	}
	if _, err := SafeJoin(root, "/etc/passwd"); err == nil {
		t.Error("expected absolute escape to be rejected")
	}
	if _, err := SafeJoin(root, "a/../../evil"); err == nil {
		t.Error("expected embedded traversal to be rejected")
	}
	p, err := SafeJoin(root, "books/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, root) {
		t.Errorf("resolved outside root: %s", p)
	}
}

func TestHashFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "x.txt")
	os.WriteFile(p, []byte("hello"), 0o644)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sum, err := HashFile(f)
	if err != nil {
		t.Fatal(err)
	}
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if sum != want {
		t.Errorf("sha256 = %s", sum)
	}
	// second read must still work (seek reset)
	sum2, _ := HashFile(f)
	if sum2 != want {
		t.Error("second hash differs")
	}
}

func TestExtractMetadataGutenberg(t *testing.T) {
	text := `The Project Gutenberg eBook of Dracula

This eBook is for the use of anyone anywhere.

Title: Dracula

Author: Bram Stoker

*** START OF THE PROJECT GUTENBERG EBOOK DRACULA ***

A long book body goes here, and more, and more.

*** END OF THE PROJECT GUTENBERG EBOOK DRACULA ***

End license text here.
`
	m := ExtractMetadata(text)
	if !m.Gutenberg {
		t.Error("want gutenberg detected")
	}
	if m.Title != "Dracula" {
		t.Errorf("title = %q", m.Title)
	}
	if m.Author != "Bram Stoker" {
		t.Errorf("author = %q", m.Author)
	}
	if !strings.Contains(text[m.BodyStart:m.BodyEnd], "book body") {
		t.Error("body must contain book text")
	}
	if strings.Contains(text[m.BodyStart:m.BodyEnd], "END OF THE PROJECT") {
		t.Error("license trailer must be excluded")
	}
}

func TestExtractMetadataNoHeaders(t *testing.T) {
	m := ExtractMetadata("just some plain text\nwith lines\n")
	if m.Title != "" || m.Author != "" {
		t.Errorf("must not invent: %+v", m)
	}
	if FallbackTitle("pg1234.txt") != "pg1234" {
		t.Error("fallback title wrong")
	}
	if FallbackTitle("my_book.txt") != "my book" {
		t.Error("fallback title cleanup wrong")
	}
}
