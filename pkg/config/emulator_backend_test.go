package config

import "testing"

// TestGetBackend is the Phase-6 backend-dispatch contract: a system
// registered in the libretro core list with backend="xemu" routes to
// the xemu backend; everything else defaults to "libretro". The library
// scanner pokes this into every GameMetadata.Backend, and the worker's
// coordinatorhandlers.go branches on it.
func TestGetBackend(t *testing.T) {
	e := Emulator{
		Libretro: LibretroConfig{
			Cores: struct {
				Paths struct{ Libs string }
				Repo  LibretroRemoteRepo
				List  map[string]LibretroCoreConfig
			}{
				List: map[string]LibretroCoreConfig{
					"n64":  {Lib: "mupen64plus_libretro"},
					"ps2":  {Lib: "pcsx2_libretro"},
					"xbox": {Backend: "xemu"},
				},
			},
		},
	}

	cases := []struct {
		system, want string
	}{
		{"n64", "libretro"},  // standard libretro core
		{"ps2", "libretro"},  // standard libretro core
		{"xbox", "xemu"},     // explicit override
		{"zzz", "libretro"},  // unknown system → libretro default
		{"", "libretro"},     // empty → libretro default (best effort)
	}
	for _, c := range cases {
		if got := e.GetBackend(c.system); got != c.want {
			t.Errorf("GetBackend(%q) = %q, want %q", c.system, got, c.want)
		}
	}
}
