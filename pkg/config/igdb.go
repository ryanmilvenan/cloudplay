package config

// IgdbConfig controls IGDB enrichment — the per-game metadata (genre,
// year, franchise, summary, cover art) that the search and agent
// phases rely on for retrieval and ranking.
//
// OAuth note: IGDB API v4 is gated behind Twitch OAuth2 client-
// credentials. ClientID is public-ish (sent on every API call) and
// ClientSecret is used once at startup to exchange for an access token
// (valid ~60 days; refreshed automatically). Both live in a
// `CredentialsFile` read at worker init; never in the repo.
type IgdbConfig struct {
	// Enabled gates the enricher entirely. When false the cache file is
	// untouched, no IGDB API calls fire, and GameInfo's enriched fields
	// stay empty. Defaults to false.
	Enabled bool

	// CredentialsFile is a path inside the worker container to an
	// env-file containing CLOUD_GAME_IGDB_CLIENT_ID= and
	// CLOUD_GAME_IGDB_CLIENT_SECRET= on separate lines. Typical path:
	// /secrets/igdb.env (bind-mounted from the host). If the file is
	// missing or parseable-but-empty, the enricher logs a warning and
	// stays off.
	CredentialsFile string `yaml:"credentialsFile"`

	// CachePath is the SQLite database path for enriched rows, on a
	// persistent volume so cache survives container rebuilds. Typical
	// path: /var/mnt/data/media/games/.metadata/igdb.db. The parent
	// directory is created if missing.
	CachePath string `yaml:"cachePath"`

	// RequestsPerSecond throttles the background backfill to respect
	// IGDB's published 4 req/s limit. Default 4. Bursts up to this rate
	// are fine; the enricher paces its queue.
	RequestsPerSecond int `yaml:"requestsPerSecond"`

	// MinConfidence (0..1) is the token-overlap threshold above which
	// an IGDB match is accepted. Below it the row is cached as
	// "unmatched" so we don't re-query IGDB forever. Default 0.6.
	MinConfidence float64 `yaml:"minConfidence"`
}
