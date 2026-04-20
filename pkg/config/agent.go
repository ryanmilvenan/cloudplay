package config

// AgentConfig controls the Phase-4 conversational agent. The agent
// receives natural-language queries through the frontend's search-bar
// Enter path, runs retrieval against the Phase-3 index, then calls
// an Ollama model to pick one of four actions (ask / recommend /
// launch / say). Disabled by default; when off, the frontend stays on
// its Phase-1 stub message.
//
// The agent is strictly additive over Phase 3 — if OllamaURL is
// unreachable or the model name is wrong, the handler returns a
// gentle "say" fallback rather than 5xx so the UI keeps working.
type AgentConfig struct {
	// Enabled gates both the /v1/agent/turn HTTP handler and the
	// frontend's use of it (the browser probes once and falls back
	// to the stub if the endpoint isn't there).
	Enabled bool

	// OllamaURL is the Ollama HTTP root (no path). Default:
	// http://localhost:11434 — same host as the worker in the
	// single-container cloudplay deploy.
	OllamaURL string `yaml:"ollamaUrl"`

	// Model is the tag passed to /api/chat. Must be pre-pulled on
	// the Ollama host. Default: "gemma4:e4b" per operator direction.
	Model string `yaml:"model"`

	// TimeoutMs caps the per-turn LLM call so a slow response can't
	// hang the UI. Default 8 000 ms; larger prompts + cold-start
	// generation occasionally hit 3-5 s on gemma4:e4b, so 8 s gives
	// comfortable headroom without frustrating a waiting user.
	TimeoutMs int `yaml:"timeoutMs"`

	// TopK is how many retrieval candidates we inject into the
	// prompt. Higher values give the LLM more to pick from but eat
	// prompt tokens; the Phase-3 retrieval already orders by score
	// so the first N are a strong sample. Default 10.
	TopK int `yaml:"topK"`
}
