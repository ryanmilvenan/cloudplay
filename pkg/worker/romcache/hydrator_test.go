package romcache

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// TestDirSizeRecurses asserts that dirSize() counts every regular file
// under root (including in subdirectories) while ignoring the
// directory-entry metadata itself.
func TestDirSizeRecurses(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("1234"), 0o644); err != nil {
		t.Fatalf("a: %v", err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b"), []byte("12345"), 0o644); err != nil {
		t.Fatalf("b: %v", err)
	}
	if got := dirSize(root); got != 9 {
		t.Errorf("dirSize = %d, want 9", got)
	}
}

// TestResolveNonArchive asserts the zero-cost passthrough.
func TestResolveNonArchive(t *testing.T) {
	h := &Hydrator{Log: logger.Default()}
	got, err := h.ResolveNoProgress("/some/path/game.iso")
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
		"game.7z":  true,
		"game.7Z":  true,
		"game.iso": false,
		"game.zip": false,
		"archive.": false,
		"noext":    false,
	}
	for in, want := range cases {
		if got := isArchive(in); got != want {
			t.Errorf("isArchive(%q)=%v want %v", in, got, want)
		}
	}
}

// TestResolveDiscImagePacked exercises the happy path for archives
// containing a single .iso file (possibly nested one directory deep).
func TestResolveDiscImagePacked(t *testing.T) {
	if _, err := exec.LookPath("7z"); err != nil {
		t.Skip("7z not on PATH — skipping real-extract test")
	}
	dir := t.TempDir()

	// Put the iso inside a subfolder to test the recursive classify walk.
	innerDir := filepath.Join(dir, "Halo - Combat Evolved")
	if err := os.Mkdir(innerDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := filepath.Join(innerDir, "Halo.iso")
	body := bytes.Repeat([]byte("XBOX"), 1024)
	if err := os.WriteFile(payload, body, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	archive := filepath.Join(dir, "Halo.iso.7z")
	if out, err := exec.Command("7z", "a", "-mx=0", archive, innerDir).CombinedOutput(); err != nil {
		t.Fatalf("7z a: %v (%s)", err, out)
	}
	if err := os.RemoveAll(innerDir); err != nil {
		t.Fatalf("clean source: %v", err)
	}

	h := &Hydrator{Log: logger.Default()}
	got, err := h.ResolveNoProgress(archive)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if filepath.Base(got) != "Halo.iso" {
		t.Errorf("unexpected payload name %q", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("extracted file missing: %v", err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Errorf("archive should have been removed; stat err=%v", err)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if !bytes.Equal(b, body) {
		t.Errorf("payload bytes changed through 7z")
	}
}

// TestResolveFilesystemPacked covers the "extracted Xbox filesystem"
// shape — archive wraps a `<Title>/default.xbe` tree, hydrator must
// invoke extract-xiso to repack into an xiso.
func TestResolveFilesystemPacked(t *testing.T) {
	if _, err := exec.LookPath("7z"); err != nil {
		t.Skip("7z not on PATH — skipping")
	}
	if _, err := exec.LookPath("extract-xiso"); err != nil {
		t.Skip("extract-xiso not on PATH — skipping")
	}
	dir := t.TempDir()
	gameDir := filepath.Join(dir, "Halo - Combat Evolved")
	mapsDir := filepath.Join(gameDir, "maps")
	if err := os.MkdirAll(mapsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Put a stub default.xbe + a map file so classify() finds both.
	if err := os.WriteFile(filepath.Join(gameDir, "default.xbe"), []byte("XBEH"), 0o644); err != nil {
		t.Fatalf("xbe: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mapsDir, "pillar.map"), []byte("map"), 0o644); err != nil {
		t.Fatalf("map: %v", err)
	}
	archive := filepath.Join(dir, "Halo.7z")
	if out, err := exec.Command("7z", "a", "-mx=0", archive, gameDir).CombinedOutput(); err != nil {
		t.Fatalf("7z a: %v (%s)", err, out)
	}
	if err := os.RemoveAll(gameDir); err != nil {
		t.Fatalf("clean source: %v", err)
	}

	h := &Hydrator{Log: logger.Default()}
	got, err := h.ResolveNoProgress(archive)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !bytes.HasSuffix([]byte(got), []byte(".xiso")) {
		t.Errorf("expected .xiso payload path, got %q", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("xiso missing: %v", err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Errorf("archive not removed: %v", err)
	}
}

// TestClassifyPrefersIso exercises the mixed-archive edge case where
// the tree contains both an ISO and a default.xbe.
func TestClassifyPrefersIso(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "game.iso"), []byte("x"), 0o644); err != nil {
		t.Fatalf("iso: %v", err)
	}
	xbeDir := filepath.Join(dir, "Game")
	if err := os.Mkdir(xbeDir, 0o755); err != nil {
		t.Fatalf("mk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(xbeDir, "default.xbe"), []byte("x"), 0o644); err != nil {
		t.Fatalf("xbe: %v", err)
	}
	shape, err := classify(dir)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if shape.kind != "disc-image" {
		t.Errorf("expected kind=disc-image, got %q", shape.kind)
	}
	if shape.isoPath == "" {
		t.Error("isoPath empty despite disc-image kind")
	}
	if shape.xbeDir == "" {
		t.Error("xbeDir empty — classify should record both even though iso wins")
	}
}

// TestClassifyGdi covers the Dreamcast multi-track shape: a .gdi plus
// track files at the archive root. classify() should return kind="gdi"
// and a gdiPath pointing at the .gdi file.
func TestClassifyGdi(t *testing.T) {
	dir := t.TempDir()
	writes := []struct{ name, body string }{
		{"Power Stone 2 (USA).gdi", "3\n1 0 4 2352 track01.bin 0\n"},
		{"track01.bin", "x"},
		{"track02.raw", "x"},
		{"track03.bin", "x"},
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(dir, w.name), []byte(w.body), 0o644); err != nil {
			t.Fatalf("write %s: %v", w.name, err)
		}
	}
	shape, err := classify(dir)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if shape.kind != "gdi" {
		t.Errorf("expected kind=gdi, got %q", shape.kind)
	}
	if shape.gdiPath == "" {
		t.Error("gdiPath empty despite gdi kind")
	}
}

// TestClassifyCue covers the CUE+BIN shape (Dreamcast Seaman and others
// distributed this way): a .cue manifest plus .bin track files at the
// archive root. classify() should return kind="cue" and a cuePath
// pointing at the .cue file.
func TestClassifyCue(t *testing.T) {
	dir := t.TempDir()
	writes := []struct{ name, body string }{
		{"Seaman (USA).cue", "FILE \"Seaman (USA) (Track 1).bin\" BINARY\n  TRACK 01 MODE1/2352\n"},
		{"Seaman (USA) (Track 1).bin", "x"},
		{"Seaman (USA) (Track 2).bin", "x"},
		{"Seaman (USA) (Track 3).bin", "x"},
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(dir, w.name), []byte(w.body), 0o644); err != nil {
			t.Fatalf("write %s: %v", w.name, err)
		}
	}
	shape, err := classify(dir)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if shape.kind != "cue" {
		t.Errorf("expected kind=cue, got %q", shape.kind)
	}
	if shape.cuePath == "" {
		t.Error("cuePath empty despite cue kind")
	}
}

// TestClassifySingleFileDreamcast covers .chd / .cdi archives — single-file
// Dreamcast disc formats handled like disc-image: no repacking, just rename.
func TestClassifySingleFileDreamcast(t *testing.T) {
	for _, ext := range []string{".chd", ".cdi"} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			name := "Crazy Taxi (USA)" + ext
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			shape, err := classify(dir)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if shape.kind != "disc-image" {
				t.Errorf("expected kind=disc-image, got %q", shape.kind)
			}
			if shape.isoPath == "" {
				t.Error("isoPath empty despite disc-image kind")
			}
		})
	}
}

// TestResolveRaceReuse verifies the post-lock re-check returns the
// extracted payload when the archive is already gone.
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
	if _, err := h.ResolveNoProgress(archive); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	got, err := h.ResolveNoProgress(archive)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if filepath.Base(got) != "g.iso" {
		t.Errorf("expected payload path, got %q", got)
	}
}
