// Package library discovers TXT books under a configured root, computes
// content hashes, and extracts bibliographic metadata without inventing any.
package library

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotRegular is returned when a discovered entry is not a regular file
// (e.g. a symlink that escaped the root, a device, or a FIFO).
var ErrNotRegular = errors.New("not a regular file")

// File describes one discovered book file.
type File struct {
	RelPath string // forward-slash separated, relative to root
	AbsPath string
	Size    int64
}

// Walk walks root recursively and returns every non-hidden .txt file whose
// real path stays inside root. Hidden files and directories (dot-prefixed)
// are ignored, as are other extensions and symlinked directories.
func Walk(root string) ([]File, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(rootAbs); err != nil {
		return nil, fmt.Errorf("books dir: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("books dir %q is not a directory", rootAbs)
	}
	var files []File
	err = filepath.WalkDir(rootAbs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subdirectory: skip, reported via count later if desired
		}
		name := d.Name()
		if path != rootAbs && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(name), ".txt") {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			// Resolve and verify containment.
			rp, err := filepath.EvalSymlinks(path)
			if err != nil || !isUnder(rootAbs, rp) {
				return nil
			}
		}
		rel, err := filepath.Rel(rootAbs, path)
		if err != nil {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		files = append(files, File{
			RelPath: filepath.ToSlash(rel),
			AbsPath: path,
			Size:    fi.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortFiles(files)
	return files, nil
}

func sortFiles(fs []File) {
	for i := range fs {
		for j := i + 1; j < len(fs); j++ {
			if fs[j].RelPath < fs[i].RelPath {
				fs[i], fs[j] = fs[j], fs[i]
			}
		}
	}
}

// isUnder reports whether path resolves to somewhere under root. When a
// component does not exist yet (e.g. SafeJoin checking a destination), it
// falls back to lexical comparison of cleaned paths — never accepting a "..".
func isUnder(root, path string) bool {
	rp, err := filepath.EvalSymlinks(path)
	if err != nil {
		// path may not exist yet (SafeJoin checking a target): lexical-only
		// check against the unresolved root, which is sound because both
		// sides came from Clean() and ".." cannot survive Clean.
		p, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
		return err == nil && p != ".." && !strings.HasPrefix(p, ".."+string(filepath.Separator))
	}
	rk, err := filepath.EvalSymlinks(root)
	if err != nil {
		rk = filepath.Clean(root)
	}
	if rp == rk {
		return true
	}
	p, err := filepath.Rel(rk, rp)
	if err != nil {
		return false
	}
	return p != ".." && !strings.HasPrefix(p, ".."+string(filepath.Separator))
}

// SafeJoin joins user-supplied relative components under root, refusing paths
// that escape root.
func SafeJoin(root, rel string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return rootAbs, nil
	}
	if strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", fmt.Errorf("path %q escapes the books directory", rel)
	}
	cleanRel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+rel)), "/")
	if cleanRel == ".." || strings.HasPrefix(cleanRel, "../") {
		return "", fmt.Errorf("path %q escapes the books directory", rel)
	}
	candidate := filepath.Join(rootAbs, filepath.FromSlash(cleanRel))
	if !isUnder(rootAbs, candidate) && candidate != rootAbs {
		return "", fmt.Errorf("path %q escapes the books directory", rel)
	}
	return candidate, nil
}

// HashFile streams f and returns its sha256 as lowercase hex.
func HashFile(f *os.File) (string, error) {
	h := sha256.New()
	buf := make([]byte, 256*1024)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashBytes returns the sha256 hex of b.
func HashBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
