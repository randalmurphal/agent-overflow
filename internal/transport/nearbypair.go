package transport

import (
	"context"
	"encoding/json"
	"net/http"

	"agent-overflow/internal/pairbootstrap"
)

// NearbyPairPath carries only the unauthenticated, committed-key bootstrap.
// Session enrollment still uses AuthPairPath after the sealed invitation opens.
const NearbyPairPath = "/auth/pair/nearby"

type nearbyPairEndpoints interface {
	NearbyPair(context.Context, string, pairbootstrap.Request) (any, error)
}

func (s *Server) handleNearbyPair(w http.ResponseWriter, r *http.Request) {
	endpoints, ok := s.cfg.AuthEndpoints.(nearbyPairEndpoints)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !s.acceptAuthPost(w, r) {
		return
	}
	var req pairbootstrap.Request
	if !decodeAuthBody(w, r, &req) {
		return
	}
	result, err := endpoints.NearbyPair(r.Context(), r.Host, req)
	if err != nil {
		writeAuthResult(w, s.csp, TokenGrant{}, "pairing_unavailable")
		return
	}
	WriteSecurityHeaders(w.Header(), s.csp)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
