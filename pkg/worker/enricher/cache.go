package enricher

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
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

-- Phase-3 semantic search index. Sibling table so a single open file
-- hydrates both genre metadata and embeddings. Stored as a raw BLOB
-- of little-endian float32s — matches Go's encoding/binary and is
-- half the disk footprint of a JSON-stringified vector. text_hash is
-- sha256 of the embedded text so a future change to the embedding-
-- source format (e.g. including summary) invalidates old rows.
CREATE TABLE IF NOT EXISTS embeddings (
    game_path   TEXT NOT NULL,
    system      TEXT NOT NULL,
    dim         INTEGER NOT NULL,
    text_hash   TEXT NOT NULL,
    vector      BLOB NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (game_path, system)
);
`

// EmbeddingRow is one persisted vector. Vector length must equal Dim.
type EmbeddingRow struct {
	GamePath  string
	System    string
	Dim       int
	TextHash  string
	Vector    []float32
	UpdatedAt time.Time
}

// GetEmbedding returns the cached vector for (path, system), or nil
// if absent. Callers typically check the returned row's TextHash
// against a freshly-computed hash to decide whether to re-embed.
func (c *Cache) GetEmbedding(path, system string) (*EmbeddingRow, error) {
	row := c.db.QueryRow(`
		SELECT dim, text_hash, vector, updated_at
		FROM embeddings WHERE game_path = ? AND system = ?`, path, system)
	r := &EmbeddingRow{GamePath: path, System: system}
	var blob []byte
	var updated int64
	if err := row.Scan(&r.Dim, &r.TextHash, &blob, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if len(blob) != r.Dim*4 {
		return nil, fmt.Errorf("cache: embedding blob length %d != dim*4 (%d)", len(blob), r.Dim*4)
	}
	r.Vector = bytesToFloat32(blob)
	r.UpdatedAt = time.Unix(updated, 0)
	return r, nil
}

// PutEmbedding upserts a vector row.
func (c *Cache) PutEmbedding(r EmbeddingRow) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	updated := r.UpdatedAt.Unix()
	if updated == 0 {
		updated = time.Now().Unix()
	}
	blob := float32sToBytes(r.Vector)
	_, err := c.db.Exec(`
		INSERT INTO embeddings (game_path, system, dim, text_hash, vector, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(game_path, system) DO UPDATE SET
		  dim=excluded.dim,
		  text_hash=excluded.text_hash,
		  vector=excluded.vector,
		  updated_at=excluded.updated_at;
	`, r.GamePath, r.System, r.Dim, r.TextHash, blob, updated)
	return err
}

// AllEmbeddings streams every embedding row. Used at worker startup
// to seed the in-memory vector index.
func (c *Cache) AllEmbeddings() ([]EmbeddingRow, error) {
	rows, err := c.db.Query(`SELECT game_path, system, dim, text_hash, vector, updated_at FROM embeddings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmbeddingRow
	for rows.Next() {
		var r EmbeddingRow
		var blob []byte
		var updated int64
		if err := rows.Scan(&r.GamePath, &r.System, &r.Dim, &r.TextHash, &blob, &updated); err != nil {
			return nil, err
		}
		if len(blob) != r.Dim*4 {
			continue // corrupt row, skip
		}
		r.Vector = bytesToFloat32(blob)
		r.UpdatedAt = time.Unix(updated, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func float32sToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func bytesToFloat32(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

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

// AllMatchedNeedingEmbedding returns every IGDB-matched row whose
// (game_path, system) has no corresponding embeddings row. Used by
// the enricher's startup backfill to cover IGDB rows that predate
// Phase-3 embedding support.
func (c *Cache) AllMatchedNeedingEmbedding() ([]Row, error) {
	rows, err := c.db.Query(`
		SELECT i.game_path, i.system, i.matched, i.igdb_id, i.name, i.genre,
		       i.franchise, i.year, i.summary, i.cover_url, i.updated_at
		FROM igdb_cache i
		LEFT JOIN embeddings e
		  ON e.game_path = i.game_path AND e.system = i.system
		WHERE i.matched = 1 AND e.game_path IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var matched int
		var updated int64
		if err := rows.Scan(&r.GamePath, &r.System, &matched, &r.IgdbID, &r.Name,
			&r.Genre, &r.Franchise, &r.Year, &r.Summary, &r.CoverURL, &updated); err != nil {
			return nil, err
		}
		r.Matched = matched != 0
		r.UpdatedAt = time.Unix(updated, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Close releases the underlying database.
func (c *Cache) Close() error { return c.db.Close() }
