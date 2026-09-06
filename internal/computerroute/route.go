// Package computerroute defines credential-free addresses advertised by a
// trusted computer. A valid address is still only a candidate: clients verify
// its TLS certificate and backend identity before presenting credentials.
package computerroute

import (
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// MaxRoutes bounds remembered alternatives, in addition to the original
// pairing endpoint. A host normally advertises LAN, tailnet and its domain.
const MaxRoutes = 4

type Route struct {
	Endpoint        string `json:"endpoint"`
	CertFingerprint string `json:"certFingerprint,omitempty"`
}

// Normalize accepts an HTTPS origin, never a link with credentials or a path.
// An empty fingerprint means WebPKI, never unverified TLS.
func Normalize(route Route) (Route, error) {
	u, err := url.Parse(strings.TrimSpace(route.Endpoint))
	if err != nil || len(route.Endpoint) > 2048 || strings.Contains(route.Endpoint, "#") || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return Route{}, errors.New("computer route must be an HTTPS origin without credentials, path, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "%") || host == "" || len(host) > 253 {
		return Route{}, errors.New("computer route has an invalid host")
	}
	if net.ParseIP(host) == nil {
		if strings.ContainsAny(u.Host, "[]") {
			return Route{}, errors.New("computer route has an invalid IP address")
		}
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return Route{}, errors.New("computer route has an invalid host")
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return Route{}, errors.New("computer route has an invalid host")
				}
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Route{}, errors.New("computer route has an invalid port")
		}
		port = strconv.Itoa(n)
		if port == "443" {
			port = ""
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return Route{}, errors.New("computer route has an empty port")
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if pin := route.CertFingerprint; pin != "" {
		if !strings.HasPrefix(pin, "sha256:") || len(pin) != 71 || pin != strings.ToLower(pin) {
			return Route{}, errors.New("computer route has an invalid certificate fingerprint")
		}
		if _, err := hex.DecodeString(pin[7:]); err != nil {
			return Route{}, errors.New("computer route has an invalid certificate fingerprint")
		}
	}
	return Route{Endpoint: "https://" + host, CertFingerprint: route.CertFingerprint}, nil
}

// Merge prefers current advertisements and retains older alternatives when a
// release omits the field or a listener temporarily disappears. Trust updates
// for an existing origin must come through the already trusted connection.
func Merge(previous, advertised []Route) []Route {
	out := make([]Route, 0, MaxRoutes)
	for _, rows := range [][]Route{advertised, previous} {
		// Malformed/duplicate input cannot turn this into an unbounded scan.
		if len(rows) > MaxRoutes*8 {
			rows = rows[:MaxRoutes*8]
		}
		for _, candidate := range rows {
			route, err := Normalize(candidate)
			if err != nil {
				continue
			}
			duplicate := false
			for _, held := range out {
				if held.Endpoint == route.Endpoint {
					duplicate = true
					break
				}
			}
			if !duplicate {
				out = append(out, route)
			}
			if len(out) == MaxRoutes {
				return out
			}
		}
	}
	return out
}
