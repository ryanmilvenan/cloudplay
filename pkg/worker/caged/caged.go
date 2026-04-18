package caged

import (
	"errors"
	"reflect"

	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/app"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/libretro"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/xemu"
)

type Manager struct {
	list map[ModName]app.App
	log  *logger.Logger
}

const (
	RetroPad = libretro.RetroPad
	Keyboard = libretro.Keyboard
	Mouse    = libretro.Mouse
)

type ModName string

const (
	Libretro ModName = "libretro"
	Xemu     ModName = "xemu"
)

func NewManager(log *logger.Logger) *Manager {
	return &Manager{log: log, list: make(map[ModName]app.App)}
}

func (m *Manager) Get(name ModName) app.App { return m.list[name] }

func (m *Manager) Load(name ModName, conf any) error {
	switch name {
	case Libretro:
		caged, err := m.loadLibretro(conf)
		if err != nil {
			return err
		}
		m.list[name] = caged
	case Xemu:
		caged, err := m.loadXemu(conf)
		if err != nil {
			return err
		}
		m.list[name] = caged
	}
	return nil
}

func (m *Manager) loadXemu(conf any) (*xemu.Caged, error) {
	s := reflect.ValueOf(conf)
	x := s.FieldByName("Xemu")
	if !x.IsValid() {
		return nil, errors.New("no xemu conf")
	}
	xc, ok := x.Interface().(config.XemuConfig)
	if !ok {
		return nil, errors.New("xemu conf wrong type")
	}
	if !xc.Enabled {
		return nil, errors.New("xemu backend disabled in config")
	}
	caged := xemu.Cage(xemu.CagedConf{Xemu: xc}, m.log)
	if err := caged.Init(); err != nil {
		return nil, err
	}
	return &caged, nil
}

func (m *Manager) loadLibretro(conf any) (*libretro.Caged, error) {
	s := reflect.ValueOf(conf)

	e := s.FieldByName("Emulator")
	if !e.IsValid() {
		return nil, errors.New("no emulator conf")
	}
	r := s.FieldByName("Recording")
	if !r.IsValid() {
		return nil, errors.New("no recording conf")
	}

	c := libretro.CagedConf{
		Emulator:  e.Interface().(config.Emulator),
		Recording: r.Interface().(config.Recording),
	}

	caged := libretro.Cage(c, m.log)
	if err := caged.Init(); err != nil {
		return nil, err
	}
	return &caged, nil
}
