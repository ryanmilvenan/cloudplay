// Package enricher wraps the IGDB API v4 (https://api.igdb.com/v4) for
// per-game metadata backfill: genre, franchise, year, summary, cover
// art. Used by pkg/games/library.go to hydrate GameMetadata and by
// Phase 3's semantic-search index as the embedding text source.
//
// Auth model: IGDB gates the API behind Twitch OAuth2 client-credentials
// (IGDB is a Twitch property). The client acquires a bearer token at
// first call and refreshes it on 401 or when it's within 5 minutes of
// expiry. Tokens normally live ~60 days, so one token per worker
// lifetime is the common case.
//
// Query model: IGDB uses "Apicalypse" (a compact text DSL). We issue
// POST /v4/games with a body like:
//
//	search "Halo Combat Evolved";
//	fields name,first_release_date,genres.name,franchises.name,summary,cover.url,platforms;
//	where platforms = (11); limit 5;
//
// The platform filter scopes to the system we're enriching for (Xbox=11,
// PS2=8, etc.) so a ROM named "NFL Blitz" doesn't cross-match the
// arcade version when we're looking for the N64 version.
package enricher

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	twitchTokenURL = "https://id.twitch.tv/oauth2/token"
	igdbGamesURL   = "https://api.igdb.com/v4/games"
	// Twitch OAuth tokens are long-lived; we refresh proactively when
	// within this window of expiry so a single in-flight request never
	// races the deadline.
	refreshLeadTime = 5 * time.Minute
)

// IgdbGame is the shape we request from IGDB (selected fields only).
// Unknown fields are ignored by json.Unmarshal so IGDB can add new
// attributes without breaking us.
type IgdbGame struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	FirstReleaseDate  int64  `json:"first_release_date"` // Unix seconds
	Summary           string `json:"summary"`
	Cover             *struct {
		URL string `json:"url"`
	} `json:"cover,omitempty"`
	Genres []struct {
		Name string `json:"name"`
	} `json:"genres,omitempty"`
	Franchises []struct {
		Name string `json:"name"`
	} `json:"franchises,omitempty"`
	Platforms []int `json:"platforms,omitempty"`
}

// Year returns the integer year from FirstReleaseDate, or 0 if unset.
func (g *IgdbGame) Year() int {
	if g.FirstReleaseDate <= 0 {
		return 0
	}
	return time.Unix(g.FirstReleaseDate, 0).UTC().Year()
}

// FirstGenre is a convenience — callers typically just want one.
func (g *IgdbGame) FirstGenre() string {
	if len(g.Genres) == 0 {
		return ""
	}
	return g.Genres[0].Name
}

// FirstFranchise is a convenience.
func (g *IgdbGame) FirstFranchise() string {
	if len(g.Franchises) == 0 {
		return ""
	}
	return g.Franchises[0].Name
}

// CoverURL returns the HTTPS cover URL with IGDB's "//images.igdb.com"
// protocol-relative prefix normalized, or "" when absent.
func (g *IgdbGame) CoverURL() string {
	if g.Cover == nil || g.Cover.URL == "" {
		return ""
	}
	u := g.Cover.URL
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	return u
}

// Client is the thin IGDB facade the enricher sits on top of.
type Client struct {
	clientID     string
	clientSecret string
	httpc        *http.Client

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

// NewClient reads client credentials from a simple env-file (one
// KEY=VALUE per line, '#' comments). The file is read once at
// construction; change it and restart the worker to rotate creds.
//
// Expected keys:
//
//	CLOUD_GAME_IGDB_CLIENT_ID
//	CLOUD_GAME_IGDB_CLIENT_SECRET
func NewClient(credsFile string) (*Client, error) {
	creds, err := readEnvFile(credsFile)
	if err != nil {
		return nil, fmt.Errorf("igdb: read creds %s: %w", credsFile, err)
	}
	id := strings.TrimSpace(creds["CLOUD_GAME_IGDB_CLIENT_ID"])
	secret := strings.TrimSpace(creds["CLOUD_GAME_IGDB_CLIENT_SECRET"])
	if id == "" || secret == "" {
		return nil, fmt.Errorf("igdb: %s missing CLOUD_GAME_IGDB_CLIENT_ID / CLIENT_SECRET", credsFile)
	}
	return &Client{
		clientID:     id,
		clientSecret: secret,
		httpc:        &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// SearchGame issues one Apicalypse query against /v4/games constrained
// to the given IGDB platform IDs (empty slice = no constraint).
// Returns at most `limit` hits in IGDB's relevance order.
func (c *Client) SearchGame(query string, platforms []int, limit int) ([]IgdbGame, error) {
	if limit <= 0 {
		limit = 5
	}
	token, err := c.ensureToken()
	if err != nil {
		return nil, err
	}
	body := buildApicalypse(query, platforms, limit)
	req, err := http.NewRequest(http.MethodPost, igdbGamesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Client-ID", c.clientID)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("igdb: search http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		// Token expired or revoked — clear and let the next call
		// re-auth. Surface the error so the backfill queue retries.
		c.mu.Lock()
		c.accessToken = ""
		c.mu.Unlock()
		return nil, fmt.Errorf("igdb: 401 unauthorized (token cleared)")
	}
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("igdb: search status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out []IgdbGame
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("igdb: decode: %w", err)
	}
	return out, nil
}

// ensureToken returns a valid access token, acquiring one on first call
// and refreshing whenever we're within refreshLeadTime of expiry.
// Serialized on c.mu so concurrent enricher workers don't each hit
// Twitch.
func (c *Client) ensureToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Until(c.tokenExpiry) > refreshLeadTime {
		return c.accessToken, nil
	}
	// client_credentials grant: POST form with our id+secret, get back
	// {access_token, expires_in (seconds), token_type}.
	form := strings.NewReader(fmt.Sprintf(
		"client_id=%s&client_secret=%s&grant_type=client_credentials",
		c.clientID, c.clientSecret))
	req, err := http.NewRequest(http.MethodPost, twitchTokenURL, form)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("igdb: token http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("igdb: token status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("igdb: token decode: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("igdb: empty access_token")
	}
	c.accessToken = tok.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

// buildApicalypse constructs the IGDB query DSL body. Escapes the
// double-quote inside the search clause because IGDB's parser breaks
// on unescaped " in search strings.
func buildApicalypse(query string, platforms []int, limit int) []byte {
	q := strings.ReplaceAll(query, `"`, `\"`)
	var b strings.Builder
	// Fields we want back. Cover uses dot-extension to pull the nested
	// URL; same for genres/franchises where we want .name.
	fmt.Fprintf(&b, `fields name,first_release_date,genres.name,franchises.name,summary,cover.url,platforms;`)
	fmt.Fprintf(&b, ` search "%s";`, q)
	if len(platforms) > 0 {
		var ids []string
		for _, p := range platforms {
			ids = append(ids, fmt.Sprintf("%d", p))
		}
		fmt.Fprintf(&b, ` where platforms = (%s);`, strings.Join(ids, ","))
	}
	fmt.Fprintf(&b, ` limit %d;`, limit)
	return []byte(b.String())
}

// readEnvFile is a dead-simple KEY=VALUE parser. Ignores blank lines
// and '#' comments. Quotes around values are trimmed.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		v = strings.Trim(v, `"'`)
		out[k] = v
	}
	return out, nil
}
