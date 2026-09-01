package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// The mock's account sign-in surfaces. Both providers' real flows finish
// somewhere this process cannot reach — a browser on somebody's phone — so what
// is reproduced here is the wire on THIS side of that: the URLs and codes AO
// shows, the answers it decodes, and the credential it then adopts.
//
// Claude's half lives here in full, because its sign-in is a whole invocation
// of its own (`claude -p --input-format stream-json --output-format
// stream-json --verbose`, holding stdin open and speaking only
// control_requests). Codex's half is in codex_login.go, because its sign-in
// runs over an ordinary `app-server` connection.
//
// One safety rule binds both: a credential is written ONLY into the isolated
// home the invocation named (CLAUDE_CONFIG_DIR / CODEX_HOME), and an
// invocation that named none writes nothing at all. AO points those at a fresh
// ephemeral directory for every sign-in, so the mock can never reach a
// developer's real provider home.

// mockClaudeLoginCode is the code the fake approval page "shows" the person.
// A callback carrying anything else is what the real CLI answers with a 400 —
// and, because one failure burns the flow, is how the fresh-link path is
// driven from a test.
const mockClaudeLoginCode = "mock-auth-code"

// mockLoginEmail is the identity every mock sign-in lands on. It matches the
// account the probe reports (claudeAccountJSON) and the one the Codex
// `account/read` answer carries, because adoption resolves the saved slot by
// the identity the PROBE observed — a credential describing somebody else
// would be adopted under the probe's name anyway, which is a fiction with no
// test value.
const mockLoginEmail = "mock@agent-overflow.test"

// isClaudeLoginInvocation matches claude.loginArgs(). `-p` plus
// `--output-format stream-json` is unambiguous: a streaming SESSION never
// passes `-p`, one-shot text generation asks for `--output-format json`, and
// the account probe is discriminated before this by `--max-turns 0`.
func isClaudeLoginInvocation(args []string) bool {
	return slices.Contains(args, "-p") && flagValue(args, "--output-format") == "stream-json"
}

// runClaudeLogin serves the sign-in control channel until the app closes
// stdin. No scenario and no control-channel registration: a sign-in is a side
// invocation like the probe, and registering it as a mock would put a process
// with no turns into every test's mock listing.
func runClaudeLogin() {
	log.Printf("claude sign-in invocation detected (-p --output-format stream-json)")
	mock := &claudeLoginMock{
		w:         newLineWriter(os.Stdout),
		configDir: os.Getenv("CLAUDE_CONFIG_DIR"),
	}
	forEachStdinLine(mock.handleLine)
	// EOF: the coordinator closes stdin to end the flow, whichever way it went.
}

// claudeLoginMock is one sign-in process. The whole state machine is the live
// flow's `state` value: minting one starts a flow, and clearing it is what
// "burned" means — every later request answers the CLI's own "No active
// claude_authenticate flow", which is the sentence AO's driver matches on.
type claudeLoginMock struct {
	w         *lineWriter
	configDir string

	mu         sync.Mutex
	state      string
	generation int
}

func (m *claudeLoginMock) handleLine(line []byte) {
	var env struct {
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		log.Printf("claude sign-in: malformed stdin line ignored: %v", err)
		return
	}
	if env.Type != "control_request" {
		return
	}
	switch subtype := claudeControlRequestSubtype(env.Request); subtype {
	case "claude_authenticate":
		m.authenticate(env.RequestID)
	case "claude_oauth_callback":
		m.callback(env.RequestID, env.Request)
	case "claude_oauth_wait_for_completion":
		// Deliberately never answered. The real CLI resolves this only when
		// its own loopback listener sees the browser come back, which cannot
		// happen to a mock, and it sends no keepalive in the meantime — so
		// parking is the honest reproduction. The browser method's own budget
		// is the caller's (claude.DefaultLoginTimeout).
		log.Printf("claude sign-in: parking claude_oauth_wait_for_completion %q", env.RequestID)
	default:
		// Forward compatibility, the same rule writeClaudeControlAck follows:
		// a subtype this mock has never heard of must not turn into a harness
		// failure with no diagnosis.
		log.Printf("claude sign-in: subtype %q is unknown to this mock; acking permissively", subtype)
		writeClaudeControlSuccess(m.w, env.RequestID, "{}")
	}
}

// authenticate mints a fresh flow, superseding whatever was live. The state
// value changes every time, which is what makes a replaced link visible: the
// URL AO renders after a burned flow is not the URL it rendered before.
func (m *claudeLoginMock) authenticate(requestID string) {
	m.mu.Lock()
	m.generation++
	state := fmt.Sprintf("mock-state-%d", m.generation)
	m.state = state
	m.mu.Unlock()

	// Two URLs carrying the SAME state, differing only in redirect_uri —
	// upstream's shape, and the reason AO can offer one flow two ways.
	payload := map[string]string{
		"manualUrl": "https://mock.agent-overflow.test/oauth/authorize" +
			"?code=" + mockClaudeLoginCode + "&state=" + state + "&redirect_uri=manual",
		"automaticUrl": "https://mock.agent-overflow.test/oauth/authorize" +
			"?state=" + state + "&redirect_uri=http%3A%2F%2Flocalhost%3A54545%2Fcallback",
	}
	writeClaudeControlSuccess(m.w, requestID, mustJSON(payload))
}

// callback answers the pasted `code#state`. One failure BURNS the flow, which
// is upstream's behaviour and the whole reason AO restarts rather than
// re-prompts: the listener closes and the slot clears, so everything after
// answers the no-active-flow sentence.
func (m *claudeLoginMock) callback(requestID string, request json.RawMessage) {
	if missing := missingControlRequestKeys(request, []string{"authorizationCode", "state"}); missing != "" {
		writeClaudeControlError(m.w, requestID, "control_request claude_oauth_callback: "+missing)
		return
	}
	var body struct {
		AuthorizationCode string `json:"authorizationCode"`
		State             string `json:"state"`
	}
	if err := json.Unmarshal(request, &body); err != nil {
		writeClaudeControlError(m.w, requestID, fmt.Sprintf("request object is not readable (%v)", err))
		return
	}

	m.mu.Lock()
	live := m.state
	matched := live != "" && body.State == live && body.AuthorizationCode == mockClaudeLoginCode
	// Spent either way: a good code completes the exchange and a bad one
	// destroys it.
	m.state = ""
	m.mu.Unlock()

	if live == "" {
		writeClaudeControlError(m.w, requestID, "No active claude_authenticate flow")
		return
	}
	if !matched {
		// The CLI's own opaque answer for a rejected exchange. It says nothing
		// about which half was wrong, which is exactly why AO's copy has to
		// explain the consequence instead of forwarding this.
		writeClaudeControlError(m.w, requestID, "Request failed with status code 400")
		return
	}
	if err := m.writeCredential(); err != nil {
		writeClaudeControlError(m.w, requestID, err.Error())
		return
	}
	writeClaudeControlSuccess(m.w, requestID, `{"account":`+claudeAccountJSON+`}`)
}

// writeCredential lands the two files adoption reads out of the login home:
// the native credential (.credentials.json) and the identity record the CLI
// caches beside it (.claude.json), which is the only on-disk source of the
// organization uuid.
//
// An invocation with no CLAUDE_CONFIG_DIR writes nothing and says so. That is
// not a degradation to tolerate — it is the guard that keeps this mock out of
// a real provider home, since AO always names an isolated one.
func (m *claudeLoginMock) writeCredential() error {
	if m.configDir == "" {
		return fmt.Errorf("this mock writes a credential only into CLAUDE_CONFIG_DIR, and none was set")
	}
	if err := os.MkdirAll(m.configDir, 0o700); err != nil {
		return fmt.Errorf("create mock login home: %w", err)
	}
	// A real access token is fixed-TTL (8h) and its expiry doubles as the
	// rotation-chain position, so a credential that already looks expired
	// would read as older than whatever it replaced.
	expiresAt := time.Now().Add(8 * time.Hour).UnixMilli()
	credential := fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":"mock-access-%d","refreshToken":"mock-refresh-%d",`+
			`"expiresAt":%d,"scopes":["user:inference","user:profile"],"subscriptionType":"max"}}`,
		expiresAt, expiresAt, expiresAt)
	if err := writeMockLoginFile(filepath.Join(m.configDir, ".credentials.json"), credential); err != nil {
		return err
	}
	identity := fmt.Sprintf(
		`{"oauthAccount":{"accountUuid":"mock-account-uuid","emailAddress":%s,`+
			`"organizationUuid":"mock-org-uuid","organizationName":"Mock Org"}}`,
		mustJSON(mockLoginEmail))
	return writeMockLoginFile(filepath.Join(m.configDir, ".claude.json"), identity)
}

// writeMockLoginFile writes one credential-shaped file at 0600, the mode the
// real stores use and the one provideraccounts preserves when it copies the
// bytes into a slot.
func writeMockLoginFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}
