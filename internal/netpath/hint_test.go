package netpath

import "testing"

func TestHintFromRealRouterNames(t *testing.T) {
	cases := []struct {
		host, city, iso, kind string
	}{
		// seen in real traceroutes from France
		{"lag-101.ear6.paris1.level3.net", "Paris", "FR", "city"},
		{"ae2.3211.edge7.par1.neo.colt.net", "Paris", "FR", "city"},
		// well-known carrier naming schemes
		{"ae-7.r20.sngpsi07.sg.bb.gin.ntt.net", "Singapore", "SG", "city"},
		{"ae-0.r20.frnkge08.de.bb.gin.ntt.net", "Frankfurt", "DE", "city"},
		{"be2231.ccr41.par01.atlas.cogentco.com", "Paris", "FR", "city"},
		{"ae12.cr3-fra2.ip4.gtt.net", "Frankfurt", "DE", "city"},
		{"ae1.cr1-ams1.ip4.example.net", "Amsterdam", "NL", "city"},
		{"xe-0-0-1.core1.newyork2.example.net", "New York", "US", "city"},
		{"PARIS1.LEVEL3.NET.", "Paris", "FR", "city"}, // case and the trailing root dot don't matter
		{"router7.paris.net", "", "", ""},             // "paris" is part of the operator's domain: never read
		{"gw.lyon.fra.example.net", "Frankfurt", "DE", "city"},
		// a bare country label when there is no city
		{"core1.edge.de.example.net", "", "DE", "country"},
		{"gw.uk.example.net", "", "GB", "country"},
	}
	for _, c := range cases {
		h, ok := HintFromHostname(c.host)
		if c.iso == "" {
			if ok {
				t.Errorf("%q should give no hint, got %+v", c.host, h)
			}
			continue
		}
		if !ok || h.ISO != c.iso || h.City != c.city || h.Kind != c.kind {
			t.Errorf("%q -> %+v ok=%v, want %s/%s/%s", c.host, h, ok, c.city, c.iso, c.kind)
		}
		if c.kind == "city" && (h.Lat == 0 && h.Lon == 0) {
			t.Errorf("%q: a city hint must carry coordinates", c.host)
		}
	}
}

// Router jargon must never be mistaken for a place.
func TestHintIgnoresRouterJargonAndCustomerNames(t *testing.T) {
	for _, host := range []string{
		"station15.multimania.isdnet.net",  // real: a customer-facing name
		"lag-101.ear6.core.example.net",    // lag is not Lagos
		"ae-1.bb1.edge7.example.net",       // bb is not Barbados; ae is not the UAE
		"xe-0-0-1.cr3.pe2.ce1.example.net", // pe/ce/cr are roles, not Peru/Costa Rica
		"et-1-0-0.agg3.dist2.example.net",  // et is not Ethiopia
		"gw1.ip4.gtt.net",                  // too few labels of its own
		"mail.example.com",                 // nothing there
		"",
		"localhost",
		"vl100.sw2.dc1.example.net", // vlan, switch, datacenter
		"po12.ar1.br2.example.net",  // po, ar, br are roles
	} {
		if h, ok := HintFromHostname(host); ok {
			t.Errorf("%q wrongly gave %+v", host, h)
		}
	}
}

func TestEveryCityTokenIsUniqueAndSane(t *testing.T) {
	// init() panics on duplicates; this also guards against tokens that clash with router words
	banned := []string{"lag", "core", "edge", "ear", "bb", "net", "gw", "ae", "xe", "ge", "et", "po", "cr", "br", "ar", "pe", "ce", "sw", "agg", "dist", "ip", "ix"}
	for _, b := range banned {
		if _, bad := cityByToken[b]; bad {
			t.Errorf("router word %q must not be a city token", b)
		}
	}
	for tok, c := range cityByToken {
		if c.Lat < -90 || c.Lat > 90 || c.Lon < -180 || c.Lon > 180 || len(c.ISO) != 2 {
			t.Errorf("%s -> bad city %+v", tok, c)
		}
		if tok != trimDigits(tok) {
			t.Errorf("token %q ends in a digit and could never match", tok)
		}
		if !isoSet[c.ISO] && !containsISO(c.ISO) {
			t.Errorf("%s has unknown ISO code %s", tok, c.ISO)
		}
	}
}

func containsISO(code string) bool {
	for i := 0; i+2 <= len(isoCodes); i++ {
		if isoCodes[i:i+2] == code && (i == 0 || isoCodes[i-1] == ' ') {
			return true
		}
	}
	return false
}

func TestCountryLabelDenylist(t *testing.T) {
	for _, code := range []string{"BB", "PE", "CE", "AE", "GE", "ET", "PO"} {
		if isoSet[code] {
			t.Errorf("%s must not count as a country label", code)
		}
	}
	for _, code := range []string{"DE", "FR", "US", "JP", "SG", "GB"} {
		if !isoSet[code] {
			t.Errorf("%s should count as a country label", code)
		}
	}
}
