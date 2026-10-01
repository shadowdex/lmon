package stats

import (
	"testing"
	"time"
)

func TestHumanCount(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1.0k", 1500: "1.5k", 9999: "10.0k", 10000: "10k", 25000: "25k", 999999: "999k", 1_000_000: "1.0M", 2_500_000: "2.5M", 1_200_000_000: "1.2G"} {
		if got := HumanCount(in); got != want {
			t.Errorf("HumanCount(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 512: "512 B", 999: "999 B", 1000: "1.0 kB", 1500: "1.5 kB", 38_000: "38 kB", 2_100_000: "2.1 MB", 38_000_000: "38 MB", 4_300_000_000: "4.3 GB", 12_000_000_000_000: "12 TB"} {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAgo(t *testing.T) {
	for in, want := range map[time.Duration]string{0: "just now", 59 * time.Second: "just now", 5 * time.Minute: "5m", 3 * time.Hour: "3h", 47 * time.Hour: "47h", 49 * time.Hour: "2d", 10 * 24 * time.Hour: "10d"} {
		if got := Ago(in); got != want {
			t.Errorf("Ago(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestBytesCell(t *testing.T) {
	if got := (CountryRow{Calls: 3, ByteCalls: 0}).BytesCell(0); got != "-" {
		t.Errorf("no proxied calls: %q", got)
	}
	if got := (CountryRow{Calls: 3, ByteCalls: 2}).BytesCell(2_000_000); got != "2.0 MB*" {
		t.Errorf("partial coverage: %q", got)
	}
	if got := (CountryRow{Calls: 3, ByteCalls: 3}).BytesCell(2_000_000); got != "2.0 MB" {
		t.Errorf("full coverage: %q", got)
	}
}

func TestAgoPhrase(t *testing.T) {
	for in, want := range map[time.Duration]string{-time.Hour: "just now", 10 * time.Second: "just now", 5 * time.Minute: "5m ago", 3 * time.Hour: "3h ago", 72 * time.Hour: "3d ago"} {
		if got := AgoPhrase(in); got != want {
			t.Errorf("AgoPhrase(%v) = %q, want %q", in, got, want)
		}
	}
}
