// Package romcache rehydrates compressed ROM archives in place: when a
// .7z-backed game is launched, extract it alongside its source archive
// on the NAS and remove the original .7z. Subsequent launches (and the
// next library scan) see a regular ISO.
//
// Flow (Resolve):
//
//  1. Path that isn't a supported archive → return as-is.
//  2. Path IS an archive → acquire a per-archive lock.
//  3. If the extracted file already exists next to the archive, return it.
//  4. Otherwise run `7z x` into a sibling temp dir, atomic-rename the
//     payload to its final name, delete the source archive.
//
// Design choices:
//
//   - Cache lives in the same dir as the archive (on the NAS) so storage
//     is cheap and we don't keep two copies (original + extracted).
//     Archive removal is the confirmation that extraction succeeded.
//   - No LRU or eviction — the NAS has room for every extracted ROM
//     forever. If space becomes tight, operators re-archive manually.
//   - The temp dir is on the same filesystem so the rename is atomic;
//     a crash mid-extract leaves the original .7z untouched.
//   - If an archive contains multiple files (bin+cue, etc.) we pick the
//     largest, which is right for ISOs. Cue/gdi-awareness is future work.
package romcache

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// Hydrator turns archive paths into extracted paths. Thread-safe; safe
// to share a single instance across the worker.
type Hydrator struct {
	// Log receives structured progress.
	Log *logger.Logger

	keyLocks sync.Map // source-path → *sync.Mutex
}

// Resolve returns a filesystem path the emulator can read directly. For
// supported archives it extracts, removes the original, and returns the
// extracted path. For anything else it returns path unchanged.
func (h *Hydrator) Resolve(path string) (string, error) {
	if !isArchive(path) {
		return path, nil
	}
	mu := h.lockFor(path)
	mu.Lock()
	defer mu.Unlock()

	// Race-safety: another goroutine may have just finished extracting
	// while we were blocked on the lock. Re-check the archive state.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Archive gone → extraction already succeeded elsewhere. Find
		// the payload by stripping .7z and seeing what's there.
		if out := strings.TrimSuffix(path, filepath.Ext(path)); fileExists(out) {
			return out, nil
		}
		return "", fmt.Errorf("romcache: archive %s missing and no extracted payload found", path)
	}

	extracted, err := h.extract(path)
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		// Removal failure is annoying but not fatal — the extracted file
		// is usable. Log and continue; operator can clean the stale
		// archive later.
		h.Log.Warn().Err(err).Str("archive", path).
			Msg("[ROMCACHE] extract succeeded but source archive removal failed")
	}
	return extracted, nil
}

// extract runs 7z against src, puts the payload next to src, and returns
// the final payload path. Uses a dot-prefixed sibling dir for scratch so
// an interrupted extract leaves no partial files in the library.
func (h *Hydrator) extract(src string) (string, error) {
	parent := filepath.Dir(src)
	tmp, err := os.MkdirTemp(parent, ".extract-")
	if err != nil {
		return "", fmt.Errorf("romcache: mktemp: %w", err)
	}
	defer os.RemoveAll(tmp)

	start := time.Now()
	h.Log.Info().Str("src", filepath.Base(src)).Str("tmp", tmp).
		Msg("[ROMCACHE] extract begin")

	// -o<dir>: output directory. -y: assume yes. -bd: no progress bar.
	cmd := exec.Command("7z", "x", "-y", "-bd", "-o"+tmp, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("romcache: 7z x: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		return "", fmt.Errorf("romcache: read tmp: %w", err)
	}
	picked, pickedSize, err := pickPayload(tmp, entries)
	if err != nil {
		return "", err
	}
	// Final name = same dir as the archive, preserving the extracted
	// file's own name — 7z archives usually contain one file named
	// sensibly (e.g. "Halo.iso"), which we'd rather keep than guess by
	// stripping the archive suffix. The archive itself gets removed
	// afterwards so there's no collision with the library.
	final := filepath.Join(parent, filepath.Base(picked))
	if err := os.Rename(picked, final); err != nil {
		if cpErr := copyFile(picked, final); cpErr != nil {
			return "", fmt.Errorf("romcache: finalize %s: rename=%v, copy=%v", final, err, cpErr)
		}
	}
	h.Log.Info().
		Str("final", final).
		Int64("bytes", pickedSize).
		Dur("elapsed", time.Since(start)).
		Msg("[ROMCACHE] extract done")
	return final, nil
}

func pickPayload(dir string, entries []os.DirEntry) (string, int64, error) {
	var best string
	var bestSize int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Size() > bestSize {
			best = filepath.Join(dir, e.Name())
			bestSize = info.Size()
		}
	}
	if best == "" {
		return "", 0, fmt.Errorf("romcache: archive extracted no files")
	}
	return best, bestSize, nil
}

func (h *Hydrator) lockFor(key string) *sync.Mutex {
	v, _ := h.keyLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// isArchive decides which extensions trigger hydration.
func isArchive(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".7z")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// copyFile is the fallback for cross-filesystem finalize.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
