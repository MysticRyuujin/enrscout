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

// ClassifyHosting labels an ASN organization name as cloud/datacenter by keyword.
func ClassifyHosting(org string) bool {
	hosting, _ := HostingClassification(org)
	return hosting
}

func HostingClassification(org string) (hosting, known bool) {
	l := strings.ToLower(org)
	for _, k := range residentialExceptions {
		if strings.Contains(l, k) {
			return false, true
		}
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
			r.Hosting, r.HostingKnown = HostingClassification(r.Org)
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
