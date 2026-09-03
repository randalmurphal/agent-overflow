package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"agent-overflow/internal/acmecert"
	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
)

// The canonical domain's certificate: where it comes from, when it is
// renewed, and what the settings screen is told about it
// (docs/specs/remote-access.md §7, path 1).
//
// One goroutine owns the whole question. It reconciles what the user
// configured against what is on disk, publishes the result into the
// transport's certificate source (which resolves per handshake, so a
// renewal takes effect without a rebind), and sleeps until the next
// check. Everything else — boot, a settings write, the "Renew now"
// button — kicks it and reads the status it left behind.
//
// Why not do this in the RPC that changed the setting: a DNS-01 exchange
// waits for a record to propagate and for the authority to look at it,
// which outlives any call timeout the frontend has. The screen polls the
// status instead, which is also what a renewal three months from now
// needs, when there is no call to attach to.

const (
	// domainCertCheckInterval is the quiet cadence. Certificates are
	// renewed 30 days before expiry (acmecert.RenewWindow), so a daily
	// look is thirty chances to notice before anything breaks, at the
	// cost of one file stat.
	domainCertCheckInterval = 24 * time.Hour

	// domainCertRetryFloor and domainCertRetryCeiling bound the backoff
	// after a failed issuance. A DNS hook that is broken now is usually
	// broken in five minutes too, and the certificate authority counts
	// failed orders against a rate limit — so retries slow down, and the
	// certificate already loaded keeps serving throughout.
	domainCertRetryFloor   = 5 * time.Minute
	domainCertRetryCeiling = 6 * time.Hour

	// domainCertIssueTimeout bounds ONE issuance attempt end to end. A
	// DNS-01 validation that has not finished inside this is not going
	// to; the loop reports it and retries on the backoff above.
	domainCertIssueTimeout = 15 * time.Minute
)

// domainCertState is everything the loop and the status read share. Its
// zero value is an App with no loop running, which is what every test
// fixture that builds *App directly gets.
type domainCertState struct {
	mu sync.Mutex

	// dir is the config root the account key and certificate live in.
	// Empty means the loop never started.
	dir string

	// kick wakes the loop. Buffered depth 1: a second kick while one is
	// pending is the same request.
	kick chan struct{}

	// kind is network.TLSServingACME or TLSServingExternal while a
	// certificate for the canonical domain is published, empty otherwise.
	kind string

	// notAfter is that certificate's expiry.
	notAfter time.Time

	// lastErr is the last failure, verbatim and naming its stage, cleared
	// by the next success. Errors are user-facing state (root AGENTS.md
	// principle 5), so this is what the screen renders.
	lastErr string

	// renewing is true while an issuance is in flight, so the screen can
	// say so and poll rather than look idle for several minutes.
	renewing bool

	// failures counts consecutive failed issuances, for the backoff.
	failures int

	// externalStamp identifies the external pair currently loaded, so a
	// tick that finds the same bytes does not re-read them. A file's
	// (size, modtime) pair is what a renewal by an outside tool changes;
	// this is deliberately not a watch, because an external certificate
	// is renewed monthly at most and a watch is a descriptor plus a
	// wake-up per unrelated write in that directory.
	externalStamp string
}

// startDomainCertificateLoop launches the reconciler. Called once from
// Start with the config root. A second call is a no-op, so a fixture
// that runs Start twice cannot fan out goroutines.
func (a *App) startDomainCertificateLoop(dir string) {
	a.domainCert.mu.Lock()
	if a.domainCert.dir != "" {
		a.domainCert.mu.Unlock()
		return
	}
	a.domainCert.dir = dir
	a.domainCert.kick = make(chan struct{}, 1)
	kick := a.domainCert.kick
	a.domainCert.mu.Unlock()

	ctx := a.lifeCtx()
	go func() {
		for {
			wait := a.reconcileDomainCertificate(ctx)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-kick:
				timer.Stop()
			case <-timer.C:
			}
		}
	}()
}

// kickDomainCertificate asks the loop to reconcile now. Never blocks and
// never fails: a kick with no loop running is a no-op, which is the
// state of every App built without Start.
func (a *App) kickDomainCertificate() bool {
	a.domainCert.mu.Lock()
	kick := a.domainCert.kick
	a.domainCert.mu.Unlock()
	if kick == nil {
		return false
	}
	select {
	case kick <- struct{}{}:
	default:
	}
	return true
}

// reconcileDomainCertificate makes the listener present what the user
// configured, and answers how long to wait before looking again.
//
// The order of the branches is the policy: an external pair WINS over
// issuance, so a user who already holds a certificate is never made to
// obtain another one, and the certificate authority is never asked for a
// name somebody else already answers for.
func (a *App) reconcileDomainCertificate(ctx context.Context) time.Duration {
	cfg := a.currentSettings().Network
	source := a.certificateSource()

	switch {
	case cfg.CanonicalDomain == "":
		// Nothing is configured, so nothing is served for a domain. The
		// self-signed certificate underneath is untouched: it is a
		// different question, answered at boot.
		//
		// The standing failure is cleared HERE, and not inside
		// publishDomainCertificate, because what makes it stale is the
		// CONFIGURATION and not the publish. A hook that could not answer,
		// an external pair that would not load, a domain a certificate was
		// never obtained for: every one of those records a failure and none
		// of them publishes, so the failure outlived the domain it was about
		// and Settings kept showing an error for a name nothing was trying
		// to serve any more. Both arms below configure no certificate
		// source, which is the honest end of any failure recorded by one.
		a.publishDomainCertificate(source, "", "", tls.Certificate{}, time.Time{})
		a.clearDomainCertFailure()
		return domainCertCheckInterval
	case cfg.HasExternalPair():
		return a.reconcileExternalCertificate(cfg, source)
	case len(cfg.ACMEDNSHook) > 0:
		return a.reconcileIssuedCertificate(ctx, cfg, source)
	default:
		// A named backend with no certificate source of its own. This is
		// the deployment where something in front terminates TLS for the
		// name; the listener answers to it (transport's Host guard) and
		// presents its self-signed certificate to anything that speaks
		// TLS directly. Same clear as the first arm, for the same reason:
		// this deployment obtains nothing, so nothing here can still be
		// failing.
		a.publishDomainCertificate(source, "", "", tls.Certificate{}, time.Time{})
		a.clearDomainCertFailure()
		return domainCertCheckInterval
	}
}

// reconcileExternalCertificate loads the user's own pair when its bytes
// have changed since the last look, and reports a load failure as
// user-facing state rather than dropping the certificate that is already
// serving.
func (a *App) reconcileExternalCertificate(cfg settings.NetworkSettings, source certificatePublisher) time.Duration {
	stamp := fileStamp(cfg.ExternalCertFile) + "|" + fileStamp(cfg.ExternalKeyFile)

	a.domainCert.mu.Lock()
	unchanged := a.domainCert.kind == network.TLSServingExternal && a.domainCert.externalStamp == stamp
	a.domainCert.mu.Unlock()
	if unchanged {
		return domainCertCheckInterval
	}

	material, err := acmecert.LoadPair(cfg.ExternalCertFile, cfg.ExternalKeyFile)
	if err != nil {
		a.recordDomainCertFailure(fmt.Sprintf("%v", err))
		return domainCertCheckInterval
	}
	if !material.Covers(cfg.CanonicalDomain) {
		a.recordDomainCertFailure(fmt.Sprintf(
			"the certificate in %s is not valid for %s", cfg.ExternalCertFile, cfg.CanonicalDomain))
		return domainCertCheckInterval
	}
	a.publishDomainCertificate(source, cfg.CanonicalDomain, network.TLSServingExternal, material.Certificate, material.NotAfter)
	a.domainCert.mu.Lock()
	a.domainCert.externalStamp = stamp
	a.domainCert.mu.Unlock()
	log.Printf("transport: serving %s with the configured certificate from %s (expires %s)",
		cfg.CanonicalDomain, cfg.ExternalCertFile, material.NotAfter.Format(time.RFC3339))
	return domainCertCheckInterval
}

// reconcileIssuedCertificate serves what was already obtained, and
// orders a new one when there is none or it is inside the renewal
// window. The certificate already loaded keeps serving while an order
// runs and if that order fails.
func (a *App) reconcileIssuedCertificate(
	ctx context.Context,
	cfg settings.NetworkSettings,
	source certificatePublisher,
) time.Duration {
	dir := a.domainCertDir()
	if dir == "" {
		a.recordDomainCertFailure("no configuration directory, so there is nowhere to keep a certificate")
		return domainCertCheckInterval
	}

	material, err := acmecert.Load(dir)
	if err != nil {
		// Material that exists and cannot be read is NOT replaced by
		// ordering over it: every order spends the authority's rate
		// limit, and a truncated file would spend one per check.
		a.recordDomainCertFailure(fmt.Sprintf("%v", err))
		return domainCertCheckInterval
	}
	if material.Loaded() && material.Covers(cfg.CanonicalDomain) {
		a.publishDomainCertificate(source, cfg.CanonicalDomain, network.TLSServingACME, material.Certificate, material.NotAfter)
	}
	if !material.NeedsRenewal(cfg.CanonicalDomain, time.Now()) {
		a.clearDomainCertFailure()
		return domainCertCheckInterval
	}

	issuer, err := acmecert.New(acmecert.Config{
		Dir:    dir,
		Domain: cfg.CanonicalDomain,
		Hook:   cfg.ACMEDNSHook,
	})
	if err != nil {
		a.recordDomainCertFailure(fmt.Sprintf("%v", err))
		return a.domainCertRetryDelay()
	}

	a.setDomainCertRenewing(true)
	issueCtx, cancel := context.WithTimeout(ctx, domainCertIssueTimeout)
	issued, err := issuer.Issue(issueCtx)
	cancel()
	a.setDomainCertRenewing(false)
	if err != nil {
		a.recordDomainCertFailure(fmt.Sprintf("%v", err))
		return a.domainCertRetryDelay()
	}
	a.publishDomainCertificate(source, cfg.CanonicalDomain, network.TLSServingACME, issued.Certificate, issued.NotAfter)
	return domainCertCheckInterval
}

// certificatePublisher is the half of transport.CertificateSource this
// file uses. Declared here so a test can observe what was published
// without building a listener.
type certificatePublisher interface {
	SetDomain(name string, cert *tls.Certificate)
}

// certificateSource returns the transport's certificate holder, or nil
// when there is no transport (a test App, or a boot that resolved none).
func (a *App) certificateSource() certificatePublisher {
	srv := a.transportServer.Load()
	if srv == nil {
		return nil
	}
	source := srv.Certificates()
	if source == nil {
		return nil
	}
	return source
}

// publishDomainCertificate installs (or clears) the certificate the
// canonical domain is served with and records what the status reads
// back. An empty kind clears both.
//
// The NAME is the caller's — the domain the certificate was loaded or
// obtained FOR, carried down from the settings this reconcile pass read.
// Re-reading it here would let a settings write that landed during a
// minutes-long issuance publish the old certificate under the new name,
// which is the one thing SNI resolution must never be handed.
func (a *App) publishDomainCertificate(
	source certificatePublisher,
	domain string,
	kind string,
	cert tls.Certificate,
	notAfter time.Time,
) {
	var published *tls.Certificate
	if kind == "" {
		domain = ""
	} else {
		published = &cert
	}
	if source != nil {
		source.SetDomain(domain, published)
	}

	a.domainCert.mu.Lock()
	defer a.domainCert.mu.Unlock()
	a.domainCert.kind = kind
	a.domainCert.notAfter = notAfter
	if kind == "" {
		a.domainCert.externalStamp = ""
	} else {
		a.domainCert.lastErr = ""
		a.domainCert.failures = 0
	}
}

func (a *App) recordDomainCertFailure(message string) {
	log.Printf("transport: canonical domain certificate: %s", message)
	a.domainCert.mu.Lock()
	defer a.domainCert.mu.Unlock()
	a.domainCert.lastErr = message
	a.domainCert.failures++
}

func (a *App) clearDomainCertFailure() {
	a.domainCert.mu.Lock()
	defer a.domainCert.mu.Unlock()
	a.domainCert.lastErr = ""
	a.domainCert.failures = 0
}

func (a *App) setDomainCertRenewing(renewing bool) {
	a.domainCert.mu.Lock()
	defer a.domainCert.mu.Unlock()
	a.domainCert.renewing = renewing
}

func (a *App) domainCertDir() string {
	a.domainCert.mu.Lock()
	defer a.domainCert.mu.Unlock()
	return a.domainCert.dir
}

// domainCertRetryDelay doubles from the floor to the ceiling with the
// consecutive-failure count.
func (a *App) domainCertRetryDelay() time.Duration {
	a.domainCert.mu.Lock()
	failures := a.domainCert.failures
	a.domainCert.mu.Unlock()
	delay := domainCertRetryFloor
	for i := 1; i < failures && delay < domainCertRetryCeiling; i++ {
		delay *= 2
	}
	if delay > domainCertRetryCeiling {
		delay = domainCertRetryCeiling
	}
	return delay
}

// domainCertStatus is the read-only half the settings screen shows.
func (a *App) domainCertStatus() network.TLSStatus {
	a.domainCert.mu.Lock()
	defer a.domainCert.mu.Unlock()
	status := network.TLSStatus{
		Serving:               a.domainCert.kind,
		Renewing:              a.domainCert.renewing,
		LastError:             a.domainCert.lastErr,
		SelfSignedFingerprint: a.certFingerprint,
	}
	if !a.domainCert.notAfter.IsZero() {
		status.NotAfter = a.domainCert.notAfter.UnixMilli()
	}
	if status.Serving == "" {
		// No certificate for the domain. What the listener presents to
		// anything that speaks TLS to it is the install's own, when the
		// boot resolved one.
		status.Serving = network.TLSServingNone
		if a.certFingerprint != "" {
			status.Serving = network.TLSServingSelfSigned
		}
	}
	return status
}

// fileStamp identifies a file's current bytes cheaply: size and
// modification time. An unreadable file stamps as empty, which differs
// from every readable one and so reads as "changed" on the next look.
func fileStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d@%d", info.Size(), info.ModTime().UnixNano())
}
