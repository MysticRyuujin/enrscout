package enrich

import (
	"net"
	"strings"

	"github.com/oschwald/geoip2-golang"
)

type GeoResult struct {
	Country          string
	City             string
	Subdivision      string
	Lat              float64
	Lon              float64
	ASN              uint
	Org              string
	Hosting          bool
	HostingKnown     bool
	Geolocated       bool
	AccuracyRadiusKM uint16
}

var hostingKeywords = []string{
	"amazon", "aws", "google", "microsoft", "azure", "hetzner", "ovh", "digitalocean",
	"digital ocean", "linode", "akamai", "cloudflare", "contabo", "vultr", "leaseweb",
	"oracle", "alibaba", "tencent", "scaleway",
	"m247", "choopa", "upcloud", "gcore", "netcup", "ionos", "fastly", "equinix",
	"kamatera", "latitude", "constant",
	"teraswitch", "allnodes", "limestone", "hivelocity", "velia", "atlantis capital",
	"inovare", "datacamp", "worldstream", "webnx", "exaion", "nessus", "veloxserv",
	"nebius", "zenlayer", "coolhousing", "aeza", "mevspace", "melbikomas",
	"host africa", "eurohoster", "gthost", "cherry servers",
}

var residentialExceptions = []string{"google fiber", "google-fiber"}

// hostingASNs classifies providers whose GeoLite organization name carries no
// hosting keyword. Keyed by ASN because org strings churn across GeoLite
// releases and generic name substrings collide; the value records the brand
// the entry was verified against. AS6939 (Hurricane Electric) is deliberately
// absent: its 2001:470::/32 tunnelbroker space terminates in homes, so the
// ASN cannot be classified without a prefix split.
var hostingASNs = map[uint]string{
	2734:   "CoreSite",
	6698:   "Virtual Systems",
	7979:   "Servers.com",
	11878:  "tzulo",
	13649:  "Flexential",
	13767:  "DataBank",
	18779:  "EGIHosting",
	18978:  "Enzu",
	19318:  "Interserver",
	21529:  "Novva Data Centers",
	22612:  "Namecheap",
	23470:  "ReliableSite",
	25369:  "Hydra Communications",
	26666:  "Interserver",
	26832:  "ServaRICA",
	27424:  "Lucky Friday Labs",
	29014:  "ScaleUp Technologies",
	29182:  "ISPsystem",
	29909:  "Metro Optic",
	30176:  "Priority Colo",
	32475:  "Internap",
	34989:  "ServeTheWorld",
	39351:  "31173 Services",
	39498:  "LMAX Digital",
	39572:  "DataWeb Global Group",
	41536:  "01node",
	43350:  "NForce Entertainment",
	43641:  "Sollutium",
	44051:  "Fornex Hosting",
	45671:  "Servers Australia",
	47447:  "23M",
	48314:  "IP-Projects",
	49505:  "Selectel",
	50372:  "Planetary Networks",
	53850:  "GorillaServers",
	57043:  "Hostkey",
	59711:  "HZ Hosting",
	59780:  "Surfboxx",
	60800:  "Netwise Hosting",
	61098:  "Exoscale",
	61272:  "BAcloud",
	62005:  "BlueVPS",
	62240:  "Clouvider",
	63018:  "Dedicated.com",
	135377: "UCloud (HK)",
	136907: "Huawei Cloud",
	142002: "Scloud",
	197352: "Tinext Cloud",
	200295: "Skoed",
	202613: "Aruba",
	206264: "KoDDoS",
	210976: "Timeweb",
	212477: "RoyaleHosting",
	213459: "Snowd",
	397423: "Tier.Net",
}

// ClassifyHosting labels a node's autonomous system as cloud/datacenter by
// ASN or organization-name keyword.
func ClassifyHosting(asn uint, org string) bool {
	hosting, _ := HostingClassification(asn, org)
	return hosting
}

func HostingClassification(asn uint, org string) (hosting, known bool) {
	l := strings.ToLower(org)
	for _, k := range residentialExceptions {
		if strings.Contains(l, k) {
			return false, true
		}
	}
	if _, ok := hostingASNs[asn]; ok {
		return true, true
	}
	for _, k := range hostingKeywords {
		if strings.Contains(l, k) {
			return true, true
		}
	}
	return false, false
}

type Geo struct {
	city *geoip2.Reader
	asn  *geoip2.Reader
}

func OpenGeo(cityPath, asnPath string) (*Geo, error) {
	g := &Geo{}
	if cityPath != "" {
		r, err := geoip2.Open(cityPath)
		if err != nil {
			return nil, err
		}
		g.city = r
	}
	if asnPath != "" {
		r, err := geoip2.Open(asnPath)
		if err != nil {
			g.Close()
			return nil, err
		}
		g.asn = r
	}
	if g.city == nil && g.asn == nil {
		return nil, nil
	}
	return g, nil
}

func (g *Geo) Lookup(ip net.IP) GeoResult {
	var r GeoResult
	if ip == nil {
		return r
	}
	if g.city != nil {
		if c, err := g.city.City(ip); err == nil {
			r.Country = c.Country.IsoCode
			r.City = c.City.Names["en"]
			if len(c.Subdivisions) > 0 {
				r.Subdivision = c.Subdivisions[0].Names["en"]
			}
			r.Lat = c.Location.Latitude
			r.Lon = c.Location.Longitude
			r.Geolocated = true
			r.AccuracyRadiusKM = c.Location.AccuracyRadius
		}
	}
	if g.asn != nil {
		if a, err := g.asn.ASN(ip); err == nil {
			r.ASN = a.AutonomousSystemNumber
			r.Org = a.AutonomousSystemOrganization
			r.Hosting, r.HostingKnown = HostingClassification(r.ASN, r.Org)
		}
	}
	return r
}

func (g *Geo) Close() {
	if g.city != nil {
		g.city.Close()
	}
	if g.asn != nil {
		g.asn.Close()
	}
}
