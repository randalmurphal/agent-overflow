package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// NativeBinary is the executable Agent Overflow runs for a Codex command
// that resolved on PATH.
type NativeBinary struct {
	// Path is the file to execute.
	Path string
	// Wrapper is the real path of the npm package's bin/codex.js that Path
	// was resolved from, or "" when Path is the resolved command itself.
	Wrapper string
}

// codexNpmPackage is the npm package whose bin/codex.js is a Node launcher
// for a vendored native binary.
const codexNpmPackage = "@openai/codex"

// ResolveNativeBinary returns the native binary the npm Codex wrapper at
// resolved would spawn, so a thread runs one process instead of a Node
// launcher plus its child. Anything that is not a recognized wrapper, or a
// wrapper whose native binary is not an executable file, resolves to itself.
//
// The wrapper also exports CODEX_MANAGED_BY_{NPM,BUN,PNPM} and
// CODEX_MANAGED_PACKAGE_ROOT to its child. Codex reads them only for
// `codex doctor` and the TUI's update prompt (codex-rs install-context,
// cli/src/doctor, tui/src/update_action.rs); the commands Agent Overflow runs
// do not, and the bundled rg, bwrap and zsh are found from the executable's
// own package layout. Running the native binary without them changes
// nothing Agent Overflow uses.
func ResolveNativeBinary(resolved string) NativeBinary {
	return resolveNativeBinary(resolved, runtime.GOOS, runtime.GOARCH)
}

// resolveNativeBinary mirrors the wrapper's own lookup (bin/codex.js
// findCodexExecutable) for the given platform: the platform package as
// Node's require.resolve finds it from the wrapper's real location, else the
// package's own vendor directory.
func resolveNativeBinary(resolved, goos, goarch string) NativeBinary {
	unchanged := NativeBinary{Path: resolved}
	triple, platformPackage, ok := codexNpmTarget(goos, goarch)
	if !ok {
		return unchanged
	}
	// npm, bun and Node version managers link the bin entry to the file.
	wrapper, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return unchanged
	}
	binDir := filepath.Dir(wrapper)
	packageRoot := filepath.Dir(binDir)
	if filepath.Base(wrapper) != "codex.js" || filepath.Base(binDir) != "bin" || !isCodexNpmPackage(packageRoot) {
		return unchanged
	}

	vendorRoot := filepath.Join(packageRoot, "vendor")
	if platformRoot, found := resolveNodePackage(binDir, platformPackage); found {
		vendorRoot = filepath.Join(platformRoot, "vendor")
	}
	native := filepath.Join(vendorRoot, triple, "bin", "codex")
	if !isExecutableFile(native) {
		return unchanged
	}
	return NativeBinary{Path: native, Wrapper: wrapper}
}

// codexNpmTarget is the wrapper's PLATFORM_PACKAGE_BY_TARGET for one Go
// platform: the Rust target triple and the npm platform package that vendors
// it.
func codexNpmTarget(goos, goarch string) (triple, platformPackage string, ok bool) {
	var nodeOS, nodeArch, arch, system string
	switch goarch {
	case "amd64":
		nodeArch, arch = "x64", "x86_64"
	case "arm64":
		nodeArch, arch = "arm64", "aarch64"
	default:
		return "", "", false
	}
	switch goos {
	case "linux":
		nodeOS, system = "linux", "unknown-linux-musl"
	case "darwin":
		nodeOS, system = "darwin", "apple-darwin"
	default:
		// Windows runs providers only inside WSL, where the backend is
		// linux; a Windows npm install seen from there is a shell shim,
		// not a link to the wrapper.
		return "", "", false
	}
	return arch + "-" + system, codexNpmPackage + "-" + nodeOS + "-" + nodeArch, true
}

// isCodexNpmPackage reports whether dir holds the @openai/codex package whose
// codex command is bin/codex.js.
func isCodexNpmPackage(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var manifest struct {
		Name string            `json:"name"`
		Bin  map[string]string `json:"bin"`
	}
	if json.Unmarshal(data, &manifest) != nil || manifest.Name != codexNpmPackage {
		return false
	}
	return filepath.Clean(filepath.FromSlash(manifest.Bin["codex"])) == filepath.Join("bin", "codex.js")
}

// resolveNodePackage finds name/package.json the way Node's require.resolve
// does from a module in fromDir: a node_modules directory under every
// ancestor that is not itself named node_modules, nearest first.
func resolveNodePackage(fromDir, name string) (string, bool) {
	rel := filepath.FromSlash(name)
	for dir := fromDir; ; {
		if filepath.Base(dir) != "node_modules" {
			candidate := filepath.Join(dir, "node_modules", rel)
			if info, err := os.Stat(filepath.Join(candidate, "package.json")); err == nil && info.Mode().IsRegular() {
				return candidate, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
