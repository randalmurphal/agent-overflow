package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/network"
)

// The bound address is the one line that is always true, so it is always
// printed. Everything else appears only when it is a fact: a URL the server
// could actually form, a tailnet node that is up, a token this launch minted.
func TestPrintServeEndpointsAlwaysNamesTheBoundAddress(t *testing.T) {
	out := &strings.Builder{}
	printServeEndpoints(out, network.Settings{}, "127.0.0.1:7777")

	text := out.String()
	if !strings.Contains(text, "127.0.0.1:7777") {
		t.Fatalf("the bound address is missing:\n%s", text)
	}
	if strings.Count(text, "\n") != 1 {
		t.Fatalf("printed %d lines for a bare bind, want one:\n%s", strings.Count(text, "\n"), text)
	}
}

func TestPrintServeEndpointsPrintsWhatIsLive(t *testing.T) {
	out := &strings.Builder{}
	endpoints := network.Settings{
		URL:   "https://ao.example.com/",
		Token: "launch-token",
	}
	endpoints.Tailnet.URL = "https://host.tail1234.ts.net/"
	printServeEndpoints(out, endpoints, "0.0.0.0:7777")

	for _, want := range []string{"0.0.0.0:7777", "https://ao.example.com/", "https://host.tail1234.ts.net/", "launch-token"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%q is missing:\n%s", want, out.String())
		}
	}
}

// A cleartext LAN bind carries the token in the open, and the person reading
// this console is the one about to share the URL. network.FromServer decides
// when that is true; this pins that the console says so when it is.
func TestPrintServeEndpointsWarnsAboutCleartext(t *testing.T) {
	out := &strings.Builder{}
	printServeEndpoints(out, network.Settings{
		URL:      "http://192.168.1.20:7777/",
		Token:    "launch-token",
		Insecure: true,
	}, "0.0.0.0:7777")

	text := out.String()
	if !strings.Contains(text, "in the clear") {
		t.Fatalf("no cleartext warning:\n%s", text)
	}
	if !strings.Contains(text, "tailnet") {
		t.Fatalf("the warning names no remedy:\n%s", text)
	}
}

// The quiet case is the common one: a loopback serve host with no domain and
// no tailnet must not print a warning about an exposure it does not have.
func TestPrintServeEndpointsIsQuietOnLoopback(t *testing.T) {
	out := &strings.Builder{}
	printServeEndpoints(out, network.Settings{
		URL:   "http://127.0.0.1:7777/",
		Token: "launch-token",
	}, "127.0.0.1:7777")

	if strings.Contains(out.String(), "in the clear") {
		t.Fatalf("warned about cleartext on a loopback bind:\n%s", out.String())
	}
}
