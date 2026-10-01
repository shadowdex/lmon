package geoip

import (
	"net/netip"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Location is what a City database knows about an address.
type Location struct {
	ISO     string  `json:"iso,omitempty"` // ISO 3166-1 alpha-2, e.g. "FR"
	Country string  `json:"country,omitempty"`
	City    string  `json:"city,omitempty"`
	Lat     float64 `json:"lat,omitempty"`
	Lon     float64 `json:"lon,omitempty"`
}

// HasCoords reports whether Lat/Lon are usable. (0,0) is treated as "none":
// it is a point in the Atlantic that no real record uses.
func (l Location) HasCoords() bool { return l.Lat != 0 || l.Lon != 0 }

// DB is an open City database (DB-IP Lite, MaxMind GeoLite2, ...).
type DB struct{ r *maxminddb.Reader }

func Open(path string) (*DB, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	return &DB{r: r}, nil
}

func (d *DB) Close() error { return d.r.Close() }

type record struct {
	Country struct {
		ISO   string            `maxminddb:"iso_code"`
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"country"`
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Location struct {
		Lat float64 `maxminddb:"latitude"`
		Lon float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

// Lookup returns the location of ip; ok is false when the database has no
// record for it. A nil DB never finds anything.
func (d *DB) Lookup(ip netip.Addr) (Location, bool) {
	if d == nil {
		return Location{}, false
	}
	res := d.r.Lookup(ip)
	if !res.Found() {
		return Location{}, false
	}
	var rec record
	if res.Decode(&rec) != nil {
		return Location{}, false
	}
	country := rec.Country.Names["en"]
	if country == "" {
		country = rec.Country.ISO
	}
	return Location{
		ISO: rec.Country.ISO, Country: country, City: rec.City.Names["en"],
		Lat: rec.Location.Lat, Lon: rec.Location.Lon,
	}, true
}
