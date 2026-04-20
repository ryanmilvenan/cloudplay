package enricher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/games"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/search"
)

// Enricher wraps Client+Cache and drives a rate-limited background
// backfill against the current game library. On library scan the worker
// calls Prime(games) to upsert rows for any title the cache already
// knows about (zero I/O, just reads SQLite), then Enqueue for the rest.
// A single goroutine drains the queue against IGDB at the configured
// RPS and writes matched / unmatched rows back to the cache.
//
// Callers that need fresh metadata after a backfill chunk can subscribe
// to OnBatchComplete — we fire it after every N items processed so the
// worker can re-broadcast LibNewGameList to connected clients.
type Enricher struct {
	cli           *Client
	cache         *Cache
	log           *logger.Logger
	rps           int
	minConfidence float64

	// Optional semantic-search plumbing. When Embedder+Index are set
	// the enricher also embeds each successfully-matched game and
	// seeds the in-memory vector index. Phase 3's search handler
	// reads the index directly.
	embedder *search.Embedder
	index    *search.Index

	mu    sync.Mutex
	queue []games.GameMetadata // games awaiting lookup
	known map[string]bool      // path+system → already cached

	// OnBatchComplete fires after every batchSize queue items drain so
	// the worker can refresh the library broadcast. Optional.
	OnBatchComplete func()
}

// New constructs an Enricher ready to accept games. The caller must
// call Run(ctx) separately to start the background loop; New() does
// no network I/O.
func New(cli *Client, cache *Cache, rps int, minConfidence float64, log *logger.Logger) *Enricher {
	if rps <= 0 {
		rps = 4 // IGDB's published ceiling
	}
	if minConfidence <= 0 {
		minConfidence = 0.6
	}
	return &Enricher{
		cli:           cli,
		cache:         cache,
		log:           log,
		rps:           rps,
		minConfidence: minConfidence,
		known:         make(map[string]bool),
	}
}

// AttachSemanticSearch wires in the vLLM embedder and the in-memory
// vector index. Called by the worker when Search.Enabled is true.
// Nil-safe: if either arg is nil the enricher stays in plain-IGDB mode.
func (e *Enricher) AttachSemanticSearch(embedder *search.Embedder, index *search.Index) {
	if e == nil {
		return
	}
	e.embedder = embedder
	e.index = index
}

// CacheHandle exposes the underlying Cache so other worker packages
// (e.g. pkg/worker/agent) can look up enriched per-game metadata to
// hydrate LLM prompts. Returns nil when the enricher isn't configured.
func (e *Enricher) CacheHandle() *Cache {
	if e == nil {
		return nil
	}
	return e.cache
}

// LoadEmbeddingsFromCache streams every persisted embedding into the
// in-memory index. Called once at worker startup after AttachSemanticSearch
// so restarts don't re-embed the library.
func (e *Enricher) LoadEmbeddingsFromCache() (int, error) {
	if e == nil || e.index == nil || e.cache == nil {
		return 0, nil
	}
	rows, err := e.cache.AllEmbeddings()
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		e.index.Upsert(search.Entry{
			GamePath: r.GamePath,
			System:   r.System,
			Vector:   r.Vector,
		})
	}
	return len(rows), nil
}

// BackfillEmbeddingsFromIGDB walks every IGDB-matched row that
// doesn't yet have an embedding, embeds its metadata string, and
// populates both cache and in-memory index. Covers the case where
// the IGDB cache was populated before Phase-3 landed: ApplyCached
// marks those games as "already known" so the enricher's regular
// Enqueue → processOne path skips them, and their embedding
// therefore never gets written on its own.
//
// Runs in a goroutine; the caller can ignore the return. Rate-limited
// by e.rps just like the main backfill so we don't burst the embedder.
func (e *Enricher) BackfillEmbeddingsFromIGDB(ctx context.Context) {
	if e == nil || e.embedder == nil || e.index == nil || e.cache == nil {
		return
	}
	rows, err := e.cache.AllMatchedNeedingEmbedding()
	if err != nil {
		e.log.Warn().Err(err).Msg("[EMBED] backfill query failed")
		return
	}
	if len(rows) == 0 {
		return
	}
	e.log.Info().Int("count", len(rows)).
		Msg("[EMBED] backfilling embeddings for matched IGDB rows")
	interval := time.Second / time.Duration(e.rps)
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	done := 0
	for _, r := range rows {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Build the same embedding text processOne would have built.
		g := games.GameMetadata{Path: r.GamePath, System: r.System, Name: r.Name}
		e.embedAndIndex(g, r)
		done++
		if done%50 == 0 {
			e.log.Info().Int("done", done).Int("total", len(rows)).
				Msg("[EMBED] backfill progress")
		}
	}
	e.log.Info().Int("done", done).Msg("[EMBED] backfill complete")
}

// ApplyCached hydrates a GameMetadata in place with cached fields when
// the cache has a matched row for this (path, system). No-op when
// there's no cache hit or the row is unmatched. Never does network I/O.
// Used by library.Scan to surface prior enrichment in the very first
// LibNewGameList broadcast.
func (e *Enricher) ApplyCached(g *games.GameMetadata) bool {
	if e == nil || e.cache == nil || g == nil {
		return false
	}
	row, err := e.cache.Get(g.Path, g.System)
	if err != nil || row == nil {
		return false
	}
	e.mu.Lock()
	e.known[g.Path+"|"+g.System] = true
	e.mu.Unlock()
	if !row.Matched {
		return false
	}
	g.Genre = row.Genre
	g.Franchise = row.Franchise
	g.Year = row.Year
	g.Summary = row.Summary
	g.CoverURL = row.CoverURL
	return true
}

// Enqueue appends a game to the backfill queue if it isn't already
// cached. Idempotent — calling twice on the same game during one
// worker lifetime only queues once.
func (e *Enricher) Enqueue(g games.GameMetadata) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := g.Path + "|" + g.System
	if e.known[key] {
		return
	}
	e.known[key] = true
	e.queue = append(e.queue, g)
}

// Run drains the queue at ~e.rps lookups per second until ctx is
// cancelled. Safe to call once per enricher.
func (e *Enricher) Run(ctx context.Context) {
	if e == nil || e.cli == nil {
		return
	}
	interval := time.Second / time.Duration(e.rps)
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	processed := 0
	const batchSize = 16
	const statSize = 100

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g, ok := e.pop()
			if !ok {
				continue
			}
			e.processOne(g)
			processed++
			if processed%batchSize == 0 && e.OnBatchComplete != nil {
				e.OnBatchComplete()
			}
			if processed%statSize == 0 {
				if total, matched, err := e.cache.Stats(); err == nil {
					e.log.Info().
						Int("processed_this_run", processed).
						Int("queue_remaining", e.QueueDepth()).
						Int("cache_total", total).
						Int("cache_matched", matched).
						Msg("[IGDB] backfill progress")
				}
			}
		}
	}
}

// QueueDepth returns the number of games still pending lookup. Useful
// for logging and for integration tests that want to wait for drain.
func (e *Enricher) QueueDepth() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.queue)
}

// pop removes and returns the front of the queue, or (zero, false) if empty.
func (e *Enricher) pop() (games.GameMetadata, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.queue) == 0 {
		return games.GameMetadata{}, false
	}
	g := e.queue[0]
	e.queue = e.queue[1:]
	return g, true
}

// processOne runs one IGDB search for a game and writes the best match
// (or an unmatched marker) to the cache. Errors are logged but don't
// abort the enricher — the game just stays absent from the cache so a
// later run can retry.
func (e *Enricher) processOne(g games.GameMetadata) {
	queryText := NormalizeTitle(g.Name)
	platformIDs := platformsForSystem(g.System)
	hits, err := e.cli.SearchGame(queryText, platformIDs, 5)
	if err != nil {
		e.log.Warn().Err(err).Str("game", g.Name).Msg("[IGDB] search failed")
		return
	}
	best, score := bestMatch(queryText, hits)
	row := Row{GamePath: g.Path, System: g.System, UpdatedAt: time.Now()}
	if best == nil || score < e.minConfidence {
		row.Matched = false
		if best != nil {
			// Still record the name/IgdbID so we can debug misses.
			row.IgdbID = best.ID
			row.Name = best.Name
		}
		if err := e.cache.Put(row); err != nil {
			e.log.Warn().Err(err).Msg("[IGDB] cache put (unmatched)")
		}
		e.log.Debug().Str("game", g.Name).Float64("score", score).
			Msg("[IGDB] no confident match — cached as unmatched")
		return
	}
	row.Matched = true
	row.IgdbID = best.ID
	row.Name = best.Name
	row.Genre = best.FirstGenre()
	row.Franchise = best.FirstFranchise()
	row.Year = best.Year()
	row.Summary = best.Summary
	row.CoverURL = best.CoverURL()
	if err := e.cache.Put(row); err != nil {
		e.log.Warn().Err(err).Msg("[IGDB] cache put")
		return
	}
	e.log.Info().Str("game", g.Name).Str("igdb", best.Name).
		Int("year", row.Year).Str("genre", row.Genre).
		Float64("score", score).Msg("[IGDB] enriched")

	// Phase-3 semantic search: embed and index. Done inline so a single
	// game's backfill either fully completes (IGDB + embedding cached
	// and indexed) or both fail and retry next restart. The embedder
	// call adds ~100-200 ms per game; at 4 req/s this is well within
	// our overall pacing budget.
	e.embedAndIndex(g, row)
}

// embedAndIndex is the Phase-3 hook wired into processOne's success
// path. Builds the canonical embedding text from the IGDB-enriched
// row, checks whether the cache already has an up-to-date vector (by
// text-hash), and calls out to the embedder only when needed.
func (e *Enricher) embedAndIndex(g games.GameMetadata, row Row) {
	if e.embedder == nil || e.index == nil || e.cache == nil {
		return
	}
	text := buildEmbeddingText(g, row)
	if text == "" {
		return
	}
	hash := sha256Hex(text)
	if existing, _ := e.cache.GetEmbedding(g.Path, g.System); existing != nil && existing.TextHash == hash {
		// Up to date. Nothing to do; the index was already seeded from
		// cache at startup.
		return
	}
	vecs, err := e.embedder.Embed([]string{text})
	if err != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
		if err != nil {
			e.log.Warn().Err(err).Str("game", g.Name).Msg("[EMBED] failed")
		}
		return
	}
	v := vecs[0]
	search.NormalizeInPlace(v)
	if err := e.cache.PutEmbedding(EmbeddingRow{
		GamePath: g.Path,
		System:   g.System,
		Dim:      len(v),
		TextHash: hash,
		Vector:   v,
		UpdatedAt: time.Now(),
	}); err != nil {
		e.log.Warn().Err(err).Msg("[EMBED] cache put")
		return
	}
	e.index.Upsert(search.Entry{GamePath: g.Path, System: g.System, Vector: v})
}

// buildEmbeddingText is the single source of truth for the string the
// embedder sees per game. Ordered so title and system (the most-
// discriminating tokens) come first, then the IGDB context (genre,
// franchise, year, summary). Empty fields are skipped to avoid
// diluting the signal with placeholder text.
func buildEmbeddingText(g games.GameMetadata, row Row) string {
	parts := []string{}
	if label := systemLabel(g.System); label != "" {
		parts = append(parts, label)
	}
	title := g.Alias
	if title == "" {
		title = g.Name
	}
	if title != "" {
		parts = append(parts, title)
	}
	if row.Franchise != "" && !strings.EqualFold(row.Franchise, title) {
		parts = append(parts, row.Franchise)
	}
	if row.Genre != "" {
		parts = append(parts, row.Genre)
	}
	if row.Year > 0 {
		parts = append(parts, fmt.Sprintf("%d", row.Year))
	}
	if row.Summary != "" {
		parts = append(parts, row.Summary)
	}
	return strings.Join(parts, ". ")
}

// systemLabel humanizes a system slug so the embedder sees "PlayStation 2"
// rather than "ps2" — critical for queries like "soccer game on ps2"
// where the user types the slug but the embedding compare against
// summary/description text that uses the full platform name.
func systemLabel(sys string) string {
	switch sys {
	case "xbox":
		return "Xbox"
	case "ps2":
		return "PlayStation 2"
	case "pcsx":
		return "PlayStation"
	case "gc":
		return "GameCube"
	case "wii":
		return "Wii"
	case "n64":
		return "Nintendo 64"
	case "snes":
		return "Super Nintendo"
	case "nes":
		return "NES"
	case "gba":
		return "Game Boy Advance"
	case "dreamcast":
		return "Dreamcast"
	case "mame":
		return "Arcade"
	case "dos":
		return "DOS"
	}
	return sys
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// -- normalization & matching ------------------------------------------------

// wrapRe strips "[!]", "(USA)", "(Rev N)", "(Disc N)", and similar
// bracketed tags from ROM filenames before IGDB lookup.
var wrapRe = regexp.MustCompile(`\s*(?:\[[^\]]*\]|\([^)]*\))`)
// trailingExtRe catches ROM "inner" extensions that the library-level
// stripper left behind (it only removes the final .ext). Examples:
// `NHL Hitz 2003.nkit`, `Halo.xiso`, `Mario.wia`. Case-insensitive.
var trailingExtRe = regexp.MustCompile(`(?i)\.(nkit|xiso|wia|gcz|rvz|wbfs|chd|cso|gdi|cdi|7z)$`)
// acronymDotRe collapses dotted acronyms like S.W.A.R.M. → SWARM BEFORE
// the generic punct→space pass; otherwise we'd end up with single-letter
// tokens that IGDB's search scorer ranks poorly.
var acronymDotRe = regexp.MustCompile(`(?i)\b([a-z])(?:\.([a-z]))+\.?\b`)
var punctRe = regexp.MustCompile(`[^a-z0-9 ]+`)
var spaceRe = regexp.MustCompile(`\s+`)

// romans is the I..X → 1..10 fold we apply after lowercasing. Handles
// Halo II, Final Fantasy IV, etc. matching IGDB's "<name> <number>"
// canonical listings.
var romans = map[string]string{
	"i": "1", "ii": "2", "iii": "3", "iv": "4", "v": "5",
	"vi": "6", "vii": "7", "viii": "8", "ix": "9", "x": "10",
}

// NormalizeTitle turns "Halo - Combat Evolved (USA) (Rev 2) [!].xiso"
// into "halo combat evolved" — the string we hand to IGDB's fuzzy
// search and compare results against.
func NormalizeTitle(s string) string {
	s = wrapRe.ReplaceAllString(s, " ")
	// Trim inner ROM-format extensions before stripping punctuation so
	// "NHL Hitz 2003.nkit" → "NHL Hitz 2003" (then "nhl hitz 2003"),
	// not "nhl hitz 2003 nkit".
	s = trailingExtRe.ReplaceAllString(s, "")
	s = strings.ToLower(s)
	// Collapse dotted acronyms: "s.w.a.r.m." → "swarm".
	s = acronymDotRe.ReplaceAllStringFunc(s, func(m string) string {
		return strings.ReplaceAll(strings.TrimSuffix(m, "."), ".", "")
	})
	s = punctRe.ReplaceAllString(s, " ")
	s = spaceRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Roman-numeral fold per token.
	parts := strings.Split(s, " ")
	for i, p := range parts {
		if r, ok := romans[p]; ok {
			parts[i] = r
		}
	}
	return strings.Join(parts, " ")
}

// bestMatch picks the IGDB result whose normalized name best overlaps
// our query (Jaccard similarity over tokens). Returns (nil, 0) when
// hits is empty.
func bestMatch(query string, hits []IgdbGame) (*IgdbGame, float64) {
	if len(hits) == 0 {
		return nil, 0
	}
	qTokens := tokenSet(query)
	type scored struct {
		g     *IgdbGame
		score float64
	}
	var all []scored
	for i := range hits {
		hTokens := tokenSet(NormalizeTitle(hits[i].Name))
		all = append(all, scored{g: &hits[i], score: jaccard(qTokens, hTokens)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	return all[0].g, all[0].score
}

func tokenSet(s string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, t := range strings.Split(s, " ") {
		if t == "" {
			continue
		}
		out[t] = struct{}{}
	}
	return out
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// -- system → IGDB platform IDs ----------------------------------------------

// IGDB platform IDs are stable; see https://api-docs.igdb.com/#platform.
// The subset we care about, keyed by cloudplay's system slug. Unknown
// slugs return nil so we don't over-constrain an unrecognized system
// and miss every result.
var systemPlatforms = map[string][]int{
	"xbox":      {11},
	"ps2":       {8},
	"gc":        {21},
	"wii":       {5},
	"n64":       {4},
	"snes":      {19},
	"nes":       {18},
	"gba":       {24},
	"dreamcast": {23},
	"pcsx":      {7}, // PlayStation 1
	"mame":      {52},
	"dos":       {13},
}

func platformsForSystem(sys string) []int {
	return systemPlatforms[sys]
}
