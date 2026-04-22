// Package romcache rehydrates compressed ROM archives in place next
// to their source archives on the NAS. Three archive shapes are handled:
//
//   - **Disc-image-packed** (Xbox): the archive contains a single
//     .iso / .xiso file (maybe nested a folder deep). Extract and
//     drop it next to the archive.
//   - **Filesystem-packed** (Xbox): the archive contains the *extracted*
//     Xbox file tree (`<Title>/default.xbe`, `<Title>/maps/*.map`, …).
//     xemu can't boot from loose files — repack into an XISO via
//     `extract-xiso -c <dir> <out.xiso>`.
//   - **GDI multi-track** (Dreamcast): the archive contains `<title>.gdi`
//     alongside `track01.bin` / `track02.raw` / `track03.bin` / etc.
//     Flycast loads the .gdi and follows relative track references, so
//     we keep the files together — each archive extracts into its own
//     `<parent>/<title>/` subdirectory (track filenames collide across
//     games, so a per-title folder is the only safe layout).
//
// After a successful hydration the source archive is removed so the
// next library scan sees the extracted payload and the 7z no longer
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
//       - .iso / .xiso top-level → move next to the archive.
//       - default.xbe anywhere → extract-xiso -c the containing dir
//         into a <title>.xiso next to the archive.
//       - .gdi at root → rename the extract dir to <parent>/<title>/
//         and return the .gdi path inside.
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// ProgressFunc is the worker-facing hook for hydration progress. Called
// from arbitrary goroutines (7z stdout reader, the stage transitions).
// Passing nil disables progress entirely — all reporting is optional.
//
//   stage   — one of "extract" | "repack" | "done" | "start"
//   percent — 0-100 when known, -1 when the stage has no granular progress
//   extras  — short human-readable hint ("Halo - Combat Evolved",
//             "1.2 GB", etc.). Free-form.
type ProgressFunc func(stage string, percent int, extras string)

// Hydrator turns archive paths into extracted paths. Thread-safe; safe
// to share a single instance across the worker.
type Hydrator struct {
	// Log receives structured progress.
	Log *logger.Logger

	keyLocks sync.Map // source-path → *sync.Mutex
}

// Resolve returns a filesystem path the emulator can read directly. For
// supported archives it extracts, removes the original, and returns the
// extracted path. For anything else it returns path unchanged. Passing
// a nil progress func disables progress reporting; callers who only
// care about the result can use ResolveNoProgress.
func (h *Hydrator) Resolve(path string, progress ProgressFunc) (string, error) {
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
			report(progress, "done", 100, "already hydrated")
			return out, nil
		}
		return "", fmt.Errorf("romcache: archive %s missing and no extracted payload found", path)
	}

	report(progress, "start", 0, filepath.Base(path))
	extracted, err := h.extract(path, progress)
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		h.Log.Warn().Err(err).Str("archive", path).
			Msg("[ROMCACHE] extract succeeded but source archive removal failed")
	}
	report(progress, "done", 100, filepath.Base(extracted))
	return extracted, nil
}

// ResolveNoProgress is Resolve without a progress callback, useful for
// callers that don't have a UI or for unit tests that don't care.
func (h *Hydrator) ResolveNoProgress(path string) (string, error) {
	return h.Resolve(path, nil)
}

// report is a nil-safe dispatcher so callers don't have to guard every
// progress emit.
func report(p ProgressFunc, stage string, percent int, extras string) {
	if p == nil {
		return
	}
	p(stage, percent, extras)
}

// extract runs 7z against src, classifies the result (disc-image vs
// filesystem-packed), and produces a bootable path alongside src. The
// progress callback fires ~20 times during the 7z phase (one per 5%
// of uncompressed bytes written) and once at each stage transition
// ("extract" → "repack" → "done").
func (h *Hydrator) extract(src string, progress ProgressFunc) (string, error) {
	parent := filepath.Dir(src)
	tmp, err := os.MkdirTemp(parent, ".extract-")
	if err != nil {
		return "", fmt.Errorf("romcache: mktemp: %w", err)
	}
	defer os.RemoveAll(tmp)

	start := time.Now()
	h.Log.Info().Str("src", filepath.Base(src)).Str("tmp", tmp).
		Msg("[ROMCACHE] extract begin")

	// 7z's own percent output (-bsp1) uses backspace-based in-place
	// terminal updates, which doesn't line-parse cleanly. Side-step
	// that entirely: get the archive's uncompressed total up front,
	// then a poll loop watches tmp's on-disk size and computes our
	// own percent. Robust, no dependence on 7z's stdout formatting.
	total, err := sevenZipUncompressedSize(src)
	if err != nil {
		// Non-fatal — extract continues with indeterminate progress.
		h.Log.Warn().Err(err).Msg("[ROMCACHE] 7z listing failed; extract progress will be indeterminate")
		total = 0
	}

	srcBase := payloadBaseName(src)
	cmd := exec.Command("7z", "x", "-y", "-bd", "-o"+tmp, src)
	var outBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("romcache: 7z start: %w", err)
	}
	// Poll extraction progress until the process exits.
	doneCh := make(chan struct{})
	go pollExtractProgress(tmp, total, doneCh, func(pct int) {
		if pct < 0 {
			report(progress, "extract", -1, srcBase)
			return
		}
		report(progress, "extract", pct, srcBase)
	})
	waitErr := cmd.Wait()
	close(doneCh)
	if waitErr != nil {
		return "", fmt.Errorf("romcache: 7z x: %w (output: %s)", waitErr, strings.TrimSpace(outBuf.String()))
	}

	shape, err := classify(tmp)
	if err != nil {
		return "", err
	}

	switch shape.kind {
	case "disc-image":
		// Disc-image-packed: rename the extracted iso next to the archive.
		// Use the .xiso extension so xemu's mime-sniff treats it as an
		// Xbox disc. (xemu accepts .iso too, but .xiso is the idiomatic
		// name when the content is xiso-format.)
		final := filepath.Join(parent, filepath.Base(shape.isoPath))
		if err := os.Rename(shape.isoPath, final); err != nil {
			if cpErr := copyFile(shape.isoPath, final); cpErr != nil {
				return "", fmt.Errorf("romcache: finalize %s: rename=%v, copy=%v", final, err, cpErr)
			}
		}
		size, _ := fileSize(final)
		h.Log.Info().Str("final", final).Int64("bytes", size).
			Dur("elapsed", time.Since(start)).
			Str("shape", "disc-image").
			Msg("[ROMCACHE] extract done")
		return final, nil

	case "filesystem":
		// Filesystem-packed: invoke extract-xiso to repack the tree into
		// a single xiso file next to the archive. extract-xiso doesn't
		// emit machine-readable percent, so we mark the stage transition
		// once and rely on the UI to show a spinner until "done".
		final := filepath.Join(parent, payloadBaseName(src)+".xiso")
		report(progress, "repack", -1, filepath.Base(shape.xbeDir))
		if err := runExtractXiso(shape.xbeDir, final); err != nil {
			return "", fmt.Errorf("romcache: repack xiso: %w", err)
		}
		size, _ := fileSize(final)
		h.Log.Info().Str("final", final).Int64("bytes", size).
			Dur("elapsed", time.Since(start)).
			Str("shape", "filesystem").
			Msg("[ROMCACHE] extract done")
		return final, nil

	case "gdi", "cue":
		// Multi-track disc dumps (Dreamcast GDI or CUE+BIN — structurally
		// identical: a manifest file references track files by relative
		// path, so everything must live in the same dir. Track filenames
		// (track01.bin, Seaman (USA) (Track 1).bin, …) collide across
		// games, so each game gets its own <parent>/<title>/ subdir.
		// Rename the entire extract temp dir into its final home.
		finalDir := filepath.Join(parent, payloadBaseName(src))
		if _, err := os.Stat(finalDir); err == nil {
			if err := os.RemoveAll(finalDir); err != nil {
				return "", fmt.Errorf("romcache: clear stale %s: %w", finalDir, err)
			}
		}
		// The tmp dir is on the same filesystem as parent (we created
		// it with MkdirTemp(parent, …)), so rename is atomic.
		if err := os.Rename(tmp, finalDir); err != nil {
			return "", fmt.Errorf("romcache: rename %s → %s: %w", tmp, finalDir, err)
		}
		tmp = ""
		manifestPath := shape.gdiPath
		if shape.kind == "cue" {
			manifestPath = shape.cuePath
		}
		final := filepath.Join(finalDir, filepath.Base(manifestPath))
		size, _ := dirSizeInt64(finalDir)
		h.Log.Info().Str("final", final).Int64("bytes", size).
			Dur("elapsed", time.Since(start)).
			Str("shape", shape.kind).
			Msg("[ROMCACHE] extract done")
		return final, nil

	default:
		return "", fmt.Errorf("romcache: archive %s matched no known shape (no .iso/.xiso/.chd/.cdi, no default.xbe, no .gdi/.cue)", src)
	}
}

// dirSizeInt64 wraps dirSize to return (bytes, nil) — provides the
// same shape as fileSize so logging code can stay consistent across
// "shape==file" (xbox) and "shape==dir" (dreamcast) outputs.
func dirSizeInt64(root string) (int64, error) { return dirSize(root), nil }

// extractShape summarizes what a classify() walk found in the extracted
// tmp tree. Only one of {isoPath, xbeDir, gdiPath, cuePath} is
// meaningful, selected by `kind`.
type extractShape struct {
	kind    string // "disc-image" | "filesystem" | "gdi" | "cue" | "unknown"
	isoPath string
	xbeDir  string
	gdiPath string
	cuePath string
}

// classify walks the extracted directory and decides which shape we
// got. Precedence when multiple signals are present: disc-image beats
// filesystem (ISO is already boot-ready, no repack needed) beats gdi.
// In practice archives are single-shape so precedence rarely matters.
func classify(root string) (extractShape, error) {
	var s extractShape
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		switch {
		case strings.HasSuffix(name, ".iso") ||
			strings.HasSuffix(name, ".xiso") ||
			// Single-file Dreamcast disc formats (flycast-native). No
			// repacking needed; treat as disc-image and rename in place.
			strings.HasSuffix(name, ".chd") ||
			strings.HasSuffix(name, ".cdi"):
			s.isoPath = path
		case name == "default.xbe":
			// default.xbe lives at the root of the Xbox game's filesystem.
			// Its parent directory is what extract-xiso -c will repack.
			s.xbeDir = filepath.Dir(path)
		case strings.HasSuffix(name, ".gdi"):
			s.gdiPath = path
		case strings.HasSuffix(name, ".cue"):
			// CUE+BIN multi-track dumps (Seaman and other DC titles
			// distributed this way). Handled structurally like .gdi:
			// the .cue references .bin tracks by relative name.
			s.cuePath = path
		}
		return nil
	})
	if walkErr != nil {
		return extractShape{}, fmt.Errorf("romcache: walk extracted tree: %w", walkErr)
	}
	switch {
	case s.isoPath != "":
		s.kind = "disc-image"
	case s.xbeDir != "":
		s.kind = "filesystem"
	case s.gdiPath != "":
		s.kind = "gdi"
	case s.cuePath != "":
		s.kind = "cue"
	default:
		s.kind = "unknown"
	}
	return s, nil
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
// already gone. Scans for something whose name matches what we would
// have produced.
//
// Matches, in order:
//  1. File <stem>.iso / <stem>.xiso (Xbox output).
//  2. Directory <stem>/ containing a <stem>.gdi (Dreamcast output).
//
// Case-insensitive.
func findLikelyPayload(dir, stem string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	stemLower := strings.ToLower(stem)
	// First pass: Xbox file outputs.
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(e.Name())
		if !strings.HasPrefix(lower, stemLower) {
			continue
		}
		if strings.HasSuffix(lower, ".iso") || strings.HasSuffix(lower, ".xiso") {
			return filepath.Join(dir, e.Name()), true
		}
	}
	// Second pass: Dreamcast directory output.
	for _, e := range entries {
		if !e.IsDir() || strings.ToLower(e.Name()) != stemLower {
			continue
		}
		gdi := filepath.Join(dir, e.Name(), stem+".gdi")
		if _, err := os.Stat(gdi); err == nil {
			return gdi, true
		}
		// Accept any .gdi inside (name might differ slightly from stem).
		inner, _ := os.ReadDir(filepath.Join(dir, e.Name()))
		for _, f := range inner {
			if strings.HasSuffix(strings.ToLower(f.Name()), ".gdi") {
				return filepath.Join(dir, e.Name(), f.Name()), true
			}
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

// sevenZipUncompressedSize parses the "Size = <n>" total line out of
// `7z l -slt <archive>`. Returns the sum of all per-file Size entries
// — the total uncompressed bytes we expect to see in the extract dir
// once 7z is done. On any parse failure returns (0, err) and the
// caller falls back to indeterminate progress.
func sevenZipUncompressedSize(archive string) (int64, error) {
	out, err := exec.Command("7z", "l", "-slt", archive).Output()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "Size = ") {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "Size = ")), 10, 64)
		if err == nil {
			total += v
		}
	}
	if total == 0 {
		return 0, fmt.Errorf("romcache: no Size entries in 7z listing")
	}
	return total, nil
}

// pollExtractProgress watches the extract tmp dir's on-disk size every
// 500 ms and fires the callback with the progress-vs-total percent.
// Exits when doneCh closes (the 7z process ended). Quantizes reports
// to 5% steps so we don't spam the data channel.
func pollExtractProgress(tmp string, total int64, doneCh <-chan struct{}, fn func(pct int)) {
	lastReported := -1
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-doneCh:
			if lastReported < 100 {
				fn(100)
			}
			return
		case <-ticker.C:
			size := dirSize(tmp)
			if total <= 0 {
				// Unknown total: emit indeterminate pulse.
				fn(-1)
				continue
			}
			pct := int(size * 100 / total)
			if pct > 99 {
				pct = 99 // save 100 for the final close-doneCh flush
			}
			if pct/5 == lastReported/5 {
				continue
			}
			lastReported = pct
			fn(pct)
		}
	}
}

// dirSize recursively adds up file sizes under root. Walking errors
// are swallowed — a transient filesystem hiccup during polling
// shouldn't crash the hydrator.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
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

