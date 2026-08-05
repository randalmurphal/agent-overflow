package devserverprobe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// dialTimeout bounds one connect attempt. Loopback answers in
	// microseconds (the SYN never leaves the kernel: accept or RST is
	// immediate), so the ceiling matters only if a firewall silently
	// drops loopback SYNs.
	dialTimeout = 150 * time.Millisecond

	// This cache is the ONLY verdict memo — the frontend consumer
	// (utils/devServerProbe.ts) deliberately keeps none, so staleness
	// has a single authority. Both TTLs must stay strictly below the
	// frontend's probe cadences (1.5s retry / 5s re-verify) or a
	// scheduled probe is answered from memory instead of the dialer.
	// A dead port re-checks sooner so a server that just started is
	// noticed promptly; a live verdict can serve a remount up to
	// liveTTL after the server died, delaying that chip's
	// disappearance by at most that long.
	liveTTL = 3 * time.Second
	deadTTL = time.Second

	// maxEntries bounds the cache's MEMORY, not its dial rate — keys
	// derive from model/tool-authored command output, so the map must
	// not grow with whatever that output mentions. Concurrency of the
	// dials themselves is bounded by the transport's per-connection
	// RPC cap, not here.
	maxEntries = 64
)

// Prober dials loopback addresses and caches the verdicts.
type Prober struct {
	mu      sync.Mutex
	entries map[string]entry

	// Injection seams for tests; New wires the real dialer and clock.
	dial func(ctx context.Context, address string) (net.Conn, error)
	now  func() time.Time
}

type entry struct {
	live    bool
	expires time.Time
}

// New returns a Prober using the real network dialer and clock.
func New() *Prober {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return &Prober{
		entries: make(map[string]entry),
		dial: func(ctx context.Context, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", address)
		},
		now: time.Now,
	}
}

// Live reports whether something is listening on the URL's loopback
// address. An unreachable port is a false verdict, not an error;
// errors are reserved for input that is not a loopback HTTP(S) URL.
func (p *Prober) Live(ctx context.Context, rawURL string) (bool, error) {
	key, addrs, err := loopbackDialAddrs(rawURL)
	if err != nil {
		return false, err
	}
	if live, ok := p.cached(key); ok {
		return live, nil
	}
	live := false
	for _, addr := range addrs {
		conn, err := p.dial(ctx, addr)
		if err == nil {
			_ = conn.Close()
			live = true
			break
		}
	}
	p.store(key, live)
	return live, nil
}

// loopbackDialAddrs validates rawURL as a loopback HTTP(S) URL and
// returns the cache key plus the dial addresses to try in order. The
// name "localhost" is resolved statically to both loopback literals
// rather than through the system resolver: the dial targets stay
// deterministic and a server bound to only one address family is still
// found.
func loopbackDialAddrs(rawURL string) (key string, addrs []string, err error) {
	value := strings.TrimSpace(rawURL)
	if value == "" {
		return "", nil, errors.New("dev-server URL is required")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", nil, errors.New("invalid dev-server URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", nil, errors.New("dev-server URL must use http or https")
	}
	host := parsed.Hostname()
	if host == "" {
		return "", nil, errors.New("dev-server URL must include a host")
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	lowered := strings.ToLower(host)
	if lowered == "localhost" {
		return "localhost:" + port, []string{
			net.JoinHostPort("127.0.0.1", port),
			net.JoinHostPort("::1", port),
		}, nil
	}
	addr, err := netip.ParseAddr(lowered)
	if err != nil || !addr.IsLoopback() {
		return "", nil, errors.New("dev-server URL host must be loopback")
	}
	// A zone suffix ("::1%eth0") is meaningless on loopback, cannot be
	// produced by triage, and would give attacker-shaped input an
	// unbounded distinct-key space in the verdict cache.
	if addr.Zone() != "" {
		return "", nil, errors.New("dev-server URL host must be loopback")
	}
	target := net.JoinHostPort(addr.String(), port)
	return target, []string{target}, nil
}

func (p *Prober) cached(key string) (bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	if !ok || !p.now().Before(e.expires) {
		return false, false
	}
	return e.live, true
}

func (p *Prober) store(key string, live bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	// An overwrite needs no room — evicting for it would drop an
	// unrelated verdict for nothing.
	if _, exists := p.entries[key]; !exists && len(p.entries) >= maxEntries {
		p.evictLocked(now)
	}
	ttl := deadTTL
	if live {
		ttl = liveTTL
	}
	p.entries[key] = entry{live: live, expires: now.Add(ttl)}
}

// evictLocked drops expired entries, then — if the cache is still at
// capacity — the entry closest to expiry, so a store always has room.
func (p *Prober) evictLocked(now time.Time) {
	for k, e := range p.entries {
		if !now.Before(e.expires) {
			delete(p.entries, k)
		}
	}
	for len(p.entries) >= maxEntries {
		oldestKey := ""
		var oldest time.Time
		for k, e := range p.entries {
			if oldestKey == "" || e.expires.Before(oldest) {
				oldestKey, oldest = k, e.expires
			}
		}
		delete(p.entries, oldestKey)
	}
}
