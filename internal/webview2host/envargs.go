package webview2host

import (
	"fmt"
	"strconv"
	"strings"
)

// paneEnvironmentArgs is the pane environment's AdditionalBrowserArguments:
// its own DevTools port followed by the launcher's extra switches.
//
// --remote-debugging-port goes on the PANE environment ONLY. If two
// environments carry it the first browser process to start wins the port
// and the isolation inverts SILENTLY: the debug endpoint would then serve
// the app's own UI and none of the pane's pages. The SPA environment must
// never be given this flag "just in case", and no extra switch may restate
// it here; ValidateExtraBrowserArgs refuses that at host construction.
func paneEnvironmentArgs(cdpPort int, extra []string) string {
	args := make([]string, 0, 1+len(extra))
	args = append(args, "--remote-debugging-port="+strconv.Itoa(cdpPort))
	args = append(args, extra...)
	return strings.Join(args, " ")
}

// ValidateExtraBrowserArgs checks the switches a launcher may add to the
// pane environment: whole "--switch" or "--switch=value" tokens with no
// whitespace (the environment takes one space-joined string, so a token
// with a space would become two switches), and never a remote-debugging
// switch, which the host owns.
func ValidateExtraBrowserArgs(extra []string) error {
	for _, arg := range extra {
		if !strings.HasPrefix(arg, "--") || len(arg) == 2 {
			return fmt.Errorf("browser host extra argument %q is not a --switch", arg)
		}
		if strings.ContainsAny(arg, " \t\r\n") {
			return fmt.Errorf("browser host extra argument %q contains whitespace", arg)
		}
		if strings.HasPrefix(arg, "--remote-debugging-") {
			return fmt.Errorf("browser host extra argument %q: the host owns the pane's debugging endpoint", arg)
		}
	}
	return nil
}
