package api

import "net/http"

// Identity is the verified end-user identity injected by the ingress auth
// layer (oauth2-proxy in front of pocket-id, or the chain-claude-test
// bypass for the Claude agent). It crosses the coordinator→worker
// boundary inside WebrtcInitRequest so the worker can tag GameSessions
// with who the user actually is — used for per-user save state keying,
// rendering profile avatars in player slots, etc.
//
// Sub is the only stable identifier; prefer it for keying durable
// state. Email/Username/Picture can change over time.
type Identity struct {
	Sub      string `json:"sub,omitempty"`
	Email    string `json:"email,omitempty"`
	Username string `json:"username,omitempty"`
	Picture  string `json:"picture,omitempty"`
	Groups   string `json:"groups,omitempty"`
}

// IsAnonymous reports whether the identity is unset (no auth headers
// arrived, or headers were all empty). Anonymous users still get a
// working session but no per-user features.
func (i Identity) IsAnonymous() bool { return i.Sub == "" }

// IdentityFromHeaders parses the X-Auth-Request-* header family that
// oauth2-proxy sets when SET_XAUTHREQUEST=true. The same headers are
// injected by the Traefik chain-claude-test middleware for the
// dev/agent bypass, so this single function handles both ingress
// modes identically.
//
// Headers are trusted unconditionally here — the security boundary
// is at Traefik ingress. Anyone speaking HTTP directly to the
// coordinator container (bypassing Traefik) could spoof these, so
// the coordinator must only be reachable via Traefik in production.
func IdentityFromHeaders(h http.Header) Identity {
	return Identity{
		Sub:      h.Get("X-Auth-Request-User"),
		Email:    h.Get("X-Auth-Request-Email"),
		Username: h.Get("X-Auth-Request-Preferred-Username"),
		Groups:   h.Get("X-Auth-Request-Groups"),
		// Picture is not a standard X-Auth-Request header; oauth2-proxy
		// doesn't set it. For Phase 1 we leave it blank and let the
		// client render a deterministic avatar from Sub. Later we can
		// parse the forwarded Authorization JWT here to pick up the
		// pocket-id `picture` claim.
	}
}
