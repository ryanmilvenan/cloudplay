package games

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

func TestLibraryScan(t *testing.T) {
	t.Skip("XEMU-WIP: requires sample ROMs in gitignored assets/games/ dir; " +
		"tracked in docs/test-hygiene-todo.md, revisit post-Phase-7")
	tests := []struct {
		directory string
		expected  []struct {
			name   string
			system string
		}
	}{
		{
			directory: "../../assets/games",
			expected: []struct {
				name   string
				system string
			}{
				{name: "Alwa's Awakening (Demo)", system: "nes"},
				{name: "Sushi The Cat", system: "gba"},
				{name: "anguna", system: "gba"},
			},
		},
	}

	emuConf := config.Emulator{Libretro: config.LibretroConfig{}}
	emuConf.Libretro.Cores.List = map[string]config.LibretroCoreConfig{
		"nes": {Roms: []string{"nes"}},
		"gba": {Roms: []string{"gba"}},
	}

	l := logger.NewConsole(false, "w", false)
	for _, test := range tests {
		library := NewLib(config.Library{
			BasePath:  test.directory,
			Supported: []string{"gba", "zip", "nes"},
		}, emuConf, l)
		library.Scan()
		games := library.GetAll()

		all := true
		for _, expect := range test.expected {
			found := false
			for _, game := range games {
				if game.Name == expect.name && (expect.system != "" && expect.system == game.System) {
					found = true
					break
				}
			}
			all = all && found
		}
		if !all {
			t.Errorf("Test fail for dir %v with %v != %v", test.directory, games, test.expected)
		}
	}
}

// TestScanSuppressesDiscImageTracks pins the fix for Seaman and every other
// CUE+BIN / GDI+BIN archive that extracts into a dir full of track files:
// only the .cue / .gdi manifest should surface as a library entry, not each
// individual track. Regression-guards against the pre-fix behaviour where
// "Seaman (USA) (Track 1).bin" etc. appeared as separate PS2 entries
// because PS2's rom list includes .bin.
func TestScanSuppressesDiscImageTracks(t *testing.T) {
	root := t.TempDir()
	dc := filepath.Join(root, "dreamcast", "Seaman (USA)")
	if err := os.MkdirAll(dc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, n := range []string{
		"Seaman (USA).cue",
		"Seaman (USA) (Track 1).bin",
		"Seaman (USA) (Track 2).bin",
		"Seaman (USA) (Track 3).bin",
	} {
		if err := os.WriteFile(filepath.Join(dc, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	// Safety: a dir with ONLY .bin files (no manifest) must still scan —
	// some cores distribute as lone .bin.
	lonely := filepath.Join(root, "ps2")
	if err := os.MkdirAll(lonely, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lonely, "Solo Title.bin"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write solo: %v", err)
	}

	emuConf := config.Emulator{Libretro: config.LibretroConfig{}}
	emuConf.Libretro.Cores.List = map[string]config.LibretroCoreConfig{
		"dreamcast": {Folder: "dreamcast", Roms: []string{"cue", "gdi"}},
		"ps2":       {Folder: "ps2", Roms: []string{"iso", "bin"}},
	}

	l := logger.NewConsole(false, "w", false)
	lib := NewLib(config.Library{
		BasePath:  root,
		Supported: []string{"cue", "gdi", "bin"},
	}, emuConf, l)
	lib.Scan()
	games := lib.GetAll()

	var cueCount, trackCount, soloCount int
	for _, g := range games {
		switch {
		case g.Name == "Seaman (USA)" && g.Type == "cue":
			cueCount++
		case g.Type == "bin" && g.System == "dreamcast":
			trackCount++
		case g.Type == "bin" && g.Name == "Solo Title":
			soloCount++
		}
	}
	if cueCount != 1 {
		t.Errorf("expected 1 .cue entry for Seaman, got %d (all=%+v)", cueCount, games)
	}
	if trackCount != 0 {
		t.Errorf("expected 0 track .bin entries under dreamcast/, got %d", trackCount)
	}
	if soloCount != 1 {
		t.Errorf("expected lone .bin outside a disc-image dir to still scan, got %d", soloCount)
	}
}

func TestAliasFileMaybe(t *testing.T) {
	lib := &library{
		config: libConf{
			aliasFile: "alias",
			path:      os.TempDir(),
		},
		log: logger.NewConsole(false, "w", false),
	}

	contents := "a=b\nc=d\n"

	path := filepath.Join(lib.config.path, lib.config.aliasFile)
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Error(err)
	}
	defer func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	}()

	want := map[string]string{}
	want["a"] = "b"
	want["c"] = "d"

	aliases := lib.AliasFileMaybe()

	if !reflect.DeepEqual(aliases, want) {
		t.Errorf("AliasFileMaybe() = %v, want %v", aliases, want)
	}
}

func TestAliasFileMaybeNot(t *testing.T) {
	lib := &library{
		config: libConf{
			path: os.TempDir(),
		},
		log: logger.NewConsole(false, "w", false),
	}

	aliases := lib.AliasFileMaybe()
	if aliases != nil {
		t.Errorf("should be nil, but %v", aliases)
	}
}

func Benchmark(b *testing.B) {
	log := logger.Default()
	logger.SetGlobalLevel(logger.Disabled)
	library := NewLib(config.Library{
		BasePath:  "../../assets/games",
		Supported: []string{"gba", "zip", "nes"},
	}, config.Emulator{}, log)

	for b.Loop() {
		library.Scan()
		_ = library.GetAll()
	}
}
