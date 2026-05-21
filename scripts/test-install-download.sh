#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT HUP INT TERM

hash_file() {
	file=$1
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$file" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$file" | awk '{print $1}'
	else
		echo "ERROR: sha256sum or shasum is required" >&2
		exit 1
	fi
}

write_checksums() {
	dir=$1
	(
		cd "$dir"
		rm -f SHASUMS256
		for file in AgentOverflow-macos.zip agent-overflow-linux-amd64 agent-overflow-wsl-amd64.exe appicon.png; do
			printf '%s  %s\n' "$(hash_file "$file")" "$file"
		done > SHASUMS256
	)
}

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
	( cd "$macos_stage" && zip -qr "$dir/AgentOverflow-macos.zip" "Agent Overflow.app" )
	cp "$ROOT_DIR/build/appicon.png" "$dir/appicon.png"
	write_checksums "$dir"
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
