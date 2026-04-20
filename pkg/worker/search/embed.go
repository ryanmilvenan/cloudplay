// Package search is the vector-search path for cloudplay's search-first
// game UI (Phase 3 of the search-and-AI rework). Three moving parts:
//
//   - Embedder (embed.go, this file) — talks to an OpenAI-compatible
//     /v1/embeddings endpoint (vLLM at spark:8088 running
//     Qwen3-Embedding-0.6B in pooling mode). 1024-dim float vectors.
//     Batched: one HTTP request covers up to BatchSize inputs.
//
//   - Index (index.go) — in-memory flat vector store on top of the
//     SQLite embeddings table. Exact cosine similarity against every
//     row. For libraries in the low thousands this outperforms any
//     ANN data structure and the code stays trivial.
//
//   - HTTP handler (handler.go) — POST /v1/search/semantic?q=…&top=…
//     on the worker's existing httpx server.
//
// The index is populated by the IGDB enricher: once a game has a
// confident IGDB match (genre/year/summary in hand), the enricher
// embeds "<system_label> <title> <franchise> <genre> <year> <summary>"
// and writes the vector to the cache. Restart-safe; cache survives
// worker rebuilds.
package search

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedder wraps a vLLM /v1/embeddings endpoint. Stateless beyond a
// tuned http client; safe for concurrent Embed calls.
type Embedder struct {
	URL    string
	Model  string
	httpc  *http.Client
}

// NewEmbedder builds an Embedder ready to call. URL must be the full
// endpoint including /v1/embeddings; Model matches the --served-model-name
// the vLLM process was started with ("embeddings" for our unit file).
func NewEmbedder(url, model string) *Embedder {
	return &Embedder{
		URL:   strings.TrimRight(url, "/"),
		Model: model,
		httpc: &http.Client{Timeout: 30 * time.Second},
	}
}

// Embed issues one request for up to len(inputs) strings and returns
// a parallel slice of float32 vectors. vLLM's OpenAI-compatible API
// preserves input order in the response; the returned slice is aligned
// 1:1 to inputs. An empty inputs slice is a no-op and returns nil.
//
// The JSON float64 payload is converted to float32 at parse time —
// Qwen3-Embedding-0.6B outputs native f32, vLLM stringifies at f64
// precision, and f32 halves the in-memory footprint of the library
// index (3 000 x 1 024 x 4 B = 12 MB vs. 24 MB at f64).
func (e *Embedder) Embed(inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{
		"model": e.Model,
		"input": inputs,
	})
	if err != nil {
		return nil, fmt.Errorf("search: marshal embed request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: embed http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("search: embed status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	// OpenAI shape: {"object":"list","data":[{"embedding":[...],"index":0}, ...]}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("search: embed decode: %w", err)
	}
	out := make([][]float32, len(inputs))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(inputs) {
			continue // defensive: vLLM violating its own ordering contract
		}
		v := make([]float32, len(d.Embedding))
		for i, f := range d.Embedding {
			v[i] = float32(f)
		}
		out[d.Index] = v
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("search: missing embedding for input %d (batch hole)", i)
		}
	}
	return out, nil
}
