package geoip

import (
	"net/netip"
	"os"
	"testing"
)

func TestNilDBFindsNothing(t *testing.T) {
	var d *DB
	if _, ok := d.Lookup(netip.MustParseAddr("1.1.1.1")); ok {
		t.Fatal("nil DB must not find anything")
	}
}

func TestHasCoords(t *testing.T) {
	if (Location{}).HasCoords() {
		t.Error("zero location has no coordinates")
	}
	if !(Location{Lat: 48.85, Lon: 2.35}).HasCoords() || !(Location{Lon: 2.35}).HasCoords() {
		t.Error("a non-zero latitude or longitude counts")
	}
}

func TestOpenMissingFile(t *testing.T) {
	if _, err := Open(t.TempDir() + "/nope.mmdb"); err == nil {
		t.Fatal("expected an error")
	}
}

// Runs only where a real database exists (a developer machine, not CI).
func TestLookupAgainstRealDatabase(t *testing.T) {
	path := DefaultPath()
	if _, err := os.Stat(path); err != nil {
		t.Skip("no GeoIP database at " + path)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	loc, ok := d.Lookup(netip.MustParseAddr("8.8.8.8"))
	if !ok || loc.ISO != "US" || loc.Country == "" {
		t.Fatalf("8.8.8.8 -> %+v ok=%v", loc, ok)
	}
	if _, ok := d.Lookup(netip.MustParseAddr("192.168.1.1")); ok {
		t.Error("a private address should have no record")
	}
}
