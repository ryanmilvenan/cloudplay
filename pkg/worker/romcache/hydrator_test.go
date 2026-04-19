package romcache

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// TestResolveNonArchive asserts the zero-cost passthrough.
func TestResolveNonArchive(t *testing.T) {
	h := &Hydrator{Log: logger.Default()}
	got, err := h.Resolve("/some/path/game.iso")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "/some/path/game.iso" {
		t.Fatalf("passthrough broken: got %q", got)
	}
}

// TestIsArchiveCase covers the extension-match case-insensitivity.
func TestIsArchiveCase(t *testing.T) {
	cases := map[string]bool{
		"game.7z":   true,
		"game.7Z":   true,
		"game.iso":  false,
		"game.zip":  false,
		"archive.": false,
		"noext":    false,
	}
	for in, want := range cases {
		if got := isArchive(in); got != want {
			t.Errorf("isArchive(%q)=%v want %v", in, got, want)
		}
	}
}

// TestResolveExtractsAndRemoves is the happy-path integration: a real
// 7z archive → extract → remove archive → return payload path. Skipped
// when 7z isn't on PATH (e.g. Mac CI without p7zip installed).
func TestResolveExtractsAndRemoves(t *testing.T) {
	if _, err := exec.LookPath("7z"); err != nil {
		t.Skip("7z not on PATH — skipping real-extract test")
	}
	dir := t.TempDir()

	// Materialize a payload, pack it into a .7z, run the hydrator.
	payload := filepath.Join(dir, "fake-halo.iso")
	body := bytes.Repeat([]byte("XBOX"), 1024) // 4 KiB
	if err := os.WriteFile(payload, body, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	archive := payload + ".7z"
	if out, err := exec.Command("7z", "a", "-mx=0", archive, payload).CombinedOutput(); err != nil {
		t.Fatalf("7z a: %v (%s)", err, out)
	}
	if err := os.Remove(payload); err != nil {
		t.Fatalf("clean source payload: %v", err)
	}

	h := &Hydrator{Log: logger.Default()}
	got, err := h.Resolve(archive)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Extracted file should live next to the archive with the inner name.
	if filepath.Base(got) != "fake-halo.iso" {
		t.Errorf("unexpected payload name %q", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("extracted file missing: %v", err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Errorf("archive should have been removed; stat err=%v", err)
	}

	// Bytes round-trip intact.
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if !bytes.Equal(b, body) {
		t.Errorf("payload bytes changed through 7z: got %d bytes, want %d", len(b), len(body))
	}
}

// TestResolveRaceReuse verifies that once a concurrent extract has
// completed and removed the archive, a second Resolve on the same
// (now-absent) archive path returns the existing payload.
func TestResolveRaceReuse(t *testing.T) {
	if _, err := exec.LookPath("7z"); err != nil {
		t.Skip("7z not on PATH — skipping")
	}
	dir := t.TempDir()
	payload := filepath.Join(dir, "g.iso")
	if err := os.WriteFile(payload, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	archive := payload + ".7z"
	if out, err := exec.Command("7z", "a", "-mx=0", archive, payload).CombinedOutput(); err != nil {
		t.Fatalf("7z a: %v (%s)", err, out)
	}
	if err := os.Remove(payload); err != nil {
		t.Fatalf("rm source: %v", err)
	}

	h := &Hydrator{Log: logger.Default()}
	if _, err := h.Resolve(archive); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Archive is gone, payload exists. Second call should find the
	// payload by stripping the .7z suffix and not error.
	got, err := h.Resolve(archive)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if filepath.Base(got) != "g.iso" {
		t.Errorf("expected payload path, got %q", got)
	}
}
