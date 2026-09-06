#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT HUP INT TERM

make_fake_release() {
	dir=$1
	mkdir -p "$dir"
	printf '#!/usr/bin/env sh\nexit 0\n' > "$dir/agent-overflow-linux-amd64"
	chmod +x "$dir/agent-overflow-linux-amd64"
	printf 'fake windows launcher\n' > "$dir/agent-overflow-wsl-amd64.exe"
	macos_stage=$TMP_DIR/macos-stage
	mkdir -p "$macos_stage/Agent Overflow.app/Contents/MacOS"
	printf '#!/usr/bin/env sh\nexit 0\n' > "$macos_stage/Agent Overflow.app/Contents/MacOS/agent-overflow"
	chmod +x "$macos_stage/Agent Overflow.app/Contents/MacOS/agent-overflow"
	( cd "$macos_stage" && zip -qry "$dir/agent-overflow-darwin-arm64.zip" "Agent Overflow.app" )
	"$ROOT_DIR/scripts/package-release-assets.sh" "$dir"
	[ ! -e "$dir/agent-overflow.desktop" ] || {
		echo "ERROR: release packaging emitted the redundant Linux desktop entry" >&2
		exit 1
	}
}

make_fake_wsl_tools() {
	bin_dir=$1
	mkdir -p "$bin_dir"
	cat > "$bin_dir/cmd.exe" <<'EOF'
#!/usr/bin/env sh
printf 'C:\\Users\\Test\\AppData\\Local\r\n'
EOF
	chmod +x "$bin_dir/cmd.exe"
	cat > "$bin_dir/wslpath" <<EOF
#!/usr/bin/env sh
printf '%s\n' '$TMP_DIR/windows-localappdata'
EOF
	chmod +x "$bin_dir/wslpath"
}

assert_fails() {
	if "$@"; then
		echo "ERROR: expected command to fail: $*" >&2
		exit 1
	fi
}

# These are synthetic amd64 Linux/WSL artifacts. Simulate their target
# architecture so the packaging test runs on an Apple Silicon build host too;
# the installer's real architecture check must remain enabled in production.
real_uname=$(command -v uname)
mkdir -p "$TMP_DIR/tools"
cat > "$TMP_DIR/tools/uname" <<'EOF'
#!/usr/bin/env sh
if [ "${1:-}" = -m ]; then
  echo x86_64
else
  exec "$AO_TEST_REAL_UNAME" "$@"
fi
EOF
chmod +x "$TMP_DIR/tools/uname"
export AO_TEST_REAL_UNAME="$real_uname"
export PATH="$TMP_DIR/tools:$PATH"

release_dir=$TMP_DIR/release
make_fake_release "$release_dir"

echo "==> Linux download dry-run"
"$ROOT_DIR/scripts/install.sh" --linux --download --source "$release_dir" --prefix "$TMP_DIR/linux-prefix" --dry-run >/dev/null

echo "==> macOS download dry-run"
"$ROOT_DIR/scripts/install.sh" --macos --download --source "$release_dir" --dry-run >/dev/null

echo "==> macOS download install"
HOME="$TMP_DIR/macos-home" "$ROOT_DIR/scripts/install.sh" --macos --download --source "$release_dir" >/dev/null
[ -d "$TMP_DIR/macos-home/Applications/Agent Overflow.app" ] || {
	echo "ERROR: macOS install did not copy app bundle" >&2
	exit 1
}

echo "==> WSL download dry-run"
fake_bin=$TMP_DIR/fake-wsl-bin
make_fake_wsl_tools "$fake_bin"
WSL_DISTRO_NAME=TestDistro \
	AO_INSTALL_CMD_EXE="$fake_bin/cmd.exe" \
	AO_INSTALL_APPDATA="C:\\Users\\Test\\AppData\\Roaming" \
	AO_INSTALL_WSLPATH="$fake_bin/wslpath" \
	"$ROOT_DIR/scripts/install.sh" --wsl --download --source "$release_dir" --dry-run >/dev/null

echo "==> Checksum mismatch fails"
bad_dir=$TMP_DIR/bad-release
mkdir -p "$bad_dir"
cp "$release_dir"/* "$bad_dir"/
printf '0000000000000000000000000000000000000000000000000000000000000000  agent-overflow-linux-amd64\n' > "$bad_dir/SHASUMS256"
assert_fails "$ROOT_DIR/scripts/install.sh" --linux --download --source "$bad_dir" --dry-run >/dev/null 2>&1

echo "==> Insecure URL source fails"
assert_fails "$ROOT_DIR/scripts/install.sh" --linux --download --source "http://127.0.0.1/release" --dry-run >/dev/null 2>&1

echo "install download smoke test passed"
