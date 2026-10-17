// Package generate is a minimal OpenAI-compatible chat-completions client.
// It is used only by `boaky answer`; normal operation (MCP/CLI search) never
// calls it. Boaky itself performs no inference and never ships an LLM.
package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one OpenAI-compatible endpoint (e.g. OpenCode Go via
// Hermes's configured provider, for CLI-side answer testing only).
type Client struct {
	BaseURL string // e.g. https://opencode.ai/zen/go/v1
	APIKey  string
	Model   string
	Timeout time.Duration
	HTTP    *http.Client
}

// Enabled reports whether generation can be used.
func (c *Client) Enabled() bool {
	return c != nil && c.BaseURL != "" && c.APIKey != "" && c.Model != ""
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Chat sends messages and returns the assistant reply text.
func (c *Client) Chat(ctx context.Context, messages []string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("generation not configured")
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: c.Timeout}
	}
	body, _ := json.Marshal(chatRequest{
		Model:       c.Model,
		Temperature: 0,
		Messages:    toMsgs(messages),
	})
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("generation request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("generation response is not valid JSON (HTTP %d)", resp.StatusCode)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("generation error (HTTP %d): %s", resp.StatusCode, cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("generation returned no choices (HTTP %d)", resp.StatusCode)
	}
	return cr.Choices[0].Message.Content, nil
}

func toMsgs(s []string) []chatMessage {
	msgs := make([]chatMessage, len(s))
	for i, m := range s {
		msgs[i] = chatMessage{Role: "user", Content: m}
	}
	msgs[0].Role = "system"
	return msgs
}
