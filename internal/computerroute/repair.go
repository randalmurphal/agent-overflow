package computerroute

import (
	"errors"
	"net/url"
)

// RepairCandidates reuses existing trust at an explicitly entered address.
// A private certificate pin can identify its computer at a new IP. WebPKI
// proves a hostname, so it authorizes a port change for that same hostname,
// never a new domain merely claiming the computer's public backend ID.
// Callers still verify TLS and backend identity before saving any candidate.
func RepairCandidates(primary Route, known []Route, endpoint string) ([]Route, error) {
	target, err := Normalize(Route{Endpoint: endpoint})
	if err != nil {
		return nil, err
	}
	targetURL, _ := url.Parse(target.Endpoint)
	primary, primaryError := Normalize(primary)
	trusted := Merge(nil, known)
	// An advertised replacement pin for the original origin supersedes its
	// old pin. The original address otherwise remains available separately.
	replaced := false
	for _, route := range trusted {
		if route.Endpoint == primary.Endpoint {
			replaced = true
		}
	}
	if primaryError == nil && !replaced {
		trusted = append(trusted, primary)
	}
	var candidates []Route
	for _, route := range trusted {
		origin, _ := url.Parse(route.Endpoint)
		if route.CertFingerprint == "" && origin.Hostname() != targetURL.Hostname() {
			continue
		}
		duplicate := false
		for _, candidate := range candidates {
			if candidate.CertFingerprint == route.CertFingerprint {
				duplicate = true
				break
			}
		}
		if !duplicate {
			candidates = append(candidates, Route{Endpoint: target.Endpoint, CertFingerprint: route.CertFingerprint})
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("this address cannot be verified with the saved computer trust; use a new pairing link from that computer")
	}
	return candidates, nil
}
