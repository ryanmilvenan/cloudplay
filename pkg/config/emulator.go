package config

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

type Emulator struct {
	FailFast         bool
	Threads          int
	Storage          string
	LocalPath        string
	Libretro         LibretroConfig
	AutosaveSec      int
	SkipLateFrames   bool
	LogDroppedFrames bool
}

type LibretroConfig struct {
	Cores struct {
		Paths struct {
			Libs string
		}
		Repo LibretroRemoteRepo
		List map[string]LibretroCoreConfig
	}
	DebounceMs      int
	Dup             bool
	SaveCompression bool
	LogLevel        int
}

type LibretroRemoteRepo struct {
	Sync      bool
	ExtLock   string
	Map       map[string]map[string]LibretroRepoMapInfo
	Main      LibretroRepoConfig
	Secondary LibretroRepoConfig
}

// LibretroRepoMapInfo contains Libretro core lib platform info.
// And the cores are just C-compiled libraries.
// See: https://buildbot.libretro.com/nightly.
type LibretroRepoMapInfo struct {
	Arch   string // bottom: x86_64, x86, ...
	Ext    string // platform dependent library file extension (dot-prefixed)
	Os     string // middle: windows, ios, ...
	Vendor string // top level: apple, nintendo, ...
}

type LibretroRepoConfig struct {
	Type        string
	Url         string
	Compression string
}

// Guess tries to map OS + CPU architecture to the corresponding remote URL path.
// See: https://gist.github.com/asukakenji/f15ba7e588ac42795f421b48b8aede63.
func (lrp LibretroRemoteRepo) Guess() (LibretroRepoMapInfo, error) {
	if os, ok := lrp.Map[runtime.GOOS]; ok {
		if arch, ok2 := os[runtime.GOARCH]; ok2 {
			return arch, nil
		}
	}
	return LibretroRepoMapInfo{},
		errors.New("core mapping not found for " + runtime.GOOS + ":" + runtime.GOARCH)
}

type LibretroCoreConfig struct {
	AltRepo         bool
	AutoGlContext   bool // hack: keep it here to pass it down the emulator
	// Backend selects which caged.ModName handles this system. Empty or
	// "libretro" routes through pkg/worker/caged/libretro (default).
	// "xemu" routes through pkg/worker/caged/xemu. Phase 6+.
	Backend         string
	CoreAspectRatio bool
	Folder          string
	Hacks           []string
	Height          int
	Hid             map[int][]int
	IsGlAllowed     bool
	KbMouseSupport  bool
	Lib             string
	NonBlockingSave bool
	Options         map[string]string
	Options4rom     map[string]map[string]string // <(^_^)>
	Roms            []string
	SaveStateFs     string
	Scale           float64
	UniqueSaveDir   bool
	UsesLibCo       bool
	PacerFps        int    `yaml:"pacerFps"`
	VFR             bool
	Width           int
}

type CoreInfo struct {
	Id      string
	Name    string
	AltRepo bool
}

// GetLibretroCoreConfig returns a core config with expanded paths.
func (e Emulator) GetLibretroCoreConfig(emulator string) LibretroCoreConfig {
	cores := e.Libretro.Cores
	conf := cores.List[emulator]
	conf.Lib = path.Join(cores.Paths.Libs, conf.Lib)
	return conf
}

// GetEmulator tries to find a suitable emulator.
// !to remove quadratic complexity
func (e Emulator) GetEmulator(rom string, path string) string {
	found := ""
	for emu, core := range e.Libretro.Cores.List {
		for _, romName := range core.Roms {
			if rom == romName {
				found = emu
				if p := strings.SplitN(filepath.ToSlash(path), "/", 2); len(p) > 1 {
					folder := p[0]
					if (folder != "" && folder == core.Folder) || folder == emu {
						return emu
					}
				}
			}
		}
	}
	return found
}

func (e Emulator) GetSupportedExtensions() []string {
	var extensions []string
	for _, core := range e.Libretro.Cores.List {
		extensions = append(extensions, core.Roms...)
	}
	return extensions
}

func (e Emulator) SessionStoragePath() string {
	return e.Storage
}

// GetBackend returns the caged.ModName backend (as a lowercase string) that
// should handle the given system. Resolution order:
//
//  1. CLOUDPLAY_BACKEND_<SYSTEM> env var (deploy-wide override; e.g.
//     CLOUDPLAY_BACKEND_DREAMCAST=flycast forces every Dreamcast launch to
//     the flycast native adapter without editing config.yaml).
//  2. `backend:` field on the system's entry in libretro.cores.list.
//  3. Defaults to "libretro" so every unedited system keeps dispatching
//     through the libretro path.
//
// A per-launch query-param override from the browser (plumbed through
// api.StartGameRequest.Backend) wins over all three at dispatch time; see
// HandleGameStart.
func (e Emulator) GetBackend(system string) string {
	if system != "" {
		envKey := "CLOUDPLAY_BACKEND_" + strings.ToUpper(system)
		if v := os.Getenv(envKey); v != "" {
			return v
		}
	}
	if core, ok := e.Libretro.Cores.List[system]; ok && core.Backend != "" {
		return core.Backend
	}
	return "libretro"
}

func (l *LibretroConfig) GetCores() (cores []CoreInfo) {
	for k, core := range l.Cores.List {
		cores = append(cores, CoreInfo{Id: k, Name: core.Lib, AltRepo: core.AltRepo})
	}
	return
}

func (l *LibretroConfig) GetCoresStorePath() string {
	pth, err := filepath.Abs(l.Cores.Paths.Libs)
	if err != nil {
		return ""
	}
	return pth
}
