package config

// SemanticSearchConfig controls the vector-search path for the
// search-first UI. Disabled by default; the frontend gracefully falls
// back to fuzzy-only when the endpoint isn't reachable, so flipping
// Enabled off is always safe.
type SemanticSearchConfig struct {
	// Enabled gates the entire pipeline: embedding on library scan,
	// index construction, and the /v1/search/semantic HTTP endpoint.
	Enabled bool

	// EmbedURL is the OpenAI-compatible /v1/embeddings endpoint.
	// Default target: lightning's ai-openclaw-embeddings.service
	// (Qwen3-Embedding-0.6B via vLLM at port 8088, pooling runner,
	// 1024-dim output). The embedder runs at a tiny GPU footprint
	// (--gpu-memory-utilization 0.05) so it coexists with the coder
	// and concierge models without evicting them.
	EmbedURL string `yaml:"embedUrl"`

	// EmbedModel is the `model` field sent in the POST body. vLLM
	// uses the value passed to --served-model-name — for our unit
	// file that is "embeddings". Note: this is NOT the underlying
	// weight name ("qwen3-embedding-0.6b").
	EmbedModel string `yaml:"embedModel"`

	// BatchSize is how many games' text strings we pack into one
	// embedding request. vLLM's /v1/embeddings accepts `input: [...]`
	// natively; batching cuts 3 000 games from ~3 000 round-trips
	// down to ~100 and loads the GPU efficiently. 32 is a safe
	// default at 1024-dim x 8192-max-tokens.
	BatchSize int `yaml:"batchSize"`

	// RequestsPerSecond throttles batch requests. At batch=32 and
	// RPS=4 we embed ~128 games/sec — the full library finishes in
	// well under a minute. Keeps GPU pressure predictable when the
	// coder/concierge models are serving user traffic alongside.
	RequestsPerSecond int `yaml:"requestsPerSecond"`
}
