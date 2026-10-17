// Package mcp exposes Boaky's read-only library tools over MCP.
// Only five tools exist; none can modify the library or database.
package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauro/boaky/internal/answer"
	"github.com/mauro/boaky/internal/config"
	"github.com/mauro/boaky/internal/evidence"
	"github.com/mauro/boaky/internal/ingest"
	"github.com/mauro/boaky/internal/search"
)

// Deps bundles everything the server needs.
type Deps struct {
	DB     *sql.DB
	Cfg    config.Config
	Log    *slog.Logger
	Search *search.Service
	Pipe   *ingest.Pipeline
	Eval   *evidence.Service
	Answer *answer.Services
}

// Server implements the Boaky MCP tool surface.
type Server struct {
	deps   Deps
	sdkSrv *sdk.Server
}

// NewServer registers all five read-only tools.
func NewServer(d Deps) *Server {
	s := &Server{deps: d}
	srv := sdk.NewServer(&sdk.Implementation{Name: "boaky", Version: "1.0.0"}, &sdk.ServerOptions{
		KeepAlive: 30 * time.Second,
	})
	s.sdkSrv = srv

	sdk.AddTool(srv, &sdk.Tool{
		Name:        "list_books",
		Description: "List indexed books with metadata (title, author, path, status, passage count). Read-only.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in struct {
		Limit int `json:"limit,omitempty" jsonschema:"Max books to return (default 100)"`
	}) (*sdk.CallToolResult, any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 100
		}
		if limit > 500 {
			limit = 500
		}
		books, err := d.Pipe.ListBooks(ctx)
		if err != nil {
			return nil, nil, err
		}
		if len(books) > limit {
			books = books[:limit]
		}
		return nil, map[string]any{"books": books, "count": len(books)}, nil
	})

	sdk.AddTool(srv, &sdk.Tool{
		Name:        "search_books",
		Description: "Search indexed books for relevant passages using keyword/natural-language queries. Returns exact passage text with source locators and BM25 relevance scores. Scores indicate relevance only, not proof that a passage supports a claim.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in struct {
		Query  string `json:"query" jsonschema:"Keywords or a natural-language question"`
		Title  string `json:"title,omitempty" jsonschema:"Filter by book title substring"`
		Author string `json:"author,omitempty" jsonschema:"Filter by author substring"`
		Limit  int    `json:"limit,omitempty" jsonschema:"Max passages (default 10)"`
	}) (*sdk.CallToolResult, any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = d.Cfg.SearchLimit
		}
		hits, err := d.Search.Search(ctx, search.Options{Query: in.Query, Title: in.Title, Author: in.Author, Limit: limit})
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"query": in.Query, "results": hits, "count": len(hits)}, nil
	})

	sdk.AddTool(srv, &sdk.Tool{
		Name:        "get_passages",
		Description: "Fetch exact indexed passages by passage_id, with citation metadata (title, author, path, heading, character offsets).",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in struct {
		PassageIDs []string `json:"passage_ids" jsonschema:"Passage IDs like 'bk-ab12cd34ef56:3'"`
	}) (*sdk.CallToolResult, any, error) {
		hits, err := d.Search.GetPassages(ctx, in.PassageIDs)
		if err != nil {
			return nil, nil, err
		}
		var missing []string
		found := map[string]bool{}
		for _, h := range hits {
			found[h.PassageID] = true
		}
		for _, id := range in.PassageIDs {
			if !found[id] {
				missing = append(missing, id)
			}
		}
		out := map[string]any{"passages": hits, "count": len(hits)}
		if len(missing) > 0 {
			out["missing"] = missing
		}
		return nil, out, nil
	})

	sdk.AddTool(srv, &sdk.Tool{
		Name:        "get_book_metadata",
		Description: "Get metadata for one book by its stable book ID.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in struct {
		BookID string `json:"book_id" jsonschema:"Stable book ID, e.g. bk-ab12cd34ef56"`
	}) (*sdk.CallToolResult, any, error) {
		detail, err := d.Pipe.Inspect(ctx, in.BookID)
		if err != nil {
			return nil, nil, err
		}
		return nil, detail, nil
	})

	sdk.AddTool(srv, &sdk.Tool{
		Name:        "check_evidence",
		Description: "Evaluate whether the local index contains sufficient evidence for a question. Optionally pass candidate passage IDs to verify them. Returns level: sufficient | partial | insufficient. Insufficient means Boaky must abstain; do not answer from general knowledge.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in struct {
		Question   string   `json:"question" jsonschema:"Question to evaluate against the local index"`
		PassageIDs []string `json:"passage_ids,omitempty" jsonschema:"Candidate passage IDs to verify (optional)"`
	}) (*sdk.CallToolResult, any, error) {
		a, err := d.Eval.Assess(ctx, in.Question, in.PassageIDs)
		if err != nil {
			return nil, nil, err
		}
		a.Hits = nil // keep the response small; passage text comes from search_books
		return nil, a, nil
	})

	return s
}

// SDKServer exposes the underlying server (for tests and transports).
func (s *Server) SDKServer() *sdk.Server { return s.sdkSrv }

// Connect attaches an arbitrary transport (used by tests).
func (s *Server) Connect(ctx context.Context, t sdk.Transport) (*sdk.ServerSession, error) {
	return s.sdkSrv.Connect(ctx, t, nil)
}

// ServeStdio connects the server over stdin/stdout (Hermes stdio transport).
func (s *Server) ServeStdio(ctx context.Context) error {
	conn, err := s.sdkSrv.Connect(ctx, &sdk.StdioTransport{}, nil)
	if err != nil {
		return err
	}
	s.deps.Log.Info("mcp stdio session started")
	err = conn.Wait()
	s.deps.Log.Info("mcp stdio session closed", "err", err)
	return err
}

// HTTPHandler returns a streamable-HTTP MCP handler plus a health endpoint.
func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s.sdkSrv }, &sdk.StreamableHTTPOptions{
		Stateless: false,
		Logger:    nil,
	})
	mux.Handle(s.deps.Cfg.MCPPath, handler)
	mux.HandleFunc(s.deps.Cfg.HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		var n int
		s.deps.DB.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&n)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "books": n})
	})
	return mux
}
