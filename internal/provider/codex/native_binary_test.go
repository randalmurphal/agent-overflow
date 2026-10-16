package codex

import (
	"os"
	"path/filepath"
	"testing"
)

const testCodexPackageJSON = `{"name":"@openai/codex","version":"0.162.1","bin":{"codex":"bin/codex.js"},"type":"module"}`

func writeTestFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func symlinkTestFile(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// writeCodexNpmPackage writes the @openai/codex package at root: its
// package.json and the bin/codex.js wrapper.
func writeCodexNpmPackage(t *testing.T, root string) string {
	t.Helper()
	writeTestFile(t, filepath.Join(root, "package.json"), testCodexPackageJSON, 0o644)
	wrapper := filepath.Join(root, "bin", "codex.js")
	writeTestFile(t, wrapper, "#!/usr/bin/env node\n", 0o755)
	return wrapper
}

// writePlatformPackage writes an npm platform package vendoring the native
// binary for triple, and returns the binary's path.
func writePlatformPackage(t *testing.T, root, triple string) string {
	t.Helper()
	writeTestFile(t, filepath.Join(root, "package.json"), `{"name":"@openai/codex","version":"0.162.1-platform"}`, 0o644)
	native := filepath.Join(root, "vendor", triple, "bin", "codex")
	writeTestFile(t, native, "native", 0o755)
	return native
}

func TestResolveNativeBinaryRecognizedLayouts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		goos   string
		goarch string
		// build lays out the install under dir and returns the PATH entry
		// for `codex` and the native binary it must resolve to.
		build func(t *testing.T, dir string) (command, native string)
	}{
		{
			// npm global under a Node prefix (nvm, fnm, n, a distro or
			// Homebrew node, and Linux npm inside WSL): bin/codex links to
			// the wrapper and the platform package nests under it.
			name: "linux npm prefix", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) (string, string) {
				pkg := filepath.Join(dir, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				native := writePlatformPackage(t, filepath.Join(pkg, "node_modules", "@openai", "codex-linux-x64"), "x86_64-unknown-linux-musl")
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", command)
				return command, native
			},
		},
		{
			name: "linux arm64 npm prefix", goos: "linux", goarch: "arm64",
			build: func(t *testing.T, dir string) (string, string) {
				pkg := filepath.Join(dir, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				native := writePlatformPackage(t, filepath.Join(pkg, "node_modules", "@openai", "codex-linux-arm64"), "aarch64-unknown-linux-musl")
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", command)
				return command, native
			},
		},
		{
			name: "macos arm64 npm prefix", goos: "darwin", goarch: "arm64",
			build: func(t *testing.T, dir string) (string, string) {
				pkg := filepath.Join(dir, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				native := writePlatformPackage(t, filepath.Join(pkg, "node_modules", "@openai", "codex-darwin-arm64"), "aarch64-apple-darwin")
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", command)
				return command, native
			},
		},
		{
			name: "macos x64 npm prefix", goos: "darwin", goarch: "amd64",
			build: func(t *testing.T, dir string) (string, string) {
				pkg := filepath.Join(dir, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				native := writePlatformPackage(t, filepath.Join(pkg, "node_modules", "@openai", "codex-darwin-x64"), "x86_64-apple-darwin")
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", command)
				return command, native
			},
		},
		{
			// bun global: the platform package is hoisted beside the
			// wrapper's package, where Node's lookup walks up to it.
			name: "bun global hoisted", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) (string, string) {
				modules := filepath.Join(dir, ".bun", "install", "global", "node_modules")
				writeCodexNpmPackage(t, filepath.Join(modules, "@openai", "codex"))
				native := writePlatformPackage(t, filepath.Join(modules, "@openai", "codex-linux-x64"), "x86_64-unknown-linux-musl")
				command := filepath.Join(dir, ".bun", "bin", "codex")
				symlinkTestFile(t, filepath.Join(modules, "@openai", "codex", "bin", "codex.js"), command)
				return command, native
			},
		},
		{
			// A symlink chain (a version manager's current link in front of
			// the prefix) resolves through to the wrapper's real location.
			name: "linked prefix", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) (string, string) {
				real := filepath.Join(dir, "versions", "v24")
				pkg := filepath.Join(real, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				native := writePlatformPackage(t, filepath.Join(pkg, "node_modules", "@openai", "codex-linux-x64"), "x86_64-unknown-linux-musl")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", filepath.Join(real, "bin", "codex"))
				symlinkTestFile(t, real, filepath.Join(dir, "current"))
				return filepath.Join(dir, "current", "bin", "codex"), native
			},
		},
		{
			// No platform package: the wrapper falls back to its own
			// vendor directory.
			name: "package vendor directory", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) (string, string) {
				pkg := filepath.Join(dir, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				native := filepath.Join(pkg, "vendor", "x86_64-unknown-linux-musl", "bin", "codex")
				writeTestFile(t, native, "native", 0o755)
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", command)
				return command, native
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			command, native := tt.build(t, dir)
			got := resolveNativeBinary(command, tt.goos, tt.goarch)
			wantNative, err := filepath.EvalSymlinks(native)
			if err != nil {
				t.Fatal(err)
			}
			if got.Path != wantNative {
				t.Fatalf("Path = %q, want native %q", got.Path, wantNative)
			}
			wrapper, err := filepath.EvalSymlinks(command)
			if err != nil {
				t.Fatal(err)
			}
			if got.Wrapper != wrapper {
				t.Fatalf("Wrapper = %q, want %q", got.Wrapper, wrapper)
			}
		})
	}
}

// Every case here must resolve to the command itself: either it is not the
// npm wrapper, or the native binary the wrapper would spawn is not there.
func TestResolveNativeBinaryKeepsUnrecognizedCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		goos   string
		goarch string
		build  func(t *testing.T, dir string) string
	}{
		{
			name: "native binary missing", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) string {
				pkg := filepath.Join(dir, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", command)
				return command
			},
		},
		{
			// The wrapper uses the platform package once require.resolve
			// finds it and fails if its binary is missing; it never falls
			// through to the package vendor directory.
			name: "platform package without its binary", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) string {
				pkg := filepath.Join(dir, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				writeTestFile(t, filepath.Join(pkg, "node_modules", "@openai", "codex-linux-x64", "package.json"), `{}`, 0o644)
				writeTestFile(t, filepath.Join(pkg, "vendor", "x86_64-unknown-linux-musl", "bin", "codex"), "native", 0o755)
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", command)
				return command
			},
		},
		{
			name: "native binary not executable", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) string {
				pkg := filepath.Join(dir, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				native := writePlatformPackage(t, filepath.Join(pkg, "node_modules", "@openai", "codex-linux-x64"), "x86_64-unknown-linux-musl")
				if err := os.Chmod(native, 0o644); err != nil {
					t.Fatal(err)
				}
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", command)
				return command
			},
		},
		{
			// npm installs only the platform package for Node's own
			// platform; another platform's binary is not this one's.
			name: "other platform's binary", goos: "darwin", goarch: "arm64",
			build: func(t *testing.T, dir string) string {
				pkg := filepath.Join(dir, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				writePlatformPackage(t, filepath.Join(pkg, "node_modules", "@openai", "codex-linux-x64"), "x86_64-unknown-linux-musl")
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", command)
				return command
			},
		},
		{
			// A Windows npm install seen from WSL: npm's cmd-shim writes a
			// shell script, not a link, and vendors the win32 binary.
			name: "windows npm shim under wsl", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) string {
				npm := filepath.Join(dir, "AppData", "Roaming", "npm")
				pkg := filepath.Join(npm, "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				writeTestFile(t, filepath.Join(pkg, "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe"), "native", 0o755)
				command := filepath.Join(npm, "codex")
				writeTestFile(t, command, "#!/bin/sh\nexec node \"$basedir/node_modules/@openai/codex/bin/codex.js\" \"$@\"\n", 0o755)
				return command
			},
		},
		{
			// pnpm global installs a shell shim too.
			name: "pnpm shim", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) string {
				pkg := filepath.Join(dir, "global", "5", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				writePlatformPackage(t, filepath.Join(pkg, "node_modules", "@openai", "codex-linux-x64"), "x86_64-unknown-linux-musl")
				command := filepath.Join(dir, "codex")
				writeTestFile(t, command, "#!/bin/sh\nexec node \"$basedir/global/5/node_modules/@openai/codex/bin/codex.js\" \"$@\"\n", 0o755)
				return command
			},
		},
		{
			name: "standalone native binary", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) string {
				command := filepath.Join(dir, "bin", "codex")
				writeTestFile(t, command, "native", 0o755)
				return command
			},
		},
		{
			name: "codex.js of another package", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) string {
				pkg := filepath.Join(dir, "lib", "node_modules", "codex-fork")
				writeTestFile(t, filepath.Join(pkg, "package.json"), `{"name":"codex-fork","bin":{"codex":"bin/codex.js"}}`, 0o644)
				writeTestFile(t, filepath.Join(pkg, "bin", "codex.js"), "#!/usr/bin/env node\n", 0o755)
				writePlatformPackage(t, filepath.Join(pkg, "node_modules", "@openai", "codex-linux-x64"), "x86_64-unknown-linux-musl")
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/codex-fork/bin/codex.js", command)
				return command
			},
		},
		{
			name: "unsupported platform", goos: "windows", goarch: "amd64",
			build: func(t *testing.T, dir string) string {
				pkg := filepath.Join(dir, "lib", "node_modules", "@openai", "codex")
				writeCodexNpmPackage(t, pkg)
				writeTestFile(t, filepath.Join(pkg, "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe"), "native", 0o755)
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, "../lib/node_modules/@openai/codex/bin/codex.js", command)
				return command
			},
		},
		{
			name: "dangling command", goos: "linux", goarch: "amd64",
			build: func(t *testing.T, dir string) string {
				command := filepath.Join(dir, "bin", "codex")
				symlinkTestFile(t, filepath.Join(dir, "missing", "codex.js"), command)
				return command
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			command := tt.build(t, t.TempDir())
			got := resolveNativeBinary(command, tt.goos, tt.goarch)
			if got != (NativeBinary{Path: command}) {
				t.Fatalf("resolveNativeBinary(%q) = %+v, want the command unchanged", command, got)
			}
		})
	}
}
