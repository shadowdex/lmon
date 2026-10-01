package stats

import (
	"fmt"
	"time"
)

// HumanCount renders a count compactly: 999, 1.5k, 25k, 2.5M, 1.2G.
func HumanCount(n int) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fG", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// HumanBytes renders a size with decimal units: 512 B, 1.5 kB, 38 MB, 2.1 GB.
func HumanBytes(n int64) string {
	f := float64(n)
	for _, u := range []struct {
		div  float64
		unit string
	}{{1e12, "TB"}, {1e9, "GB"}, {1e6, "MB"}, {1e3, "kB"}} {
		if f >= u.div {
			if v := f / u.div; v < 10 {
				return fmt.Sprintf("%.1f %s", v, u.unit)
			}
			return fmt.Sprintf("%.0f %s", f/u.div, u.unit)
		}
	}
	return fmt.Sprintf("%d B", n)
}

// Ago renders a duration as the single largest whole unit: "just now", "12m", "3h", "2d".
func Ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// BytesCell renders a byte total for a country row: "-" when the proxy saw none
// of the row's calls, and a trailing "*" when it saw only some of them.
func (r CountryRow) BytesCell(n int64) string {
	switch {
	case r.ByteCalls == 0:
		return "-"
	case r.ByteCalls < r.Calls:
		return HumanBytes(n) + "*"
	}
	return HumanBytes(n)
}

// AgoPhrase renders an age as a phrase: "just now", "12m ago", "3h ago", "2d ago".
func AgoPhrase(d time.Duration) string {
	if a := Ago(d); a != "just now" {
		return a + " ago"
	}
	return "just now"
}
