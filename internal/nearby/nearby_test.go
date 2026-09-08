package nearby

import (
	"net"
	"strings"
	"testing"

	"github.com/hashicorp/mdns"
	"github.com/miekg/dns"
)

func announcement(t *testing.T, name func() string) []dns.RR {
	t.Helper()
	service, err := mdns.NewMDNSService("test", service, "local.", "test.local.", 4242, []net.IP{net.IPv4(192, 168, 1, 10)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	zone := advertisementZone{service, Advertisement{BackendID: "backend", Name: name, Port: 4242}}
	return zone.Records(dns.Question{Name: serviceDomain, Qtype: dns.TypePTR, Qclass: dns.ClassINET})
}
func pack(t *testing.T, records []dns.RR) []byte {
	t.Helper()
	msg := dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: records}
	packet, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return packet
}
func TestAdvertisementRoundTripReadsCurrentNameAndOnlyPublicMetadata(t *testing.T) {
	name := "Work PC"
	for _, want := range []string{"Work PC", "Renamed PC", `Randy’s \ PC 🖥`} {
		name = want
		records := announcement(t, func() string { return name })
		hosts := parseHosts(pack(t, records))
		if len(hosts) != 1 || hosts[0] != (Host{BackendID: "backend", Name: want, Address: "https://192.168.1.10:4242"}) {
			t.Fatalf("hosts = %+v", hosts)
		}
		for _, record := range records {
			if txt, ok := record.(*dns.TXT); ok && len(txt.Txt) != 3 {
				t.Fatalf("unexpected advertised metadata: %v", txt.Txt)
			}
		}
	}
}
func TestDiscoveryRejectsUnrelatedMalformedOrUnsafeAnnouncements(t *testing.T) {
	cases := []struct {
		name   string
		change func([]dns.RR) []dns.RR
	}{
		{"unrelated service", func(rr []dns.RR) []dns.RR {
			for _, r := range rr {
				if p, ok := r.(*dns.PTR); ok {
					p.Hdr.Name = "_other._tcp.local."
				}
			}
			return rr
		}},
		{"unrelated instance", func(rr []dns.RR) []dns.RR {
			for _, r := range rr {
				if p, ok := r.(*dns.PTR); ok {
					p.Ptr = "wrong._other._tcp.local."
				}
			}
			return rr
		}},
		{"expired", func(rr []dns.RR) []dns.RR {
			for _, r := range rr {
				r.Header().Ttl = 0
			}
			return rr
		}},
		{"loopback", func(rr []dns.RR) []dns.RR {
			for _, r := range rr {
				if a, ok := r.(*dns.A); ok {
					a.A = net.IPv4(127, 0, 0, 1)
				}
			}
			return rr
		}},
		{"public address", func(rr []dns.RR) []dns.RR {
			for _, r := range rr {
				if a, ok := r.(*dns.A); ok {
					a.A = net.IPv4(8, 8, 8, 8)
				}
			}
			return rr
		}},
		{"missing txt", func(rr []dns.RR) []dns.RR {
			var out []dns.RR
			for _, r := range rr {
				if _, ok := r.(*dns.TXT); !ok {
					out = append(out, r)
				}
			}
			return out
		}},
		{"duplicate metadata", func(rr []dns.RR) []dns.RR {
			for _, r := range rr {
				if txt, ok := r.(*dns.TXT); ok {
					txt.Txt = []string{"v=1", "id=x", "id=y"}
				}
			}
			return rr
		}},
		{"invalid version", func(rr []dns.RR) []dns.RR {
			for _, r := range rr {
				if txt, ok := r.(*dns.TXT); ok {
					txt.Txt[0] = "v=2"
				}
			}
			return rr
		}},
		{"control characters", func(rr []dns.RR) []dns.RR {
			for _, r := range rr {
				if txt, ok := r.(*dns.TXT); ok {
					txt.Txt[2] = "name=bad\nname"
				}
			}
			return rr
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := tc.change(announcement(t, func() string { return "PC" }))
			if got := parseHosts(pack(t, records)); len(got) != 0 {
				t.Fatalf("accepted %+v", got)
			}
		})
	}
	if got := parseHosts([]byte("broken DNS")); len(got) != 0 {
		t.Fatalf("accepted malformed DNS: %+v", got)
	}
	if got := parseHosts([]byte(strings.Repeat("x", maxPacketBytes+1))); len(got) != 0 {
		t.Fatal("accepted oversized packet")
	}
}

// TestAdvertisementReadsTheNameOnlyForAnswersThatCarryIt: the responder is
// asked about every name on the LAN, and the name getter reaches a file.
// Only an answer that carries the TXT record may read it.
func TestAdvertisementReadsTheNameOnlyForAnswersThatCarryIt(t *testing.T) {
	service, err := mdns.NewMDNSService("test", service, "local.", "test.local.", 4242, []net.IP{net.IPv4(192, 168, 1, 10)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	zone := advertisementZone{service, Advertisement{BackendID: "backend", Name: func() string {
		t.Fatal("name read for an answer without a TXT record")
		return ""
	}, Port: 4242}}
	if records := zone.Records(dns.Question{Name: "test.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}); len(records) == 0 {
		t.Fatal("address query answered nothing")
	}
	if records := zone.Records(dns.Question{Name: "printer.local.", Qtype: dns.TypeANY, Qclass: dns.ClassINET}); len(records) != 0 {
		t.Fatalf("unrelated query answered %v", records)
	}
}
