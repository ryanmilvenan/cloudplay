// Package romcache rehydrates compressed ROM archives in place next
// to their source archives on the NAS. Two archive shapes are handled:
//
//   - **Disc-image-packed**: the archive contains a single .iso / .xiso
//     file (maybe nested a folder deep). We just extract it.
//   - **Filesystem-packed**: the archive contains the *extracted* Xbox
//     file tree (`<Title>/default.xbe`, `<Title>/maps/*.map`, …).
//     xemu can't boot from loose files — we repack into an XISO via
//     `extract-xiso -c <dir> <out.xiso>`.
//
// After a successful hydration the source archive is removed so the
// next library scan sees a plain ISO (or XISO) and the 7z no longer
// lingers alongside.
//
// Flow (Resolve):
//
//  1. Path that isn't a supported archive → return as-is.
//  2. Path IS an archive → per-archive mutex, re-check post-lock.
//  3. If the archive is already gone (concurrent extract finished),
//     scan its directory for a matching extracted payload.
//  4. Otherwise, 7z x into a sibling .extract-XXXX temp dir.
//  5. Classify the extracted tree:
//       - Has a top-level .iso / .xiso file → move it next to the archive.
//       - Has default.xbe (anywhere in the tree) → extract-xiso -c
//         the containing dir into a <title>.xiso next to the archive.
//       - Otherwise, error.
//  6. Atomic-rename / extract-xiso finalize.
//  7. os.Remove the source archive.
//
// The temp dir is on the same NFS mount so rename is atomic; an
// interrupted extract leaves the source archive untouched.
package romcache

import (
	"fmt"
	"io"
	"io/fs"
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
	// while we were blocked on the lock. If the archive is gone but a
	// plausible payload sits next to it, reuse that.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if out, found := findLikelyPayload(filepath.Dir(path), payloadBaseName(path)); found {
			return out, nil
		}
		return "", fmt.Errorf("romcache: archive %s missing and no extracted payload found", path)
	}

	extracted, err := h.extract(path)
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		h.Log.Warn().Err(err).Str("archive", path).
			Msg("[ROMCACHE] extract succeeded but source archive removal failed")
	}
	return extracted, nil
}

// extract runs 7z against src, classifies the result (disc-image vs
// filesystem-packed), and produces a bootable path alongside src.
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

	iso, isoFound, xbeDir, xbeFound, err := classify(tmp)
	if err != nil {
		return "", err
	}

	final := filepath.Join(parent, payloadBaseName(src)+".xiso")
	switch {
	case isoFound:
		// Disc-image-packed: rename the extracted iso next to the archive.
		// Use the .xiso extension so xemu's mime-sniff treats it as an
		// Xbox disc. (xemu accepts .iso too, but .xiso is the idiomatic
		// name when the content is xiso-format.)
		final = filepath.Join(parent, filepath.Base(iso))
		if err := os.Rename(iso, final); err != nil {
			if cpErr := copyFile(iso, final); cpErr != nil {
				return "", fmt.Errorf("romcache: finalize %s: rename=%v, copy=%v", final, err, cpErr)
			}
		}
		size, _ := fileSize(final)
		h.Log.Info().Str("final", final).Int64("bytes", size).
			Dur("elapsed", time.Since(start)).
			Str("shape", "disc-image").
			Msg("[ROMCACHE] extract done")
		return final, nil

	case xbeFound:
		// Filesystem-packed: invoke extract-xiso to repack the tree into
		// a single xiso file next to the archive.
		if err := runExtractXiso(xbeDir, final); err != nil {
			return "", fmt.Errorf("romcache: repack xiso: %w", err)
		}
		size, _ := fileSize(final)
		h.Log.Info().Str("final", final).Int64("bytes", size).
			Dur("elapsed", time.Since(start)).
			Str("shape", "filesystem").
			Msg("[ROMCACHE] extract done")
		return final, nil

	default:
		return "", fmt.Errorf("romcache: archive contains neither an .iso/.xiso nor a default.xbe (%s)", src)
	}
}

// classify walks the extracted directory and decides which shape we
// got. Returns (isoPath, true, "", false) for disc-image shape or
// ("", false, xbeDir, true) for filesystem shape. If neither matches
// both bools are false.
//
// When both shapes are present (weird: an archive with both an ISO
// *and* a filesystem extraction), prefer the ISO — it's the thing the
// emulator can boot with less work.
func classify(root string) (isoPath string, isoFound bool, xbeDir string, xbeFound bool, err error) {
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		switch {
		case strings.HasSuffix(name, ".iso") || strings.HasSuffix(name, ".xiso"):
			isoPath = path
			isoFound = true
		case name == "default.xbe":
			// default.xbe lives at the root of the Xbox game's filesystem.
			// Its parent directory is what extract-xiso -c will repack.
			xbeDir = filepath.Dir(path)
			xbeFound = true
		}
		return nil
	})
	if walkErr != nil {
		return "", false, "", false, fmt.Errorf("romcache: walk extracted tree: %w", walkErr)
	}
	return isoPath, isoFound, xbeDir, xbeFound, nil
}

// runExtractXiso packs the given filesystem directory into an xiso
// image using the `extract-xiso` CLI. Writes to a neighbour temp path
// and atomic-renames to final so a crash doesn't leave a half-built
// xiso in the library.
func runExtractXiso(srcDir, finalPath string) error {
	// extract-xiso -c writes to <basename>.iso in the current directory.
	// We run it inside the srcDir's parent with a known output name.
	workParent := filepath.Dir(srcDir)
	cmd := exec.Command("extract-xiso", "-c", filepath.Base(srcDir))
	cmd.Dir = workParent
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract-xiso -c: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// extract-xiso names its output "<srcDirName>.iso" next to srcDir.
	produced := filepath.Join(workParent, filepath.Base(srcDir)+".iso")
	if _, err := os.Stat(produced); err != nil {
		return fmt.Errorf("extract-xiso output not found at %s: %w", produced, err)
	}
	if err := os.Rename(produced, finalPath); err != nil {
		if cpErr := copyFile(produced, finalPath); cpErr != nil {
			return fmt.Errorf("finalize xiso %s: rename=%v, copy=%v", finalPath, err, cpErr)
		}
		_ = os.Remove(produced)
	}
	return nil
}

// findLikelyPayload handles the post-race case where the archive is
// already gone. Scans for a file whose base name matches what we would
// have produced. Returns ("", false) if nothing obvious is there.
//
// Matches: <stem>.iso, <stem>.xiso. Case-insensitive.
func findLikelyPayload(dir, stem string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	stemLower := strings.ToLower(stem)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, stemLower) {
			continue
		}
		if strings.HasSuffix(lower, ".iso") || strings.HasSuffix(lower, ".xiso") {
			return filepath.Join(dir, name), true
		}
	}
	return "", false
}

// payloadBaseName strips the archive suffix to give the base name we
// use for derived filenames. `Halo.xiso.7z` → `Halo.xiso`.
func payloadBaseName(archive string) string {
	return strings.TrimSuffix(filepath.Base(archive), filepath.Ext(archive))
}

func (h *Hydrator) lockFor(key string) *sync.Mutex {
	v, _ := h.keyLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// isArchive decides which extensions trigger hydration.
func isArchive(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".7z")
}

// NeedsHydration is the public equivalent of isArchive — callers use it
// to decide whether to run the slow async path or the fast inline one.
// Cheap: string comparison only, no I/O.
func NeedsHydration(path string) bool {
	return isArchive(path)
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
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
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

