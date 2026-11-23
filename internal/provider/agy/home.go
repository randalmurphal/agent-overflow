package agy

import (
	"errors"
	"maps"
	"path/filepath"
	"strings"
)

// Environment variables through which agy chooses its login store and home.
// Launch is the only code that sets or clears them.
const (
	forceFileStorageEnv = "AGY_ACP_FORCE_FILE_STORAGE"
	geminiHomeEnv       = "GEMINI_HOME"
	appDataDirEnv       = "AGY_APP_DATA_DIR"
)

// ambientCredentialEnvVars are scrubbed from agy subprocess environments unless
// explicitly configured by the user so ambient shell GCP/Gemini API exports do
// not hijack Google OAuth sessions.
var ambientCredentialEnvVars = []string{
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_CLOUD_PROJECT",
	"GOOGLE_CLOUD_LOCATION",
	"GOOGLE_GENAI_USE_VERTEXAI",
	"GCLOUD_PROJECT",
	"CLOUDSDK_CORE_PROJECT",
}

// Home is the login store and home directory one agy process runs against.
// The canonical home is agy's default under HOME, on the store the account
// manager reads (provideraccountapp.Manager.AgyCanonicalHome is its only
// maker); an isolated home is a directory of its own on file storage. The
// zero value is no home, and Launch refuses it.
type Home struct {
	// appDataDir is the isolated home's app data directory, empty for the
	// canonical home.
	appDataDir  string
	fileStorage bool
	set         bool
}

// CanonicalHome is agy's default home on the file store (fileStorage) or the
// macOS Keychain. Only the account manager, which reads that store, calls it.
func CanonicalHome(fileStorage bool) Home {
	return Home{fileStorage: fileStorage, set: true}
}

// IsolatedHome is the home whose app data directory is appDataDir
// (<geminiDir>/<appDataSubdir>), on file storage so nothing it does reaches the
// Keychain item of the canonical login.
func IsolatedHome(appDataDir string) (Home, error) {
	cleaned := filepath.Clean(strings.TrimSpace(appDataDir))
	if strings.TrimSpace(appDataDir) == "" || cleaned == "." {
		return Home{}, errors.New("agy: an isolated home needs a directory")
	}
	return Home{appDataDir: cleaned, fileStorage: true, set: true}, nil
}

// Isolated reports a home of its own directory.
func (h Home) Isolated() bool { return h.appDataDir != "" }

// Launch is what an agy command needs to run against one Home: the home
// arguments to place after its subcommand, the environment overrides, and the
// inherited variables to clear.
type Launch struct {
	Args  []string
	Env   map[string]string
	Unset []string
}

// Launch builds the command parts for this home over the user's configured
// environment. The store flag is set for file storage and cleared otherwise,
// and an inherited home variable is always cleared, so an agy process never
// follows a store or home that the environment Agent Overflow inherited
// points elsewhere. Auto-update is disabled so AO's providerupdate service
// owns upgrades.
func (h Home) Launch(customEnv map[string]string) (Launch, error) {
	if !h.set {
		return Launch{}, errors.New("agy: launch without a home")
	}
	env := make(map[string]string, len(customEnv)+3)
	maps.Copy(env, customEnv)
	// HOME is reserved, never set here: the process HOME is the home the
	// account manager reads, and agy's default home lies under it.
	unset := []string{geminiHomeEnv, appDataDirEnv}
	delete(env, "HOME")
	delete(env, geminiHomeEnv)
	delete(env, appDataDirEnv)
	if h.fileStorage {
		env[forceFileStorageEnv] = "1"
		env["SSH_CONNECTION"] = "127.0.0.1 0 127.0.0.1 0"
	} else {
		delete(env, forceFileStorageEnv)
		unset = append(unset, forceFileStorageEnv)
	}
	if _, exists := env["AGY_CLI_DISABLE_AUTO_UPDATE"]; !exists {
		env["AGY_CLI_DISABLE_AUTO_UPDATE"] = "1"
	}
	for _, key := range ambientCredentialEnvVars {
		if _, userSet := customEnv[key]; !userSet {
			unset = append(unset, key)
		}
	}
	var args []string
	if h.Isolated() {
		args = []string{"--gemini-dir", filepath.Dir(h.appDataDir), "--app-data-dir", filepath.Base(h.appDataDir)}
	}
	return Launch{Args: args, Env: env, Unset: unset}, nil
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
