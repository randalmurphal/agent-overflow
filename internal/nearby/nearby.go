package nearby

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hashicorp/mdns"
	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

const (
	service        = "_agent-overflow._tcp"
	serviceDomain  = service + ".local."
	browseDuration = 2 * time.Second
	maxHosts       = 64
	maxInterfaces  = 16
	maxPackets     = 256
	maxPacketBytes = 8192
)

// Advertisement contains public installation metadata, never pairing secrets.
// Name is read for every query so a rename takes effect without restarting.
type Advertisement struct {
	// Addresses optionally restricts advertisements to the native listener addresses.
	Addresses []string
	BackendID string
	Name      func() string
	Port      int
}

// Host is an untrusted hint. Pairing must independently verify the host and
// receive owner confirmation before granting access or persisting trust.
type Host struct {
	BackendID string `json:"backendId"`
	Name      string `json:"name"`
	Address   string `json:"address"`
}

// Server owns the advertisements on the interfaces present at Start.
// The caller restarts it after network changes and closes it when LAN sharing ends.
type Server struct{ servers []*mdns.Server }

func Start(ad Advertisement) (*Server, error) {
	if !validText(ad.BackendID, 128) || ad.Name == nil || ad.Port < 1 || ad.Port > 65535 {
		return nil, errors.New("invalid nearby advertisement")
	}
	interfaces, err := lanInterfaces()
	if err != nil {
		return nil, err
	}
	result := &Server{}
	var failures []error
	for _, iface := range interfaces {
		ips, err := interfaceIPs(iface)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if len(ad.Addresses) > 0 {
			filtered := ips[:0]
			for _, ip := range ips {
				for _, address := range ad.Addresses {
					if ip.String() == address {
						filtered = append(filtered, ip)
						break
					}
				}
			}
			ips = filtered
		}
		if len(ips) == 0 {
			continue
		}
		hash := sha256.Sum256([]byte(ad.BackendID))
		instance := fmt.Sprintf("ao-%x", hash[:16])
		zone, err := mdns.NewMDNSService(instance, service, "local.", instance+".local.", ad.Port, ips, nil)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		server, err := mdns.NewServer(&mdns.Config{Iface: &iface, Zone: &advertisementZone{zone, ad}, Logger: log.New(io.Discard, "", 0)})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		result.servers = append(result.servers, server)
	}
	if len(result.servers) == 0 {
		return nil, fmt.Errorf("nearby discovery unavailable: %w", errors.Join(append(failures, errors.New("no usable multicast interface"))...))
	}
	return result, nil
}

func (s *Server) Close() error {
	var failures []error
	for _, server := range s.servers {
		failures = append(failures, server.Shutdown())
	}
	return errors.Join(failures...)
}

type advertisementZone struct {
	service *mdns.MDNSService
	ad      Advertisement
}

func (z *advertisementZone) Records(q dns.Question) []dns.RR {
	// Build a query-local copy: the library invokes Zone concurrently for IPv4/6.
	copy := *z.service
	name := z.ad.Name()
	if !validText(name, 200) {
		name = "Agent Overflow"
	}
	copy.TXT = []string{"v=1", "id=" + strings.ReplaceAll(z.ad.BackendID, `\`, `\\`), "name=" + strings.ReplaceAll(name, `\`, `\\`)}
	return copy.Records(q)
}

// Discover performs one bounded scan, returning no retained/background cache.
// A cancelled caller receives its cancellation error; the ordinary scan deadline
// returns the hosts collected so far. Multicast may be blocked by network policy.
func Discover(ctx context.Context) ([]Host, error) {
	caller := ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	interfaces, err := lanInterfaces()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, browseDuration)
	defer cancel()
	var mu sync.Mutex
	hosts := make(map[string]Host)
	var failures []error
	var successful int
	var wg sync.WaitGroup
	for _, iface := range interfaces {
		wg.Go(func() {
			found, err := browseInterface(ctx, iface)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			successful++
			for _, host := range found {
				if len(hosts) < maxHosts {
					hosts[host.BackendID+"\x00"+host.Address] = host
				}
			}
		})
	}
	wg.Wait()
	if caller.Err() != nil {
		return nil, caller.Err()
	}
	if successful == 0 {
		return nil, fmt.Errorf("nearby discovery unavailable: %w", errors.Join(append(failures, errors.New("no usable multicast interface"))...))
	}
	result := make([]Host, 0, len(hosts))
	for _, host := range hosts {
		result = append(result, host)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].Address < result[j].Address
	})
	return result, nil
}

func browseInterface(ctx context.Context, iface net.Interface) ([]Host, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := ipv4.NewPacketConn(conn).SetMulticastInterface(&iface); err != nil {
		return nil, err
	}
	// QU requests a unicast response to this ephemeral socket; browsing does not
	// compete with the operating system's mDNS listener on port 5353.
	query := dns.Msg{Question: []dns.Question{{Name: serviceDomain, Qtype: dns.TypePTR, Qclass: dns.ClassINET | 1<<15}}}
	packet, err := query.Pack()
	if err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}); err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	buffer := make([]byte, maxPacketBytes+1)
	hosts := make(map[string]Host)
	for i := 0; i < maxPackets && len(hosts) < maxHosts; i++ {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				break
			}
			return nil, err
		}
		if n > maxPacketBytes {
			continue
		}
		for _, host := range parseHosts(buffer[:n]) {
			if len(hosts) < maxHosts {
				hosts[host.BackendID+"\x00"+host.Address] = host
			}
		}
	}
	result := make([]Host, 0, len(hosts))
	for _, host := range hosts {
		result = append(result, host)
	}
	return result, nil
}

func parseHosts(packet []byte) []Host {
	if len(packet) < 12 || len(packet) > maxPacketBytes {
		return nil
	}
	// Bound the declared sections before the DNS library allocates them.
	if binary.BigEndian.Uint16(packet[4:6]) > 16 || int(binary.BigEndian.Uint16(packet[6:8]))+int(binary.BigEndian.Uint16(packet[8:10]))+int(binary.BigEndian.Uint16(packet[10:12])) > 128 {
		return nil
	}
	var msg dns.Msg
	if msg.Unpack(packet) != nil || !msg.Response || msg.Opcode != dns.OpcodeQuery || msg.Rcode != dns.RcodeSuccess || msg.Truncated {
		return nil
	}
	records := append(msg.Answer, msg.Extra...)
	if len(records) > 128 {
		return nil
	}
	var result []Host
	for _, record := range records {
		ptr, ok := record.(*dns.PTR)
		if !ok || ptr.Hdr.Name != serviceDomain || ptr.Hdr.Ttl == 0 || !strings.HasSuffix(ptr.Ptr, "."+serviceDomain) {
			continue
		}
		var srv *dns.SRV
		var txt *dns.TXT
		for _, r := range records {
			if r.Header().Name != ptr.Ptr || r.Header().Ttl == 0 {
				continue
			}
			switch r := r.(type) {
			case *dns.SRV:
				srv = r
			case *dns.TXT:
				txt = r
			}
		}
		if srv == nil || srv.Port == 0 || txt == nil {
			continue
		}
		metadata, ok := parseMetadata(txt.Txt)
		if !ok {
			continue
		}
		for _, r := range records {
			if r.Header().Name != srv.Target || r.Header().Ttl == 0 {
				continue
			}
			var ip net.IP
			switch r := r.(type) {
			case *dns.A:
				ip = r.A
			case *dns.AAAA:
				ip = r.AAAA
			default:
				continue
			}
			addr, ok := netip.AddrFromSlice(ip)
			if !ok || !addr.Unmap().IsPrivate() {
				continue
			}
			metadata.Address = "https://" + net.JoinHostPort(addr.Unmap().String(), strconv.Itoa(int(srv.Port)))
			result = append(result, metadata)
			if len(result) == maxHosts {
				return result
			}
		}
	}
	return result
}

func parseMetadata(fields []string) (Host, bool) {
	if len(fields) != 3 {
		return Host{}, false
	}
	values := make(map[string]string, 3)
	for _, field := range fields {
		field, valid := decodeTXT(field)
		if !valid {
			return Host{}, false
		}
		key, value, ok := strings.Cut(field, "=")
		if !ok || len(field) > 255 {
			return Host{}, false
		}
		if _, duplicate := values[key]; duplicate {
			return Host{}, false
		}
		values[key] = value
	}
	if values["v"] != "1" || !validText(values["id"], 128) || !validText(values["name"], 200) {
		return Host{}, false
	}
	return Host{BackendID: values["id"], Name: values["name"]}, true
}

func validText(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

func lanInterfaces() ([]net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make([]net.Interface, 0)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		ips, err := interfaceIPs(iface)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			continue
		}
		result = append(result, iface)
		if len(result) == maxInterfaces {
			break
		}
	}
	return result, nil
}

func interfaceIPs(iface net.Interface) ([]net.IP, error) {
	addresses, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err == nil && prefix.Addr().Is4() && prefix.Addr().IsPrivate() {
			ips = append(ips, net.IP(prefix.Addr().AsSlice()))
		}
	}
	return ips, nil
}

// miekg/dns exposes TXT using DNS presentation escapes (including decimal
// byte escapes for UTF-8), rather than returning the raw UTF-8 bytes.
func decodeTXT(value string) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			out.WriteByte(value[i])
			continue
		}
		i++
		if i >= len(value) {
			return "", false
		}
		if value[i] >= '0' && value[i] <= '9' {
			if i+3 > len(value) {
				return "", false
			}
			n, err := strconv.ParseUint(value[i:i+3], 10, 8)
			if err != nil {
				return "", false
			}
			out.WriteByte(byte(n))
			i += 2
		} else {
			out.WriteByte(value[i])
		}
	}
	return out.String(), true
}
