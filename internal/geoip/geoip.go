// Package geoip downloads and locates the free DB-IP "IP to City Lite" database.
//
// DB-IP Lite is licensed CC BY 4.0: it needs no account or key, but anything
// that shows its results must credit DB-IP.com (see Attribution). lmon
// downloads it onto the user's machine and never redistributes it.
package geoip

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

const (
	DefaultBaseURL = "https://download.db-ip.com/free"
	Attribution    = "IP geolocation by DB-IP.com (CC BY 4.0)"
)

// DefaultPath is ~/.lmon/dbip-city-lite.mmdb.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lmon", "dbip-city-lite.mmdb")
}

// Resolve picks the database to use: explicit flag/env value first, then the
// default path if it exists. It returns "" when none is available.
func Resolve(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if _, err := os.Stat(DefaultPath()); err == nil {
		return DefaultPath()
	}
	return ""
}

type Options struct {
	BaseURL string       // default DefaultBaseURL
	Dest    string       // default DefaultPath()
	Now     time.Time    // default time.Now(); injectable for tests
	Client  *http.Client // default: 10 minute timeout
	Out     io.Writer    // progress messages; may be nil
}

// Update downloads the current month's database, falling back to the previous
// month when the new one isn't published yet. The file is validated as an
// mmdb before it replaces Dest, so a failed or partial download never clobbers
// a working database. It returns the path written.
func Update(ctx context.Context, o Options) (string, error) {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	if o.Dest == "" {
		o.Dest = DefaultPath()
	}
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Minute}
	}
	say := func(f string, a ...any) {
		if o.Out != nil {
			fmt.Fprintf(o.Out, f+"\n", a...)
		}
	}

	if err := os.MkdirAll(filepath.Dir(o.Dest), 0o755); err != nil {
		return "", err
	}
	months := []time.Time{o.Now, o.Now.AddDate(0, -1, 0)}
	var lastErr error
	for _, m := range months {
		url := fmt.Sprintf("%s/dbip-city-lite-%s.mmdb.gz", o.BaseURL, m.Format("2006-01"))
		say("downloading %s", url)
		err := download(ctx, o.Client, url, o.Dest)
		if err == nil {
			say("saved %s", o.Dest)
			say("%s", Attribution)
			return o.Dest, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", lastErr
}

func download(ctx context.Context, c *http.Client, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".geoip-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	// The server sends a .gz file; Go won't decode it for us since it isn't a
	// Content-Encoding.
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("%s: not a gzip file: %w", url, err)
	}
	if _, err := io.Copy(tmp, gz); err != nil {
		tmp.Close()
		return fmt.Errorf("%s: %w", url, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Validate before replacing the existing database.
	db, err := maxminddb.Open(tmp.Name())
	if err != nil {
		return fmt.Errorf("%s: downloaded file is not a valid mmdb: %w", url, err)
	}
	db.Close()
	return os.Rename(tmp.Name(), dest)
}

// Age reports how old the database file is.
func Age(path string) (time.Duration, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return time.Since(st.ModTime()), nil
}
