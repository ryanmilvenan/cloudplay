package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/games"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/enricher"
	"github.com/giongto35/cloud-game/v3/pkg/worker/search"
)

// retrievalDeps is the minimum surface the handler needs from the
// worker — library lookup, IGDB cache lookup for per-candidate
// metadata, and the semantic index for vector retrieval. We accept
// interfaces so tests can swap them.
type retrievalDeps struct {
	Library  games.GameLibrary
	Cache    *enricher.Cache
	Index    *search.Index
	Embedder *search.Embedder
	TopK     int
}

// Handler serves POST /v1/agent/turn. Request:
//
//	{"query": "let's play halo", "history": [{"role":"user","text":"..."}, ...]}
//
// Response: one of the action shapes the LLM emits (see prompts.go).
// Always returns HTTP 200 with an action — transport errors / LLM
// failures are folded into {"action":"say","text":"..."} so the
// frontend never has to special-case network errors.
type Handler struct {
	ollama     *OllamaClient
	deps       retrievalDeps
	log        *logger.Logger
	embedCache *queryEmbedCache // bounded LRU, 32 entries
}

// NewHandler wires the pieces the agent needs. Any of the deps can
// be nil (degrades the retrieval quality correspondingly).
func NewHandler(ollama *OllamaClient, lib games.GameLibrary, cache *enricher.Cache, idx *search.Index, embedder *search.Embedder, topK int, log *logger.Logger) *Handler {
	if topK <= 0 {
		topK = 10
	}
	return &Handler{
		ollama: ollama,
		log:    log,
		deps: retrievalDeps{
			Library:  lib,
			Cache:    cache,
			Index:    idx,
			Embedder: embedder,
			TopK:     topK,
		},
		embedCache: newQueryEmbedCache(32),
	}
}

type turnRequest struct {
	Query   string        `json:"query"`
	History []HistoryTurn `json:"history"`
}

// Action is the wire shape the browser consumes. Kept as a fat union
// rather than a sum type because JSON from the LLM may omit fields
// and we echo it through unchanged.
type Action struct {
	Action     string      `json:"action"`
	Text       string      `json:"text,omitempty"`
	GamePath   string      `json:"game_path,omitempty"`
	System     string      `json:"system,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req turnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		writeAction(w, Action{Action: "say", Text: "Say a game name or tell me what you feel like playing."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	candidates := h.retrieve(ctx, req.Query)
	messages := BuildPrompt(req.Query, candidates, req.History)
	raw, err := h.ollama.Chat(ctx, messages)
	if err != nil {
		h.log.Warn().Err(err).Msg("[AGENT] chat failed")
		writeAction(w, Action{Action: "say", Text: "I'm having trouble reaching the assistant. Try picking from the list."})
		return
	}
	action, err := parseAction(raw)
	if err != nil {
		h.log.Warn().Err(err).Str("raw", firstN(raw, 200)).Msg("[AGENT] unparseable LLM output")
		writeAction(w, Action{Action: "say", Text: firstN(raw, 240)})
		return
	}
	// Validate: launch / recommend actions must reference real candidates.
	// Drops inventor mode — the model occasionally makes up a game_path.
	action = h.validate(action, candidates)
	writeAction(w, action)
}

// retrieve combines semantic hits (vector cosine) with fuzzy title
// hits from the library and deduplicates, returning up to TopK.
func (h *Handler) retrieve(ctx context.Context, query string) []Candidate {
	top := h.deps.TopK
	seen := map[string]int{}
	var out []Candidate

	// Semantic branch (may fail → empty slice; fuzzy still runs).
	// Cache recent query embeddings so repeat queries during an active
	// conversation ("FIFA", "halo", "mario") skip the embedder's
	// 50-150 ms round-trip.
	if h.deps.Index != nil && h.deps.Embedder != nil {
		var vec []float32
		if v, ok := h.embedCache.Get(query); ok {
			vec = v
		} else if vecs, err := h.deps.Embedder.Embed([]string{query}); err == nil && len(vecs) > 0 {
			vec = vecs[0]
			h.embedCache.Put(query, vec)
		}
		if len(vec) > 0 {
			hits := h.deps.Index.Top(vec, top)
			for i, hit := range hits {
				key := hit.GamePath + "|" + hit.System
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = i
				out = append(out, h.makeCandidate(len(out)+1, hit.GamePath, hit.System))
			}
		}
	}

	// Fuzzy branch: pull any library entry whose title contains any
	// lowercase query token. Quick and permissive — the LLM will
	// filter further downstream.
	if h.deps.Library != nil && len(out) < top {
		q := strings.ToLower(query)
		tokens := strings.Fields(q)
		for _, g := range h.deps.Library.GetAll() {
			key := g.Path + "|" + g.System
			if _, ok := seen[key]; ok {
				continue
			}
			name := strings.ToLower(g.Alias + " " + g.Name)
			match := false
			for _, t := range tokens {
				if len(t) >= 3 && strings.Contains(name, t) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			seen[key] = len(out)
			out = append(out, h.makeCandidateFromMeta(len(out)+1, g))
			if len(out) >= top {
				break
			}
		}
	}

	// Cap just in case.
	if len(out) > top {
		out = out[:top]
	}
	return out
}

func (h *Handler) makeCandidate(rank int, gamePath, system string) Candidate {
	c := Candidate{Rank: rank, GamePath: gamePath, System: system, Title: gamePath}
	if h.deps.Cache != nil {
		if row, _ := h.deps.Cache.Get(gamePath, system); row != nil && row.Matched {
			if row.Name != "" {
				c.Title = row.Name
			}
			c.Genre = row.Genre
			c.Year = row.Year
			c.Franchise = row.Franchise
		}
	}
	// Fallback: trim the system-prefixed path down to just the
	// filename stem so the LLM sees "Halo 2" not "xbox/Halo 2.iso".
	if c.Title == gamePath {
		base := gamePath
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		c.Title = trimRomExt(base)
	}
	return c
}

func (h *Handler) makeCandidateFromMeta(rank int, g games.GameMetadata) Candidate {
	return Candidate{
		Rank:      rank,
		GamePath:  g.Path,
		System:    g.System,
		Title:     firstNonEmpty(g.Alias, g.Name, g.Path),
		Genre:     g.Genre,
		Year:      g.Year,
		Franchise: g.Franchise,
	}
}

// validate ensures launch/recommend actions only reference candidates
// in the prompt. Falls back to a gentle ask if the model invented
// a path.
func (h *Handler) validate(a Action, cands []Candidate) Action {
	candSet := map[string]struct{}{}
	for _, c := range cands {
		candSet[c.GamePath+"|"+c.System] = struct{}{}
	}
	switch a.Action {
	case "launch":
		if _, ok := candSet[a.GamePath+"|"+a.System]; !ok {
			h.log.Warn().Str("path", a.GamePath).Str("system", a.System).
				Msg("[AGENT] model hallucinated launch target; downgrading to say")
			return Action{Action: "say", Text: "I couldn't find that in your library."}
		}
	case "recommend":
		// Drop any fabricated candidates from the list; if nothing's
		// left, downgrade to say.
		var kept []Candidate
		for _, c := range a.Candidates {
			if _, ok := candSet[c.GamePath+"|"+c.System]; ok {
				kept = append(kept, c)
			}
		}
		a.Candidates = kept
		if len(a.Candidates) == 0 {
			a.Action = "say"
		}
	}
	return a
}

// parseAction decodes the raw JSON blob from the LLM into our Action
// shape. Strips common cruft (markdown fences, leading prose) so we
// tolerate the occasional prelude.
func parseAction(raw string) (Action, error) {
	s := strings.TrimSpace(raw)
	// Strip ``` fences if present.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	// Extract the outermost JSON object if extra prose wraps it.
	if i := strings.IndexByte(s, '{'); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndexByte(s, '}'); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}
	var a Action
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return Action{}, fmt.Errorf("unmarshal: %w", err)
	}
	if a.Action == "" {
		return Action{}, fmt.Errorf("missing action field")
	}
	return a, nil
}

func writeAction(w http.ResponseWriter, a Action) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a)
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// trimRomExt strips the last ".<ext>" suffix so titles look clean.
func trimRomExt(s string) string {
	if i := strings.LastIndex(s, "."); i > 0 {
		return s[:i]
	}
	return s
}
