// Package agent is the conversational game-select agent (Phase 4). It
// sits above the Phase-3 retrieval layer and asks a local Ollama model
// (gemma4:e4b by default) to turn a short conversation into exactly one
// structured action:
//
//   ask        — need more info from the user
//   recommend  — here are options, pick one
//   launch     — I'm sure, open this game
//   say        — nothing actionable; just respond
//
// The HTTP surface is one endpoint, POST /v1/agent/turn, served on the
// worker and reverse-proxied by the coordinator so the browser's same-
// origin fetch Just Works.
package agent

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

// OllamaClient is a thin wrapper over Ollama's /api/chat. Stateless,
// safe for concurrent calls.
type OllamaClient struct {
	URL     string
	Model   string
	httpc   *http.Client
	timeout time.Duration
}

// NewOllamaClient is the constructor. URL should NOT include the /api
// path; we append it per request. Timeout <= 0 falls back to 8 s.
func NewOllamaClient(url, model string, timeout time.Duration) *OllamaClient {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	return &OllamaClient{
		URL:     strings.TrimRight(url, "/"),
		Model:   model,
		httpc:   &http.Client{Timeout: timeout},
		timeout: timeout,
	}
}

// ChatMessage mirrors Ollama's /api/chat message shape.
type ChatMessage struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// Chat issues one /api/chat call with format=json and stream=false.
// Returns the raw assistant content string — the caller parses the
// JSON the model produced.
func (c *OllamaClient) Chat(ctx context.Context, messages []ChatMessage) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":    c.Model,
		"messages": messages,
		"format":   "json",
		"stream":   false,
	})
	if err != nil {
		return "", err
	}
	endpoint := c.URL + "/api/chat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("agent: chat http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("agent: chat status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	// Ollama /api/chat (stream=false) returns a single JSON object with
	// .message.content holding the assistant's reply.
	var parsed struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Done bool `json:"done"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("agent: chat decode: %w", err)
	}
	return parsed.Message.Content, nil
}
