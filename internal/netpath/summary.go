package netpath

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Summary is the slim, regenerable record of a path that other views use: which
// countries the observed path crosses and where it enters the provider. It
// deliberately holds no router addresses, so the saved file does not map your
// network.
type Summary struct {
	Host           string    `json:"host"`
	Dest           string    `json:"dest"`
	Family         string    `json:"family"` // "IPv4" or "IPv6"
	SavedAt        time.Time `json:"saved_at"`
	Viewpoint      string    `json:"viewpoint,omitempty"`
	Reached        bool      `json:"reached"`
	Countries      []string  `json:"countries"` // observed on the path, in order
	EdgeKind       string    `json:"edge_kind"` // "nearby", "consistent" or "unknown"
	EdgeISO        string    `json:"edge_iso,omitempty"`
	EdgeCity       string    `json:"edge_city,omitempty"`
	EdgeMaxKm      float64   `json:"edge_max_km,omitempty"`
	RegisteredISO  string    `json:"registered_iso,omitempty"`
	RegisteredCity string    `json:"registered_city,omitempty"`
}

// Summary condenses an assessed path.
func (p Path) Summary(now time.Time) Summary {
	s := Summary{
		Host: p.Host, Dest: p.Dest.String(), Family: p.family(), SavedAt: now.UTC(),
		Reached: p.Reached, Countries: append([]string{}, p.Countries...),
		EdgeKind: p.Edge.Kind, EdgeISO: p.Edge.ISO, EdgeCity: p.Edge.City, EdgeMaxKm: p.Edge.MaxKm,
		RegisteredISO: p.Edge.Registered.ISO, RegisteredCity: p.Edge.Registered.City,
	}
	if p.Vantage != nil {
		s.Viewpoint = p.Vantage.Label
	}
	return s
}

// Enters is a short label for where traffic enters the provider: a place when
// the answering router is known ("FR Paris"), "near you" when only the distance
// is, and a leading "≈" when it is merely consistent with the registered place.
func (s Summary) Enters() string {
	switch s.EdgeKind {
	case "nearby":
		if where := placeLabel(s.EdgeISO, s.EdgeCity); where != "?" {
			return where
		}
		return "near you"
	case "consistent":
		return "≈ " + placeLabel(s.EdgeISO, s.EdgeCity)
	}
	return "unknown"
}

// Age is how long ago the path was measured.
func (s Summary) Age(now time.Time) time.Duration { return now.Sub(s.SavedAt) }

// ---- store ----

// DefaultStorePath is ~/.lmon/paths.json.
func DefaultStorePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lmon", "paths.json")
}

// Store keeps the latest Summary per endpoint host in a small JSON file.
type Store struct{ Path string }

type storeFile struct {
	Paths map[string]Summary `json:"paths"`
}

var storeMu sync.Mutex // one writer at a time within a process

// Load returns the saved summaries by host. A missing file is an empty store.
func (s Store) Load() (map[string]Summary, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Summary{}, nil
		}
		return nil, err
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Paths == nil {
		f.Paths = map[string]Summary{}
	}
	return f.Paths, nil
}

// Save merges sums into the store (the newest per host wins) and writes it
// atomically. An unreadable existing file is replaced: the data can be
// regenerated with `lmon path`.
func (s Store) Save(sums ...Summary) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	cur, err := s.Load()
	if err != nil {
		cur = map[string]Summary{}
	}
	for _, x := range sums {
		cur[x.Host] = x
	}
	b, err := json.MarshalIndent(storeFile{Paths: cur}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".paths-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}
