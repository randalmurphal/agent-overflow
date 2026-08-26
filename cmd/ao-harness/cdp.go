package main

// The half `profile` and `bench --trace` share: where a DevTools endpoint
// comes from, how the instance's own page is found on it, and the one
// sentence that explains why an instance may not have one at all.
//
// EVERY OTHER COMMAND in this CLI reaches the page through the harness
// bridge, which is engine-agnostic. These two cannot: a CPU profile and a
// forced-layout trace are Chromium instruments, and no bridge can
// synthesize them. That is the whole reason the endpoint is a flag rather
// than something the instance publishes — the browser attached to a
// harness is the operator's choice, and on Linux (WebKitGTK) there is no
// such endpoint to publish.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/cdpclient"
	"agent-overflow/internal/harnessclient"
)

const (
	// cdpURLEnv and cdpPortEnv are the two spellings of the same default.
	// An agent driving one browser for a whole session exports one instead
	// of threading --cdp through every invocation; the flag still wins.
	cdpURLEnv  = "AO_CDP_URL"
	cdpPortEnv = "AO_CDP_PORT"
)

// cdpAttachTimeout bounds discovery plus the WebSocket open. Loopback, so
// anything slower is a wedged browser rather than a slow network.
const cdpAttachTimeout = 15 * time.Second

// bindCDPFlag declares --cdp with the environment defaults resolved, so
// `-h` prints what an unflagged run would actually use.
func bindCDPFlag(flags *flag.FlagSet) *string {
	return flags.String("cdp", defaultCDPSpec(),
		"devtools endpoint: a port, host:port, http://host:port, or a ws:// page url (default $"+cdpURLEnv+", else $"+cdpPortEnv+")")
}

func defaultCDPSpec() string {
	if value := strings.TrimSpace(os.Getenv(cdpURLEnv)); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(cdpPortEnv))
}

// resolveCDPEndpoint reads the flag. An absent endpoint is a USAGE error
// (the invocation is under-specified), and an unparseable one is too;
// both carry the note about which engines can serve one at all.
func resolveCDPEndpoint(spec string, t target) (cdpclient.Endpoint, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return cdpclient.Endpoint{}, usagef(
			"no devtools endpoint: pass --cdp <port|host:port|ws url>, or set $%s / $%s\n%s",
			cdpURLEnv, cdpPortEnv, cdpRequirementNote(t))
	}
	endpoint, err := cdpclient.ParseEndpoint(spec)
	if err != nil {
		return cdpclient.Endpoint{}, usagef("%v\n%s", err, cdpRequirementNote(t))
	}
	return endpoint, nil
}

// attachCDP opens the instance's own page on the endpoint. The instance
// URL is what disambiguates a browser holding several tabs; a failure
// here repeats the engine note, because "connection refused" on a port an
// operator typed from memory is most often a WebKitGTK window.
func attachCDP(ctx context.Context, endpoint cdpclient.Endpoint, t target, bs harnessclient.Bootstrap) (*cdpclient.Conn, cdpclient.Target, error) {
	attachCtx, cancel := context.WithTimeout(ctx, cdpAttachTimeout)
	defer cancel()
	conn, page, err := cdpclient.Attach(attachCtx, endpoint, bs.URL)
	if err != nil {
		return nil, cdpclient.Target{}, fmt.Errorf("%w\n%s", err, cdpRequirementNote(t))
	}
	return conn, page, nil
}

// cdpRequirementNote is the honest statement of what serves this
// protocol. It names the instance's OWN port when the registry row says
// which shell it is, because "9224 or 9225" is a coin flip an operator
// should not have to call.
func cdpRequirementNote(t target) string {
	var b strings.Builder
	b.WriteString("  a devtools endpoint is served only by a CHROMIUM-family webview:\n")
	if t.Row != nil {
		if port := appidentity.DevToolsPort(string(t.Row.Mode)); port > 0 {
			fmt.Fprintf(&b, "    this instance is mode=%s, whose Windows WebView2 shell publishes 127.0.0.1:%d — try --cdp %d\n",
				t.Row.Mode, port, port)
		}
	}
	b.WriteString("    the Windows WebView2 shells publish one on loopback (soak 9224, harness 9225)\n")
	b.WriteString("    an external Chrome/Edge does with --remote-debugging-port=<port>\n")
	b.WriteString("  a WebKitGTK window (`make harness-window` on Linux) serves NO devtools protocol,\n")
	b.WriteString("  so neither `ao-harness profile` nor `ao-harness bench --trace` can run against one.")
	return b.String()
}
