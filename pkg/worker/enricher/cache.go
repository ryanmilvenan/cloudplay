package enricher

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGo)
)

// Cache persists enrichment results under a SQLite file on the NAS so
// backfill is idempotent across worker restarts. Key = (game_path,
// system); row shape mirrors GameMetadata's enriched fields plus a
// `matched` flag that distinguishes "IGDB had nothing for us" from "we
// just haven't tried yet".
//
// The schema is tiny and the row count caps at ~library size; cosine
// over the table for Phase 3 reads it all into memory. No indexes
// beyond the PK.
type Cache struct {
	db *sql.DB
	mu sync.Mutex // serialize writers; modernc.org/sqlite is goroutine-safe on reads
}

// Row is one cached enrichment. Matched=true means IGDB returned a
// result we accepted; false means we queried and nothing cleared the
// MinConfidence bar. Either way we won't re-query unless the source
// mtime changes.
type Row struct {
	GamePath  string
	System    string
	Matched   bool
	IgdbID    int64
	Name      string
	Genre     string
	Franchise string
	Year      int
	Summary   string
	CoverURL  string
	UpdatedAt time.Time
}

// NewCache opens (or creates) the SQLite file at path, ensuring the
// parent directory exists and the schema is applied. Safe to call from
// worker startup.
func NewCache(path string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("igdb cache: mkdir %s: %w", filepath.Dir(path), err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("igdb cache: open %s: %w", path, err)
	}
	// modernc.org/sqlite defaults to WAL if we ask — good concurrency
	// posture given our NFS-mount target.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		return nil, fmt.Errorf("igdb cache: enable WAL: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("igdb cache: schema: %w", err)
	}
	return &Cache{db: db}, nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS igdb_cache (
    game_path   TEXT NOT NULL,
    system      TEXT NOT NULL,
    matched     INTEGER NOT NULL,
    igdb_id     INTEGER NOT NULL DEFAULT 0,
    name        TEXT NOT NULL DEFAULT '',
    genre       TEXT NOT NULL DEFAULT '',
    franchise   TEXT NOT NULL DEFAULT '',
    year        INTEGER NOT NULL DEFAULT 0,
    summary     TEXT NOT NULL DEFAULT '',
    cover_url   TEXT NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (game_path, system)
);
`

// Get returns the cached row for (path, system), or nil if absent.
func (c *Cache) Get(path, system string) (*Row, error) {
	row := c.db.QueryRow(`
		SELECT matched, igdb_id, name, genre, franchise, year, summary, cover_url, updated_at
		FROM igdb_cache WHERE game_path = ? AND system = ?`, path, system)
	r := &Row{GamePath: path, System: system}
	var matched int
	var updated int64
	if err := row.Scan(&matched, &r.IgdbID, &r.Name, &r.Genre, &r.Franchise,
		&r.Year, &r.Summary, &r.CoverURL, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	r.Matched = matched != 0
	r.UpdatedAt = time.Unix(updated, 0)
	return r, nil
}

// Put upserts a row. Both matched and unmatched rows go here — the
// "unmatched" form (Matched=false, empty fields) is what stops us from
// re-querying IGDB forever on ROMs that truly have no listing.
func (c *Cache) Put(r Row) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	matched := 0
	if r.Matched {
		matched = 1
	}
	updated := r.UpdatedAt.Unix()
	if updated == 0 {
		updated = time.Now().Unix()
	}
	_, err := c.db.Exec(`
		INSERT INTO igdb_cache (game_path, system, matched, igdb_id, name, genre, franchise, year, summary, cover_url, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(game_path, system) DO UPDATE SET
		  matched=excluded.matched,
		  igdb_id=excluded.igdb_id,
		  name=excluded.name,
		  genre=excluded.genre,
		  franchise=excluded.franchise,
		  year=excluded.year,
		  summary=excluded.summary,
		  cover_url=excluded.cover_url,
		  updated_at=excluded.updated_at;
	`, r.GamePath, r.System, matched, r.IgdbID, r.Name, r.Genre, r.Franchise,
		r.Year, r.Summary, r.CoverURL, updated)
	return err
}

// Stats returns counts useful for logging / ops visibility.
func (c *Cache) Stats() (total, matched int, err error) {
	err = c.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(matched),0) FROM igdb_cache`).Scan(&total, &matched)
	return
}

// Close releases the underlying database.
func (c *Cache) Close() error { return c.db.Close() }
