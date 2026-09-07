package nativenetwork

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

const maxConnections = 64

// Relay preserves TLS bytes and never injects credentials or HTTP headers.
// Its upstream is always a non-loopback WSL address: forwarding to localhost
// would let an off-host peer acquire local-only authorization semantics.
type Relay struct {
	listeners []net.Listener
	addresses []string
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	slots     chan struct{}
	target    string
	once      sync.Once
	errMu     sync.Mutex
	err       error
}

func StartRelay(ctx context.Context, target string, addresses []string) (*Relay, error) {
	destination, port, err := relayTarget(target)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 || len(addresses) > 16 {
		return nil, errors.New("no usable Windows LAN interface")
	}
	ctx, cancel := context.WithCancel(ctx)
	relay := &Relay{target: destination, cancel: cancel, slots: make(chan struct{}, maxConnections)}
	var failures []error
	seen := make(map[string]bool)
	for _, address := range addresses {
		ip, err := netip.ParseAddr(address)
		if err != nil || !ip.Is4() || !ip.IsPrivate() {
			failures = append(failures, fmt.Errorf("invalid Windows LAN address %q", address))
			continue
		}
		if seen[address] {
			continue
		}
		seen[address] = true
		listener, err := net.Listen("tcp4", net.JoinHostPort(address, port))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		relay.listeners = append(relay.listeners, listener)
		relay.addresses = append(relay.addresses, "https://"+net.JoinHostPort(address, port))
		relay.wg.Go(func() { relay.accept(ctx, listener) })
	}
	if len(relay.listeners) == 0 {
		cancel()
		return nil, fmt.Errorf("Windows LAN listener unavailable: %w", errors.Join(failures...))
	}
	sort.Strings(relay.addresses)
	relay.errMu.Lock()
	relay.err = errors.Join(append(failures, relay.err)...)
	relay.errMu.Unlock()
	return relay, nil
}

func relayTarget(target string) (string, string, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", "", errors.New("invalid WSL LAN target")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	port, portErr := strconv.Atoi(u.Port())
	if err != nil || !ip.Is4() || !ip.IsPrivate() || portErr != nil || port < 1 || port > 65535 {
		return "", "", errors.New("WSL LAN target must use a private, non-loopback IPv4 address and port")
	}
	return net.JoinHostPort(ip.String(), u.Port()), u.Port(), nil
}

func (r *Relay) Addresses() []string { return append([]string(nil), r.addresses...) }
func (r *Relay) Close() error {
	r.once.Do(func() {
		r.cancel()
		for _, listener := range r.listeners {
			listener.Close()
		}
		r.wg.Wait()
	})
	return nil
}
func (r *Relay) accept(ctx context.Context, listener net.Listener) {
	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				r.errMu.Lock()
				r.err = errors.Join(r.err, err)
				r.errMu.Unlock()
			}
			return
		}
		select {
		case r.slots <- struct{}{}:
		default:
			client.Close()
			continue
		}
		r.wg.Go(func() { defer func() { <-r.slots }(); r.carry(ctx, client) })
	}
}
func (r *Relay) carry(ctx context.Context, client net.Conn) {
	defer client.Close()
	upstream, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp4", r.target)
	if err != nil {
		return
	}
	defer upstream.Close()
	stop := context.AfterFunc(ctx, func() { client.Close(); upstream.Close() })
	defer stop()
	done := make(chan struct{}, 1)
	go func() {
		// An EOF is a half-close: the host may still be producing its response.
		io.Copy(upstream, client)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	io.Copy(client, upstream)
	if tcp, ok := client.(*net.TCPConn); ok {
		tcp.CloseWrite()
	}
	<-done
}

// LANAddresses excludes loopback, point-to-point VPNs and Windows virtual
// adapters. Binding a virtual adapter would advertise unreachable WSL/NAT IPs.
func LANAddresses() ([]string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 || !physicalInterface(iface) {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err == nil && prefix.Addr().Is4() && prefix.Addr().IsPrivate() {
				result = append(result, prefix.Addr().String())
				if len(result) == 16 {
					sort.Strings(result)
					return result, nil
				}
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func (r *Relay) Err() error { r.errMu.Lock(); defer r.errMu.Unlock(); return r.err }
