package netpath

import (
	"strings"
	"unicode"
)

// Hint is a location read out of a router's hostname.
type Hint struct {
	ISO     string
	Country string // empty for a bare country label
	City    string // empty for a bare country label
	Lat     float64
	Lon     float64
	Kind    string // "city" or "country"
}

var (
	cityByToken = map[string]city{}
	isoSet      = map[string]bool{}
)

func init() {
	for _, c := range cities {
		for _, t := range c.tokens {
			if _, dup := cityByToken[t]; dup {
				panic("netpath: duplicate city token " + t)
			}
			cityByToken[t] = c.city
		}
	}
	notCtry := map[string]bool{}
	for _, f := range strings.Fields(notCountry) {
		notCtry[f] = true
	}
	for _, f := range strings.Fields(isoCodes) {
		if !notCtry[f] {
			isoSet[f] = true
		}
	}
}

// trimDigits removes trailing digits: "par1" -> "par", "frnkge08" -> "frnkge".
func trimDigits(s string) string { return strings.TrimRightFunc(s, unicode.IsDigit) }

// HintFromHostname reads a location out of a router's reverse-DNS name, such as
// lag-101.ear6.paris1.level3.net (Paris) or ae2.3211.edge7.par1.neo.colt.net
// (Paris). It is a heuristic: it returns the first city token found, and
// otherwise a bare two-letter country label (".sg." in ...sngpsi07.sg.bb.gin.ntt.net).
// The last two labels are the operator's own domain and are never read.
func HintFromHostname(host string) (Hint, bool) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	labels := strings.Split(host, ".")
	if len(labels) < 3 {
		return Hint{}, false
	}
	own := labels[:len(labels)-2]

	for _, label := range own {
		for _, tok := range strings.FieldsFunc(label, func(r rune) bool { return r == '-' || r == '_' }) {
			if c, ok := cityByToken[trimDigits(tok)]; ok {
				return Hint{ISO: c.ISO, Country: c.Country, City: c.Name, Lat: c.Lat, Lon: c.Lon, Kind: "city"}, true
			}
		}
	}
	for _, label := range own {
		if len(label) != 2 {
			continue
		}
		code := strings.ToUpper(label)
		if code == "UK" {
			code = "GB" // .uk is the ccTLD, GB is the ISO code
		}
		if isoSet[code] {
			return Hint{ISO: code, Kind: "country"}, true
		}
	}
	return Hint{}, false
}
