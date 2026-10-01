package netpath

// city is a location that router hostnames commonly abbreviate.
type city struct {
	Name, ISO, Country string
	Lat, Lon           float64
}

// cities lists well-known network hubs. Each entry is reachable by several
// tokens in a hostname: the city name without spaces, IATA airport or metro
// codes, and the 6-letter CLLI-style codes some carriers use (frnkge08,
// londen12). Only tokens that can't be mistaken for router jargon are listed:
// "lag", "edge", "core", "bb" and similar must never match.
var cities = []struct {
	city
	tokens []string
}{
	// Europe
	{city{"Paris", "FR", "France", 48.8566, 2.3522}, []string{"paris", "par", "cdg", "parsfr"}},
	{city{"Marseille", "FR", "France", 43.2965, 5.3698}, []string{"marseille", "mrs"}},
	{city{"London", "GB", "United Kingdom", 51.5074, -0.1278}, []string{"london", "lon", "ldn", "lhr", "londen"}},
	{city{"Manchester", "GB", "United Kingdom", 53.4808, -2.2426}, []string{"manchester", "man"}},
	{city{"Frankfurt", "DE", "Germany", 50.1109, 8.6821}, []string{"frankfurt", "fra", "ffm", "frnkge"}},
	{city{"Berlin", "DE", "Germany", 52.52, 13.405}, []string{"berlin", "ber"}},
	{city{"Amsterdam", "NL", "Netherlands", 52.3676, 4.9041}, []string{"amsterdam", "ams", "amstnl"}},
	{city{"Brussels", "BE", "Belgium", 50.8503, 4.3517}, []string{"brussels", "bru"}},
	{city{"Madrid", "ES", "Spain", 40.4168, -3.7038}, []string{"madrid", "mad"}},
	{city{"Barcelona", "ES", "Spain", 41.3874, 2.1686}, []string{"barcelona", "bcn"}},
	{city{"Lisbon", "PT", "Portugal", 38.7223, -9.1393}, []string{"lisbon", "lis"}},
	{city{"Milan", "IT", "Italy", 45.4642, 9.19}, []string{"milan", "mil", "mxp"}},
	{city{"Rome", "IT", "Italy", 41.9028, 12.4964}, []string{"rome", "fco"}},
	{city{"Zurich", "CH", "Switzerland", 47.3769, 8.5417}, []string{"zurich", "zrh"}},
	{city{"Geneva", "CH", "Switzerland", 46.2044, 6.1432}, []string{"geneva", "gva"}},
	{city{"Vienna", "AT", "Austria", 48.2082, 16.3738}, []string{"vienna", "vie"}},
	{city{"Stockholm", "SE", "Sweden", 59.3293, 18.0686}, []string{"stockholm", "sto", "arn"}},
	{city{"Copenhagen", "DK", "Denmark", 55.6761, 12.5683}, []string{"copenhagen", "cph"}},
	{city{"Oslo", "NO", "Norway", 59.9139, 10.7522}, []string{"oslo", "osl"}},
	{city{"Helsinki", "FI", "Finland", 60.1699, 24.9384}, []string{"helsinki", "hel"}},
	{city{"Warsaw", "PL", "Poland", 52.2297, 21.0122}, []string{"warsaw", "waw"}},
	{city{"Prague", "CZ", "Czechia", 50.0755, 14.4378}, []string{"prague", "prg"}},
	{city{"Budapest", "HU", "Hungary", 47.4979, 19.0402}, []string{"budapest", "bud"}},
	{city{"Dublin", "IE", "Ireland", 53.3498, -6.2603}, []string{"dublin", "dub"}},
	{city{"Athens", "GR", "Greece", 37.9838, 23.7275}, []string{"athens", "ath"}},
	{city{"Istanbul", "TR", "Türkiye", 41.0082, 28.9784}, []string{"istanbul", "ist"}},
	{city{"Moscow", "RU", "Russia", 55.7558, 37.6173}, []string{"moscow", "mow", "svo"}},
	// North America
	{city{"New York", "US", "United States", 40.7128, -74.006}, []string{"newyork", "nyc", "jfk", "ewr", "nycmny"}},
	{city{"Ashburn", "US", "United States", 39.0438, -77.4874}, []string{"ashburn", "ash", "iad", "asbnva"}},
	{city{"Chicago", "US", "United States", 41.8781, -87.6298}, []string{"chicago", "chi", "ord", "chcgil"}},
	{city{"Dallas", "US", "United States", 32.7767, -96.797}, []string{"dallas", "dal", "dfw", "dllstx"}},
	{city{"Los Angeles", "US", "United States", 34.0522, -118.2437}, []string{"losangeles", "lax", "lsanca"}},
	{city{"San Jose", "US", "United States", 37.3382, -121.8863}, []string{"sanjose", "sjc", "snjsca"}},
	{city{"San Francisco", "US", "United States", 37.7749, -122.4194}, []string{"sanfrancisco", "sfo"}},
	{city{"Seattle", "US", "United States", 47.6062, -122.3321}, []string{"seattle", "sea", "sttlwa"}},
	{city{"Miami", "US", "United States", 25.7617, -80.1918}, []string{"miami", "mia", "miamfl"}},
	{city{"Atlanta", "US", "United States", 33.749, -84.388}, []string{"atlanta", "atl", "atlnga"}},
	{city{"Denver", "US", "United States", 39.7392, -104.9903}, []string{"denver", "den"}},
	{city{"Toronto", "CA", "Canada", 43.6532, -79.3832}, []string{"toronto", "yyz", "tor"}},
	{city{"Montreal", "CA", "Canada", 45.5017, -73.5673}, []string{"montreal", "yul", "mtl"}},
	{city{"Vancouver", "CA", "Canada", 49.2827, -123.1207}, []string{"vancouver", "yvr"}},
	// Asia-Pacific, Middle East, Africa, South America
	{city{"Singapore", "SG", "Singapore", 1.3521, 103.8198}, []string{"singapore", "sin", "sngpsi"}},
	{city{"Tokyo", "JP", "Japan", 35.6762, 139.6503}, []string{"tokyo", "tyo", "nrt", "hnd", "tokyjp"}},
	{city{"Osaka", "JP", "Japan", 34.6937, 135.5023}, []string{"osaka", "osa", "kix"}},
	{city{"Hong Kong", "HK", "Hong Kong", 22.3193, 114.1694}, []string{"hongkong", "hkg"}},
	{city{"Seoul", "KR", "South Korea", 37.5665, 126.978}, []string{"seoul", "sel", "icn"}},
	{city{"Mumbai", "IN", "India", 19.076, 72.8777}, []string{"mumbai", "bom"}},
	{city{"Delhi", "IN", "India", 28.6139, 77.209}, []string{"delhi", "del"}},
	{city{"Sydney", "AU", "Australia", -33.8688, 151.2093}, []string{"sydney", "syd", "sydnau"}},
	{city{"Melbourne", "AU", "Australia", -37.8136, 144.9631}, []string{"melbourne", "mel"}},
	{city{"Dubai", "AE", "United Arab Emirates", 25.2048, 55.2708}, []string{"dubai", "dxb"}},
	{city{"Tel Aviv", "IL", "Israel", 32.0853, 34.7818}, []string{"telaviv", "tlv"}},
	{city{"Johannesburg", "ZA", "South Africa", -26.2041, 28.0473}, []string{"johannesburg", "jnb"}},
	{city{"São Paulo", "BR", "Brazil", -23.5505, -46.6333}, []string{"saopaulo", "sao", "gru"}},
}

// isoCodes are the ISO 3166-1 alpha-2 country codes, used to recognise a bare
// country label such as ".de." in a hostname.
const isoCodes = "AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ " +
	"CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR " +
	"GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP " +
	"KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT " +
	"MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW " +
	"SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG " +
	"UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW"

// notCountry are two-letter labels that are valid country codes but in router
// names almost always mean something else (bb = backbone, pe/ce = provider/
// customer edge, ae/xe/ge/et = interface types, ...).
const notCountry = "BB CR AR BR PE GW CE PR SW AE XE GE ET PO VL LO IX DC AS ME IP VX TO"
