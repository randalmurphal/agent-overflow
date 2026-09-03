package transport

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// The bundle routes (docs/specs/remote-access.md §9, "Bundle sync: the
// backend is the phone's update server", wave 6g-a).
//
// Two reads, and between them they are the whole update channel for the
// one client that carries a bundle of its own:
//
//	GET /bundle/manifest.json   what this backend's SPA is, file by file
//	GET /bundle/archive.zip     exactly those files, deflated
//
// `internal/bundle` produces both and this file serves them. The hello
// frame advertises the id, so a shell knows whether to ask at all before
// it issues a request.
//
// **The credential is the paired session, not the page cookie.** Both
// routes run exactly the check `/bootstrap.json` falls back to — a live
// session credential in `X-AO-Session` plus the device proof its
// enrolment bound (`sessionAdmitsRequest`). The page cookie is
// deliberately NOT enough, and that is a property of the consumer rather
// than a hardening choice: the consumer is a shell at
// `https://shell.agent-overflow.invalid`, which holds no cookie for this
// backend's origin and could not be sent one. Admitting the cookie as
// well would mean the routes could be reached by something whose
// credential this surface cannot revoke.
//
// A caller with no session gets the same unfingerprintable 404 an
// unpaired remote gets at the manifest — including a loopback page,
// which is not an exception carved out for it but the plain consequence
// of the rule. Nothing on this listener says whether a path exists.
//
// **The tier rule, recorded where the gate would go.** The spec states
// that only OWNER-TIER backends may supply bundles: peer and hub
// connections never push executable content and are served by capability
// flags instead, because one misbehaving owner-tier backend can reach
// the phone's device key and its other backends' credentials through an
// update. Every paired session this backend can issue today is
// owner-tier — there is no peer or hub session in the identity core yet
// — so there is nothing here to compare and no gate is written. When
// those tiers exist, THIS is where the comparison belongs: one check in
// `bundleSessionAdmits` below, beside the session lookup, rather than a
// tier test sprinkled through two handlers.
//
// **CORS is the shell's, exactly as the attachment routes have it**
// (shellorigin.go). Both patterns are method-qualified so a write verb
// is the mux's 405 rather than something a handler has to consider,
// which means each registers its own OPTIONS pattern: without it the mux
// answers 405 to the preflight and the browser never sends the real
// request.

// BundleManifestPath serves the SHA-256 manifest of this backend's SPA.
//
// A separate route from `/bootstrap.json` even though both are JSON a
// client reads at connect time, because their credentials and their
// lifetimes differ: the manifest here is only ever read by a paired
// shell, is large enough to be worth not sending to every page load, and
// changes only when the binary does.
const BundleManifestPath = "GET /bundle/manifest.json"

// BundleManifestPreflightPath is that pattern for OPTIONS; see the header.
const BundleManifestPreflightPath = "OPTIONS /bundle/manifest.json"

// BundleArchivePath serves the deflated zip of exactly the manifest's
// files.
const BundleArchivePath = "GET /bundle/archive.zip"

// BundleArchivePreflightPath is that pattern for OPTIONS.
const BundleArchivePreflightPath = "OPTIONS /bundle/archive.zip"

// bundleSessionAdmits is the one admission both routes run.
//
// Factored out rather than inlined twice so the tier comparison the spec
// describes has one place to land, and so "the manifest and the archive
// are reached by the same credential" is a fact about the code rather
// than about two handlers that happen to agree.
func (s *Server) bundleSessionAdmits(r *http.Request) bool {
	return s.sessionAdmitsRequest(r)
}

// handleBundleManifest answers BundleManifestPath.
func (s *Server) handleBundleManifest(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Bundle == nil || !s.bundleSessionAdmits(r) {
		http.NotFound(w, r)
		return
	}
	manifest, err := s.cfg.Bundle.Manifest()
	if err != nil {
		// Our own bookkeeping failing, not a caller being refused: an
		// operator gets the reason in the log, and the wire gets the same
		// 404 every other refusal on this listener gets.
		log.Printf("transport: bundle manifest: %v", err)
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	// no-store rather than a validator: the document is small, it is read
	// once per hello that disagrees with the phone's state, and a cached
	// copy would let a shell decide against a manifest the backend has
	// already replaced.
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(manifest)
}

// handleBundleArchive answers BundleArchivePath.
//
// http.ServeContent over the cached bytes, for the reason the attachment
// download uses it: Range and the conditional statuses come free from
// one seekable reader, and a phone that lost a 5 MB transfer at 90% can
// resume it instead of paying for the whole thing again. Nothing is
// copied — the reader is a view over the one archive this process built.
func (s *Server) handleBundleArchive(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Bundle == nil || !s.bundleSessionAdmits(r) {
		http.NotFound(w, r)
		return
	}
	archive, err := s.cfg.Bundle.Archive()
	if err != nil {
		log.Printf("transport: bundle archive: %v", err)
		http.NotFound(w, r)
		return
	}
	// The same window the attachment bytes get, for the same arithmetic:
	// the server's 60s write timeout is right for an RPC and wrong for
	// megabytes. Sized from the archive's own length, the way an upload
	// is sized from its ticket.
	extendTransferDeadline(w, AttachmentTransferWindowFor(int64(len(archive))))
	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/zip")
	// The zero ModTime withholds Last-Modified, which is right: the
	// archive carries no timestamps by construction, and inventing one
	// would invite a shell to revalidate against a clock rather than
	// against the id the hello frame already gave it.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(archive))
}
