// Package stats aggregates recorded events into per-provider/model summaries.
package stats

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sort"
	"time"

	"github.com/shadowdex/lmon/internal/proxy"
)

type Row struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Calls      int    `json:"calls"`
	Errors     int    `json:"errors"`
	Input      int    `json:"input_tokens"`
	Output     int    `json:"output_tokens"`
	CacheRead  int    `json:"cache_read_tokens"`
	CacheWrite int    `json:"cache_write_tokens"`
	// CacheHitRate is cache-read tokens / total input tokens.
	CacheHitRate float64 `json:"cache_hit_rate"`
	AvgTotalMs   float64 `json:"avg_total_ms"`
	P50TotalMs   float64 `json:"p50_total_ms"`
	P95TotalMs   float64 `json:"p95_total_ms"`
	AvgTTFBMs    float64 `json:"avg_ttfb_ms"`
}

// Load reads JSONL events, keeping those at or after since (zero = all).
func Load(r io.Reader, since time.Time) ([]proxy.Event, error) {
	var out []proxy.Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e proxy.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // tolerate a torn/partial line
		}
		if e.Time.Before(since) {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

type acc struct {
	Row
	totals, ttfbs []float64
}

// Aggregate groups events by provider and model.
func Aggregate(events []proxy.Event) []Row {
	m := map[[2]string]*acc{}
	for _, e := range events {
		model := e.Model
		if model == "" {
			model = "(unknown)"
		}
		k := [2]string{e.Provider, model}
		a := m[k]
		if a == nil {
			a = &acc{Row: Row{Provider: e.Provider, Model: model}}
			m[k] = a
		}
		a.Calls++
		if e.Status >= 400 {
			a.Errors++
		}
		a.Input += e.InputTokens
		a.Output += e.OutputTokens
		a.CacheRead += e.CacheReadTokens
		a.CacheWrite += e.CacheWriteTokens
		a.totals = append(a.totals, e.TotalMs)
		a.ttfbs = append(a.ttfbs, e.TTFBMs)
	}
	rows := make([]Row, 0, len(m))
	for _, a := range m {
		if tot := a.Input + a.CacheRead + a.CacheWrite; tot > 0 {
			a.CacheHitRate = float64(a.CacheRead) / float64(tot)
		}
		a.AvgTotalMs, a.P50TotalMs, a.P95TotalMs = mean(a.totals), pct(a.totals, 50), pct(a.totals, 95)
		a.AvgTTFBMs = mean(a.ttfbs)
		rows = append(rows, a.Row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		return rows[i].Model < rows[j].Model
	})
	return rows
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// pct is the nearest-rank percentile.
func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(p/100*float64(len(s))+0.999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

// Percentile is the nearest-rank percentile (p in 0-100) of v.
func Percentile(v []float64, p float64) float64 { return pct(v, p) }

// LoadFiles reads several JSONL logs (oldest first, as returned by
// proxy.LogFiles). A file whose last write is before since can't contain
// qualifying events, so it is skipped without being read.
func LoadFiles(paths []string, since time.Time) ([]proxy.Event, error) {
	var all []proxy.Event
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue // rotated away between listing and opening
			}
			return nil, err
		}
		if st, err := f.Stat(); err == nil && !since.IsZero() && st.ModTime().Before(since) {
			f.Close()
			continue
		}
		evs, err := Load(f, since)
		f.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, evs...)
	}
	return all, nil
}
