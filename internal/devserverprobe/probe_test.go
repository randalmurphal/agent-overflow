package devserverprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeProber returns a Prober whose dial and clock are test-controlled.
// The dial func records every address tried; verdicts come from the
// supplied decide func.
func fakeProber(decide func(address string) bool) (*Prober, *[]string, *time.Time) {
	var mu sync.Mutex
	tried := &[]string{}
	current := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	now := &current
	p := &Prober{
		entries: make(map[string]entry),
		dial: func(_ context.Context, address string) (net.Conn, error) {
			mu.Lock()
			*tried = append(*tried, address)
			mu.Unlock()
			if decide(address) {
				client, server := net.Pipe()
				go func() { _ = server.Close() }()
				return client, nil
			}
			return nil, errors.New("connection refused")
		},
		now: func() time.Time { return *now },
	}
	return p, tried, now
}

func TestLiveAgainstRealListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	p := New()
	url := fmt.Sprintf("http://127.0.0.1:%d/", listener.Addr().(*net.TCPAddr).Port)
	live, err := p.Live(context.Background(), url)
	if err != nil {
		t.Fatalf("Live(%q): %v", url, err)
	}
	if !live {
		t.Fatalf("Live(%q) = false, want true", url)
	}
}

func TestLiveAgainstClosedPort(t *testing.T) {
	// Grab a port the OS considers free, then close it so nothing
	// listens there when the probe dials.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	p := New()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	live, err := p.Live(context.Background(), url)
	if err != nil {
		t.Fatalf("Live(%q): %v", url, err)
	}
	if live {
		t.Fatalf("Live(%q) = true, want false", url)
	}
}

func TestLocalhostTriesBothLoopbackFamilies(t *testing.T) {
	p, tried, _ := fakeProber(func(string) bool { return false })
	live, err := p.Live(context.Background(), "http://localhost:5173/")
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if live {
		t.Fatal("Live = true with every dial refused")
	}
	want := []string{"127.0.0.1:5173", "[::1]:5173"}
	if len(*tried) != len(want) || (*tried)[0] != want[0] || (*tried)[1] != want[1] {
		t.Fatalf("dialed %v, want %v", *tried, want)
	}
}

func TestLocalhostFallsBackToIPv6OnlyListener(t *testing.T) {
	p, tried, _ := fakeProber(func(address string) bool { return address == "[::1]:5173" })
	live, err := p.Live(context.Background(), "http://localhost:5173/")
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if !live {
		t.Fatal("Live = false with a listener on [::1]")
	}
	want := []string{"127.0.0.1:5173", "[::1]:5173"}
	if len(*tried) != len(want) || (*tried)[0] != want[0] || (*tried)[1] != want[1] {
		t.Fatalf("dialed %v, want %v", *tried, want)
	}
}

func TestLocalhostShortCircuitsOnFirstSuccess(t *testing.T) {
	p, tried, _ := fakeProber(func(address string) bool { return address == "127.0.0.1:5173" })
	live, err := p.Live(context.Background(), "http://localhost:5173/")
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if !live {
		t.Fatal("Live = false, want true")
	}
	if len(*tried) != 1 {
		t.Fatalf("dialed %v, want the IPv4 attempt only", *tried)
	}
}

func TestDefaultPortsByScheme(t *testing.T) {
	p, tried, _ := fakeProber(func(string) bool { return false })
	if _, err := p.Live(context.Background(), "http://127.0.0.1/"); err != nil {
		t.Fatalf("http default port: %v", err)
	}
	if _, err := p.Live(context.Background(), "https://127.0.0.1/"); err != nil {
		t.Fatalf("https default port: %v", err)
	}
	want := []string{"127.0.0.1:80", "127.0.0.1:443"}
	if len(*tried) != len(want) || (*tried)[0] != want[0] || (*tried)[1] != want[1] {
		t.Fatalf("dialed %v, want %v", *tried, want)
	}
}

func TestRejectsNonLoopbackInput(t *testing.T) {
	p, tried, _ := fakeProber(func(string) bool { return true })
	for _, raw := range []string{
		"",
		"not a url",
		"ftp://localhost:21/",
		"http:///path",
		"http://example.com/",
		"http://192.168.1.24:5173/",
		"http://10.0.0.5:80/",
		// Triage rewrites wildcard binds before meta; the raw forms are
		// invalid here.
		"http://0.0.0.0:5173/",
		"http://[::]:5173/",
		// Outside 127.0.0.0/8.
		"http://128.0.0.1:80/",
		"http://localhost.evil.com:80/",
		// Zoned loopback: meaningless on ::1 and an unbounded cache-key
		// space if accepted ("%25" is a URL-escaped "%").
		"http://[::1%25eth0]:5173/",
	} {
		if _, err := p.Live(context.Background(), raw); err == nil {
			t.Errorf("Live(%q) accepted, want error", raw)
		}
	}
	if len(*tried) != 0 {
		t.Fatalf("rejected input still dialed %v", *tried)
	}
}

func TestBracketedIPv6Loopback(t *testing.T) {
	p, tried, _ := fakeProber(func(string) bool { return true })
	live, err := p.Live(context.Background(), "http://[::1]:5173/")
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if !live {
		t.Fatal("Live = false, want true")
	}
	if len(*tried) != 1 || (*tried)[0] != "[::1]:5173" {
		t.Fatalf("dialed %v, want [::1]:5173", *tried)
	}
}

func TestVerdictCacheAndExpiry(t *testing.T) {
	liveAnswer := true
	p, tried, now := fakeProber(func(string) bool { return liveAnswer })

	// First call dials and caches a live verdict.
	if live, _ := p.Live(context.Background(), "http://127.0.0.1:3000/"); !live {
		t.Fatal("first probe: want live")
	}
	// Within liveTTL: served from cache even though the server died.
	liveAnswer = false
	if live, _ := p.Live(context.Background(), "http://127.0.0.1:3000/"); !live {
		t.Fatal("cached probe: want live from cache")
	}
	if len(*tried) != 1 {
		t.Fatalf("cached probe dialed again: %v", *tried)
	}
	// Past liveTTL: re-dials and observes the death.
	*now = now.Add(liveTTL + time.Millisecond)
	if live, _ := p.Live(context.Background(), "http://127.0.0.1:3000/"); live {
		t.Fatal("expired probe: want dead")
	}
	// Dead verdicts expire on the shorter TTL, so a just-started server
	// is noticed promptly.
	liveAnswer = true
	*now = now.Add(deadTTL + time.Millisecond)
	if live, _ := p.Live(context.Background(), "http://127.0.0.1:3000/"); !live {
		t.Fatal("post-deadTTL probe: want live again")
	}
	if len(*tried) != 3 {
		t.Fatalf("dialed %d times, want 3", len(*tried))
	}
}

func TestCacheStaysBounded(t *testing.T) {
	p, _, _ := fakeProber(func(string) bool { return false })
	for port := 1000; port < 1000+maxEntries*2; port++ {
		url := fmt.Sprintf("http://127.0.0.1:%d/", port)
		if _, err := p.Live(context.Background(), url); err != nil {
			t.Fatalf("Live(%q): %v", url, err)
		}
	}
	p.mu.Lock()
	size := len(p.entries)
	p.mu.Unlock()
	if size > maxEntries {
		t.Fatalf("cache grew to %d entries, cap is %d", size, maxEntries)
	}
}
