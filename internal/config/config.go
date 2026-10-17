// Package config loads Boaky settings from environment variables with
// conservative defaults. No secrets are stored here; API keys stay in the
// process environment and are never logged.
package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// Config holds every tunable knob in one place.
type Config struct {
	// BooksDir is the root of the read-only TXT library.
	BooksDir string
	// DBPath is the SQLite database file.
	DBPath string
	// MetaDir holds per-book JSON metadata written during indexing.
	MetaDir string

	// PassageMaxRunes is the target maximum passage length in runes.
	PassageMaxRunes int
	// PassageOverlapRunes is dropped from the start of the next passage
	// so a sentence split across a boundary is not lost.
	PassageOverlapRunes int
	// IndexBatchSize is the number of passages inserted per prepared batch
	// inside a single indexing transaction.
	IndexBatchSize int

	// SearchLimit is the default number of passages returned.
	SearchLimit int
	// MaxHitsPerBook keeps one book from flooding the result list.
	MaxHitsPerBook int
	// MaxContextChars caps the total runes of retrieved text returned to a
	// caller (and eventually to the model).
	MaxContextChars int

	// DBPoolSize bounds SQLite connections. Writers serialize on SQLite
	// anyway; 1 writer + a few readers is plenty for one process.
	DBPoolSize int

	// Evidence thresholds (lexical signals, not semantic proof).
	EvidenceMinScore            float64
	EvidenceMinCoverage         float64
	EvidencePartialCover        float64
	EvidenceMinResults          int
	EvidenceMinPassagesForScore int

	// MaxFileBytes refuses to ingest absurdly large files.
	MaxFileBytes int64

	// HTTPAddr is the bind address for `boaky serve-http`.
	HTTPAddr string
	// MCPPath is where the streamable HTTP MCP endpoint is mounted.
	MCPPath string
	// HealthPath is the liveness endpoint.
	HealthPath string

	// LogLevel is debug|info|warn|error.
	LogLevel string

	// Generation (only used by `boaky answer`; unset means abstain-only).
	GenBaseURL string
	GenAPIKey  string
	GenModel   string
	GenTimeout int // seconds
}

// Default returns the shipped defaults, resolved against dir ("" = cwd).
func Default(dir string) Config {
	c := Config{
		BooksDir:                    filepath.Join(dir, "books"),
		DBPath:                      filepath.Join(dir, "data", "index", "boaky.db"),
		MetaDir:                     filepath.Join(dir, "data", "metadata"),
		PassageMaxRunes:             2500,
		PassageOverlapRunes:         250,
		IndexBatchSize:              500,
		SearchLimit:                 10,
		MaxHitsPerBook:              3,
		MaxContextChars:             12000,
		DBPoolSize:                  4,
		EvidenceMinScore:            2.5,
		EvidenceMinCoverage:         0.60,
		EvidencePartialCover:        0.30,
		EvidenceMinResults:          1,
		EvidenceMinPassagesForScore: 50,
		MaxFileBytes:                64 << 20,
		HTTPAddr:                    "127.0.0.1:8000",
		MCPPath:                     "/mcp",
		HealthPath:                  "/healthz",
		LogLevel:                    "info",
		GenModel:                    "deepseek-chat",
		GenTimeout:                  60,
	}
	return c
}

// FromEnv applies BOAKY_* overrides on top of the defaults for dir.
func FromEnv(dir string) Config {
	c := Default(dir)
	envStr("BOAKY_BOOKS_DIR", &c.BooksDir)
	envStr("BOAKY_DB_PATH", &c.DBPath)
	envStr("BOAKY_META_DIR", &c.MetaDir)
	envInt("BOAKY_PASSAGE_MAX_RUNES", &c.PassageMaxRunes)
	envInt("BOAKY_PASSAGE_OVERLAP_RUNES", &c.PassageOverlapRunes)
	envInt("BOAKY_INDEX_BATCH_SIZE", &c.IndexBatchSize)
	envInt("BOAKY_SEARCH_LIMIT", &c.SearchLimit)
	envInt("BOAKY_MAX_HITS_PER_BOOK", &c.MaxHitsPerBook)
	envInt("BOAKY_MAX_CONTEXT_CHARS", &c.MaxContextChars)
	envInt("BOAKY_DB_POOL_SIZE", &c.DBPoolSize)
	envFloat("BOAKY_EVIDENCE_MIN_SCORE", &c.EvidenceMinScore)
	envFloat("BOAKY_EVIDENCE_MIN_COVERAGE", &c.EvidenceMinCoverage)
	envFloat("BOAKY_EVIDENCE_PARTIAL_COVERAGE", &c.EvidencePartialCover)
	envInt("BOAKY_EVIDENCE_MIN_RESULTS", &c.EvidenceMinResults)
	envInt("BOAKY_EVIDENCE_MIN_PASSAGES_FOR_SCORE", &c.EvidenceMinPassagesForScore)
	envInt64("BOAKY_MAX_FILE_BYTES", &c.MaxFileBytes)
	envStr("BOAKY_HTTP_ADDR", &c.HTTPAddr)
	envStr("BOAKY_LOG_LEVEL", &c.LogLevel)
	envStr("BOAKY_GEN_BASE_URL", &c.GenBaseURL)
	c.GenAPIKey = os.Getenv("BOAKY_GEN_API_KEY") // never logged
	envStr("BOAKY_GEN_MODEL", &c.GenModel)
	envInt("BOAKY_GEN_TIMEOUT", &c.GenTimeout)
	return c
}

func envStr(k string, dst *string) {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		*dst = v
	}
}

func envInt(k string, dst *int) {
	if v, ok := os.LookupEnv(k); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func envInt64(k string, dst *int64) {
	if v, ok := os.LookupEnv(k); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			*dst = n
		}
	}
}

func envFloat(k string, dst *float64) {
	if v, ok := os.LookupEnv(k); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			*dst = f
		}
	}
}
