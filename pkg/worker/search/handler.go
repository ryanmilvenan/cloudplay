package search

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// Handler is the POST /v1/search/semantic endpoint wired onto the
// worker's existing httpx Mux. Body: JSON {q: string, top: int}.
// Response: JSON {hits: [{game_path, system, score}, ...]}.
//
// No auth; the worker's HTTP surface is already exposed on the same
// origin as the WebRTC/ping endpoints and these are all best-effort
// helpers. If we later gate worker HTTP the gate applies uniformly.
type Handler struct {
	embedder *Embedder
	index    *Index
	log      *logger.Logger
}

// NewHandler wraps an embedder + index into an http.Handler.
// Safe for concurrent requests; the index uses an RWMutex internally
// and the embedder's http client is already goroutine-safe.
func NewHandler(embedder *Embedder, index *Index, log *logger.Logger) *Handler {
	return &Handler{embedder: embedder, index: index, log: log}
}

type searchRequest struct {
	Q   string `json:"q"`
	Top int    `json:"top"`
}

type searchResponse struct {
	Hits []Hit `json:"hits"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req searchRequest
	// Accept both JSON body and `?q=` for quick curl probes.
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		req.Q = r.URL.Query().Get("q")
		if t := r.URL.Query().Get("top"); t != "" {
			n, _ := strconv.Atoi(t)
			req.Top = n
		}
	}
	req.Q = strings.TrimSpace(req.Q)
	if req.Top <= 0 || req.Top > 100 {
		req.Top = 10
	}
	if req.Q == "" {
		writeJSON(w, searchResponse{Hits: []Hit{}})
		return
	}
	vecs, err := h.embedder.Embed([]string{req.Q})
	if err != nil || len(vecs) == 0 {
		if err != nil {
			h.log.Warn().Err(err).Str("q", req.Q).Msg("[SEARCH] embed failed")
		}
		// Degrade gracefully: the frontend will have its own fuzzy
		// fallback, so we return an empty list instead of 500.
		writeJSON(w, searchResponse{Hits: []Hit{}})
		return
	}
	hits := h.index.Top(vecs[0], req.Top)
	writeJSON(w, searchResponse{Hits: hits})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
