package agy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/provider"
)

// sshConnectionValue is what a file-store launch sets SSH_CONNECTION to, as
// the sign-in flow has since it was added; the repo records no reason of its
// own for it.
const sshConnectionValue = "127.0.0.1 0 127.0.0.1 0"

// Home is the login store and home directory one agy process runs against.
// The canonical home is the login the account manager reads, on the store it
// reads it from (provideraccountapp.Manager.AgyCanonicalHome is its only
// maker); an isolated home is a sign-in's or a saved login's directory of its
// own, on file storage. The zero value is no home, and Launch refuses it.
type Home struct {
	// appDataDir is the app data directory the launch names, empty for agy's
	// default under the process HOME.
	appDataDir  string
	fileStorage bool
	isolated    bool
	set         bool
}

// HomeSource decides the Home of one launch. A launcher calls it with its own
// context, inside its own time limit, immediately before it starts the
// process, because the canonical store can change between launches.
type HomeSource func(ctx context.Context) (Home, error)

// Source is a HomeSource that answers h, for a home already decided.
func (h Home) Source() HomeSource {
	return func(context.Context) (Home, error) { return h, nil }
}

// CanonicalHome is the canonical login's home: agy's default under the
// process HOME when appDataDir is empty, else appDataDir, on the file store
// (fileStorage) or the macOS Keychain. Only the account manager, which reads
// that store, calls it. A directory of its own is only ever the file store's:
// what a named directory does to agy's Keychain login is unknown, and no boot
// that names one uses the Keychain.
func CanonicalHome(appDataDir string, fileStorage bool) (Home, error) {
	if appDataDir == "" {
		return Home{fileStorage: fileStorage, set: true}, nil
	}
	if !fileStorage {
		return Home{}, fmt.Errorf("agy: the canonical home %s is not agy's default, and only the file store may name its directory", appDataDir)
	}
	dir, err := cleanAppDataDir(appDataDir)
	if err != nil {
		return Home{}, err
	}
	return Home{appDataDir: dir, fileStorage: true, set: true}, nil
}

// IsolatedHome is the home whose app data directory is appDataDir
// (<geminiDir>/<appDataSubdir>), on file storage so nothing it does reaches the
// Keychain item of the canonical login.
func IsolatedHome(appDataDir string) (Home, error) {
	dir, err := cleanAppDataDir(appDataDir)
	if err != nil {
		return Home{}, err
	}
	return Home{appDataDir: dir, fileStorage: true, isolated: true, set: true}, nil
}

func cleanAppDataDir(appDataDir string) (string, error) {
	if strings.TrimSpace(appDataDir) == "" {
		return "", errors.New("agy: a home needs a directory")
	}
	cleaned := filepath.Clean(strings.TrimSpace(appDataDir))
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("agy: the home directory %q is not an absolute path", appDataDir)
	}
	return cleaned, nil
}

// Isolated reports a sign-in's or a saved login's home, not the canonical one.
func (h Home) Isolated() bool { return h.isolated }

// Identity names the store and directory a process on h reads its login
// from, for a key that must tell apart servers on different ones.
func (h Home) Identity() string {
	return fmt.Sprintf("set=%t isolated=%t file=%t dir=%s", h.set, h.isolated, h.fileStorage, h.appDataDir)
}

// Launch is what an agy command needs to run against one Home: the home
// arguments to place after its subcommand, and the environment to start it
// with.
type Launch struct {
	Args []string
	Env  provider.EnvInput
}

// Launch builds the command parts for this home over in, the user's
// configured environment. The store flag is pinned for file storage and
// cleared otherwise, and an inherited home variable is always cleared, so an
// agy process never follows a store or home that the environment Agent
// Overflow inherited points elsewhere. HOME is reserved and never set here:
// the process HOME is the home the account manager reads, and agy's default
// home lies under it. SSH_CONNECTION is pinned only with the file store; an
// inherited one is otherwise left alone. Auto-update is pinned off so AO's
// providerupdate service owns upgrades, and the ambient GCP/Gemini
// credentials a shell exports are cleared so they cannot hijack a Google
// OAuth session; a value the user's custom environment sets for one of them
// still applies.
func (h Home) Launch(in provider.EnvInput) (Launch, error) {
	if !h.set {
		return Launch{}, errors.New("agy: launch without a home")
	}
	env := in.Clear(provider.AgyGeminiHomeEnv, provider.AgyAppDataDirEnv)
	if h.fileStorage {
		env = env.Pin(provider.AgyForceFileStorageEnv, "1").
			Pin(provider.AgySSHConnectionEnv, sshConnectionValue)
	} else {
		env = env.Clear(provider.AgyForceFileStorageEnv)
	}
	env = env.Pin(provider.AgyDisableAutoUpdateEnv, "1").
		Clear(provider.AgyAmbientCredentialEnvs...)
	var args []string
	if h.appDataDir != "" {
		args = []string{"--gemini-dir", filepath.Dir(h.appDataDir), "--app-data-dir", filepath.Base(h.appDataDir)}
	}
	return Launch{Args: args, Env: env}, nil
}

// OnDefaultHome refuses a launch that names its home directory, for a
// command other than `agy acp`: only `acp` is known to take --gemini-dir and
// --app-data-dir, so such a command cannot be pointed at that home and would
// read agy's default under the process HOME instead.
func (l Launch) OnDefaultHome(command string) error {
	if len(l.Args) == 0 {
		return nil
	}
	return fmt.Errorf("agy: `agy %s` runs only on agy's default home under HOME, and this login's home is %s", command, strings.Join(l.Args, " "))
}

// ACPArgs is the argv of a shared multiplexed `agy acp` server on this
// launch's home, followed by extra. It omits `--process-per-session`
// (measured 16x lower per-session memory with verified same-name HTTP MCP and
// cwd isolation) and `--dangerously-skip-permissions`, so every AO RuntimeMode
// and plan mode switches live without restarting the server.
func (l Launch) ACPArgs(extra ...string) []string {
	args := append([]string{"acp"}, l.Args...)
	return append(args, extra...)
}
