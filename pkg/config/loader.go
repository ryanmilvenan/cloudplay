package config

import (
	"bytes"
	"embed"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/knadh/koanf/maps"
	"github.com/knadh/koanf/v2"
	"gopkg.in/yaml.v3"
)

const EnvPrefix = "CLOUD_GAME_"

var (
	//go:embed config.yaml
	conf embed.FS
)

type Kv = map[string]any
type Bytes []byte

func (b *Bytes) ReadBytes() ([]byte, error) { return *b, nil }
func (b *Bytes) Read() (Kv, error)          { return nil, nil }

type File string

func (f *File) ReadBytes() ([]byte, error) { return os.ReadFile(string(*f)) }
func (f *File) Read() (Kv, error)          { return nil, nil }

type YAML struct{}

func (p *YAML) Marshal(Kv) ([]byte, error) { return nil, nil }
func (p *YAML) Unmarshal(b []byte) (Kv, error) {
	var out Kv
	klw := keysToLower(b)
	decoder := yaml.NewDecoder(bytes.NewReader(klw))
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// keysToLower iterates YAML bytes and tries to lower the keys.
// Used for merging with environment vars which are lowered as well.
func keysToLower(in []byte) []byte {
	l, r, ignore := 0, 0, false
	for i, b := range in {
		switch b {
		case '#': // skip comments
			ignore = true
		case ':': // lower left chunk before the next : symbol
			if ignore {
				continue
			}
			r = i
			ignore = true
			for j := l; j <= r; j++ {
				c := in[j]
				// we skip the line with the first explicit " string symbol
				if c == '"' {
					break
				}
				if 'A' <= c && c <= 'Z' {
					in[j] += 'a' - 'A'
				}
			}
		case '\n':
			l = i
			ignore = false
		}
	}
	return in
}

type Env string

func (e *Env) ReadBytes() ([]byte, error) { return nil, nil }
func (e *Env) Read() (Kv, error) {
	var keys []string
	for _, k := range os.Environ() {
		if strings.HasPrefix(k, string(*e)) {
			keys = append(keys, k)
		}
	}
	mp := make(Kv)
	for _, k := range keys {
		parts := strings.SplitN(k, "=", 2)
		if parts == nil {
			continue
		}
		n := strings.ToLower(strings.TrimPrefix(parts[0], string(*e)))
		if n == "" {
			continue
		}
		// convert VAR_VAR to VAR.VAR or if we need to preserve _
		// i.e. VAR_VAR__KEY_HAS_SLASHES to VAR.VAR.KEY_HAS_SLASHES
		// with the result: VAR: { VAR: { KEY_HAS_SLASHES: '' } } }
		x := strings.Index(n, "__")
		var key string
		if x == -1 {
			key = strings.Replace(n, "_", ".", -1)
		} else {
			key = strings.Replace(n[:x+1], "_", ".", -1) + n[x+2:]
		}
		if len(parts) > 1 {
			mp[key] = parts[1]
		}
	}
	collectArrayIndexedKeys(mp)
	return maps.Unflatten(mp, "."), nil
}

// collectArrayIndexedKeys rewrites entries whose final path segment is
// `name[N]` into a single `name` key whose value is a slice with index N set,
// so env overrides like CLOUD_GAME_ENCODER_AUDIO_FRAMES[0]=10 produce
// `encoder.audio.frames = [10, ...]` instead of the literal key
// `encoder.audio.frames[0]` which koanf's Unflatten can't route into a Go
// slice field.
func collectArrayIndexedKeys(mp Kv) {
	type entry struct {
		key string
		idx int
	}
	groups := map[string][]entry{}
	for k := range mp {
		dot := strings.LastIndex(k, ".")
		last := k[dot+1:]
		open := strings.LastIndex(last, "[")
		if open <= 0 || !strings.HasSuffix(last, "]") {
			continue
		}
		idx, err := strconv.Atoi(last[open+1 : len(last)-1])
		if err != nil || idx < 0 {
			continue
		}
		base := k[:dot+1] + last[:open]
		groups[base] = append(groups[base], entry{key: k, idx: idx})
	}
	for base, es := range groups {
		sort.Slice(es, func(i, j int) bool { return es[i].idx < es[j].idx })
		max := es[len(es)-1].idx
		arr := make([]any, max+1)
		for _, e := range es {
			arr[e.idx] = mp[e.key]
			delete(mp, e.key)
		}
		mp[base] = arr
	}
}

// LoadConfig loads a configuration file into the given struct.
// The path param specifies a custom path to the configuration file.
// Reads and puts environment variables with the prefix CLOUD_GAME_.
func LoadConfig(config any, path string) (loaded []string, err error) {
	dirs := []string{".", "configs", "../../../configs"}
	if path != "" {
		dirs = append([]string{path}, dirs...)
	}

	homeDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		homeDir = home + "/.cr"
		dirs = append(dirs, homeDir)
	}

	k := koanf.New("_") // move to global scope if configs become dynamic
	defer k.Delete("")
	data, err := conf.ReadFile("config.yaml")
	if err != nil {
		return nil, err
	}
	conf := Bytes(data)
	if err := k.Load(&conf, &YAML{}); err != nil {
		return nil, err
	}
	loaded = append(loaded, "default")

	for _, dir := range dirs {
		path := filepath.Join(filepath.Clean(dir), "config.yaml")
		f := File(path)
		if _, err := os.Stat(string(f)); !os.IsNotExist(err) {
			if err := k.Load(&f, &YAML{}); err != nil {
				return loaded, err
			}
			loaded = append(loaded, path)
		}
	}

	env := Env(EnvPrefix)
	if err := k.Load(&env, nil); err != nil {
		return loaded, err
	}

	if err := k.Unmarshal("", config); err != nil {
		return loaded, err
	}

	return loaded, nil
}
