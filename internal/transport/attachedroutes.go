package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"agent-overflow/internal/loopback"
)

// The attached-backend routes: how one page reaches machines that are not
// this one (docs/specs/remote-access.md §10).
//
// The desktop's answer to "show me every machine at once" is not a second
// window and not a cross-origin fetch. The local backend serves one
// carried socket per attached machine ON ITS OWN LISTENER, so every
// backend the page talks to is same-origin, the page holds exactly one
// credential, and the pinned device session for each remote machine never
// leaves this process. The phone realizes the same seam differently — the
// SPA opens those sockets itself — which is why the manifest names the
// URLs rather than the SPA composing them.
//
// Three routes, all subtree patterns so the id is read from the path here
// rather than by a mux wildcard, and all registered only when
// Config.AttachedBackends is set:
//
//	/ws/backend/<id>        the carried WebSocket
//	/bootstrap/<id>.json    that backend's manifest, wsUrl rewritten here
//	/backend/<id>/attachments/…  that backend's attachment bytes
//
// # Who may reach them
//
// The loopback PEER only, plus the page credential and the origin rule.
// Tighter than /ws deliberately, and it is the spec's own shape rather
// than caution: an off-host client realizes this seam by opening its own
// sockets to each backend with its own paired session. Carrying it
// through here instead would lend a device THIS machine's pinned
// credential for a machine it never paired with, and would make one
// revocation on the far backend mean nothing to the client that is
// actually driving it.
//
// # What this code is
//
// Triage and a pipe. Route registration, the id, three admission checks,
// and a manifest whose wsUrl is rewritten to name this listener. Every
// credential decision belongs to the carrier behind the seam, and the
// merging of what several backends say belongs to the SPA.

const (
	// AttachedWSPrefix is the subtree the carried upgrades live under.
	// The upstream's own /ws is what the far side sees; this prefix is
	// this listener's addressing and is never sent anywhere.
	AttachedWSPrefix = "/ws/backend/"

	// AttachedBootstrapPrefix is the subtree the per-backend manifests
	// live under. A subtree rather than a `{id}.json` wildcard because a
	// ServeMux wildcard must be a whole path segment — the `.json`
	// suffix a page requests is trimmed here instead.
	AttachedBootstrapPrefix = "/bootstrap/"

	// AttachedTransferPrefix is the subtree the per-backend attachment
	// bytes live under. One prefix for the pair of upstream routes, for
	// the same reason internal/clientmode uses one: the query carries
	// their whole admission and the path names the attachment, so the
	// only thing this listener adds is which backend.
	AttachedTransferPrefix = "/backend/"

	// attachedTransferSegment is what must follow the backend id under
	// AttachedTransferPrefix. Nothing else is carried: the subtree exists
	// for the upstream's attachment routes and for nothing else, so a
	// path naming something else is a 404 rather than a hop.
	attachedTransferSegment = "attachments/"

	// attachedBootstrapSuffix is the spelling a page asks for. Optional
	// on the way in and never required, because the manifest publishes
	// the exact URL and no client composes one.
	attachedBootstrapSuffix = ".json"
)

// AttachedBackends is the set of machines this installation has attached,
// as the transport uses them. Declared here and satisfied by
// internal/attachedbackends, the same direction as CDPTunnelEndpoint: the
// transport never learns what a pairing is, what a device key is, or how
// a session rotates — it asks these two questions and nothing else.
//
// Optional. Nil means this backend attaches to nothing, the three routes
// are not registered, and the manifest carries no backends array.
type AttachedBackends interface {
	// Attached lists the profiles this installation holds, in a stable
	// order. Ids and labels only: the URLs the manifest publishes belong
	// to this listener and are derived per request from the Host header,
	// exactly as wsUrl is.
	Attached() []AttachedProfile

	// Carrier returns the hop for one profile id, or nil when this
	// installation holds no such profile. Nil is an ordinary answer — a
	// page whose manifest is a moment stale asks for a backend that has
	// just been removed — and it is answered with the same 404 a bad
	// credential gets.
	Carrier(id string) BackendCarrier
}

// AttachedProfile is one attached machine as the manifest names it.
type AttachedProfile struct {
	// ID is this installation's own name for the profile, and the id in
	// every path above.
	ID string

	// BackendID is what that machine called itself when this device
	// paired with it. The client keys its per-backend replica by it, so
	// it is the identity; ID is only an address on this listener.
	BackendID string

	// Name is what to show a person: the owner's nickname when they set
	// one, else the machine's own name, else its address. Display only.
	Name string
}

// BackendCarrier is one attached machine's hop.
type BackendCarrier interface {
	// Manifest asks that backend what it says about itself, with the
	// credential this installation holds for it.
	Manifest(ctx context.Context) (AttachedManifest, error)

	// CarryUpgrade and CarryTransfer carry one already-admitted request.
	CarryUpgrade(w http.ResponseWriter, r *http.Request)
	CarryTransfer(w http.ResponseWriter, r *http.Request)
}

// AttachedManifest is what an attached backend answered about itself, in
// the fields a page may believe about a machine that is not this one.
//
// A closed list rather than the far side's whole manifest, because most
// of what a manifest says describes the LISTENER a page loaded from:
// harness mode, the page marker, whether a passkey ceremony can start
// here. Forwarding those would let one machine's configuration answer for
// another's page. What remains is what identifies the backend's history
// store, what to call it, and which launch this is.
type AttachedManifest struct {
	BackendID         string
	ReplicaGeneration string
	BackendName       string
	LaunchID          string
}

// AttachedBackendEntry is one row of the page manifest's backends array.
// Every URL is absolute-path and same-origin: the page opens them with
// its own credential, and this listener is the only thing that holds the
// credential for the machine behind them.
type AttachedBackendEntry struct {
	ID        string `json:"id"`
	BackendID string `json:"backendId,omitempty"`
	Name      string `json:"name,omitempty"`
	// WSURL is absolute because a WebSocket needs a scheme, and derived
	// from the request's Host for the same reason the page's own wsUrl is.
	WSURL string `json:"wsUrl"`
	// BootstrapURL is a same-origin path, which is what a fetch takes and
	// what the page's own manifest is fetched as. Published rather than
	// composed by the SPA so the desktop and the phone can spell this
	// seam differently without the page knowing which it is on.
	BootstrapURL string `json:"bootstrapUrl"`
}

// attachedBackendEntries builds the manifest's backends array for one
// request. Per request because wsUrl is derived from the request's own
// Host header — the same rule DeriveWSURL follows, and the reason the SPA
// can require a same-origin wsUrl on every path with no exemptions.
func (s *Server) attachedBackendEntries(r *http.Request) []AttachedBackendEntry {
	if s.cfg.AttachedBackends == nil {
		return nil
	}
	profiles := s.cfg.AttachedBackends.Attached()
	if len(profiles) == 0 {
		return nil
	}
	wsOrigin := deriveWSURL(r)
	wsOrigin = strings.TrimSuffix(wsOrigin, WSPath)
	entries := make([]AttachedBackendEntry, 0, len(profiles))
	for _, profile := range profiles {
		entries = append(entries, AttachedBackendEntry{
			ID:           profile.ID,
			BackendID:    profile.BackendID,
			Name:         profile.Name,
			WSURL:        wsOrigin + AttachedWSPrefix + profile.ID,
			BootstrapURL: AttachedBootstrapPrefix + profile.ID + attachedBootstrapSuffix,
		})
	}
	return entries
}

// attachedCarrier resolves the hop a request names, after checking that
// the request may reach one at all.
//
// One function for all three routes, because the answer to "may this
// caller use another machine's credential" cannot be allowed to differ
// between them. Every refusal is http.NotFound, so a caller cannot tell
// an unattached backend from one it may not reach from a path that does
// not exist.
func (s *Server) attachedCarrier(w http.ResponseWriter, r *http.Request, id string) BackendCarrier {
	if s.cfg.AttachedBackends == nil || id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return nil
	}
	// The peer rule, and it is the whole reason these routes are narrower
	// than /ws. See the file comment.
	if !loopback.PeerAddress(r.RemoteAddr) {
		http.NotFound(w, r)
		return nil
	}
	// Read the live (post-rebind) allow-list, not Config's static value,
	// for the same reason the upgrader does.
	if !OriginAllowed(r, s.currentOriginPatterns()) {
		http.NotFound(w, r)
		return nil
	}
	// The page credential and nothing else. A durable session admits /ws
	// and the manifest because a paired device legitimately holds one;
	// here it must not, because a session is exactly what an off-host
	// client presents and these routes are not for one.
	if !s.cred.Authenticate(r) {
		http.NotFound(w, r)
		return nil
	}
	carrier := s.cfg.AttachedBackends.Carrier(id)
	if carrier == nil {
		http.NotFound(w, r)
		return nil
	}
	return carrier
}

// handleAttachedWS carries one page socket to an attached backend.
func (s *Server) handleAttachedWS(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, AttachedWSPrefix)
	carrier := s.attachedCarrier(w, r, id)
	if carrier == nil {
		return
	}
	// Tracked BEFORE the carry so a hop in flight also blocks Shutdown,
	// the same rule handleWS follows for its own upgrade.
	s.wg.Add(1)
	defer s.wg.Done()
	carrier.CarryUpgrade(w, r)
}

// handleAttachedTransfer carries one attachment body to or from an
// attached backend. The ticket on the query is the far side's whole
// admission and was minted by a carried RPC; this listener contributes
// only which backend.
func (s *Server) handleAttachedTransfer(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, AttachedTransferPrefix)
	id, tail, _ := strings.Cut(rest, "/")
	if !strings.HasPrefix(tail, attachedTransferSegment) {
		http.NotFound(w, r)
		return
	}
	carrier := s.attachedCarrier(w, r, id)
	if carrier == nil {
		return
	}
	s.wg.Add(1)
	defer s.wg.Done()
	carrier.CarryTransfer(w, r)
}

// handleAttachedBootstrap answers one attached backend's manifest.
//
// The wsUrl is THIS listener's, because that is where the page's socket
// goes and the only wsUrl the SPA accepts. Everything else is the far
// side's own answer, narrowed to AttachedManifest's closed list on the
// way through.
//
// No ticket exchange here, unlike /bootstrap.json: the page already holds
// this listener's cookie — it could not have reached this route without
// it — and a second exchange would be a second credential for the same
// origin.
func (s *Server) handleAttachedBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, AttachedBootstrapPrefix), attachedBootstrapSuffix)
	carrier := s.attachedCarrier(w, r, id)
	if carrier == nil {
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store, max-age=0")
	WriteSecurityHeaders(h, s.csp)
	manifest, err := carrier.Manifest(r.Context())
	if err != nil {
		// Unreachable, or a credential that machine no longer honours.
		// Both are transient in shape here: the SPA's per-backend
		// reconnect ladder owns the retry, and a machine that is simply
		// asleep must not read as one that has forgotten this device.
		h.Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "backend unreachable", http.StatusServiceUnavailable)
		return
	}
	h.Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	wsOrigin := strings.TrimSuffix(deriveWSURL(r), WSPath)
	_ = json.NewEncoder(w).Encode(Bootstrap{
		WSURL:             wsOrigin + AttachedWSPrefix + id,
		LaunchID:          manifest.LaunchID,
		BackendID:         manifest.BackendID,
		ReplicaGeneration: manifest.ReplicaGeneration,
		BackendName:       manifest.BackendName,
		// Always: the page loaded from THIS backend, and every backend
		// reached through this route is a different one. What the SPA
		// gates on the bit is whether the machine doing the work is the
		// page's own, and here it never is.
		Remote: true,
	})
}
