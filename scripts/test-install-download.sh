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

make_fake_noremote_release() {
	dir=$1
	mkdir -p "$dir"
	printf 'fake windows launcher without remote access\n' > "$dir/agent-overflow-wsl-noremote-amd64.exe"
	"$ROOT_DIR/scripts/package-release-assets.sh" "$dir" --wsl-only
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
# Every source below is a local directory or a refused URL, so nothing may
# reach the network. These stand-ins record any download attempt.
network_marker=$TMP_DIR/network-attempted
for tool in curl wget; do
	printf '#!/usr/bin/env sh\necho "$0 $*" >> "%s"\nexit 1\n' "$network_marker" > "$TMP_DIR/tools/$tool"
	chmod +x "$TMP_DIR/tools/$tool"
done
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

run_wsl_install() {
	WSL_DISTRO_NAME=TestDistro \
		AO_INSTALL_CMD_EXE="$fake_bin/cmd.exe" \
		AO_INSTALL_APPDATA="C:\\Users\\Test\\AppData\\Roaming" \
		AO_INSTALL_WSLPATH="$fake_bin/wslpath" \
		"$ROOT_DIR/scripts/install.sh" "$@"
}

noremote_dir=$TMP_DIR/release-noremote
make_fake_noremote_release "$noremote_dir"

echo "==> WSL noremote download dry-run"
noremote_out=$(run_wsl_install --wsl --noremote --download --source "$noremote_dir" --dry-run)
printf '%s\n' "$noremote_out" | grep -q 'agent-overflow-wsl-noremote-amd64.exe' || {
	echo "ERROR: --noremote did not install the noremote launcher" >&2
	printf '%s\n' "$noremote_out" >&2
	exit 1
}
if printf '%s\n' "$noremote_out" | grep -q 'agent-overflow-wsl-amd64.exe'; then
	echo "ERROR: --noremote installed the standard launcher" >&2
	exit 1
fi

echo "==> WSL standard download does not take the noremote launcher"
standard_out=$(run_wsl_install --wsl --download --source "$release_dir" --dry-run)
if printf '%s\n' "$standard_out" | grep -q 'noremote'; then
	echo "ERROR: the standard install named the noremote launcher" >&2
	exit 1
fi

echo "==> WSL noremote from a standard release fails"
assert_fails run_wsl_install --wsl --noremote --download --source "$release_dir" --dry-run >/dev/null 2>&1

echo "==> WSL noremote checksum mismatch fails"
bad_noremote=$TMP_DIR/bad-release-noremote
mkdir -p "$bad_noremote"
cp "$noremote_dir"/* "$bad_noremote"/
printf '0000000000000000000000000000000000000000000000000000000000000000  ./agent-overflow-wsl-noremote-amd64.exe\n' > "$bad_noremote/SHASUMS256"
assert_fails run_wsl_install --wsl --noremote --download --source "$bad_noremote" --dry-run >/dev/null 2>&1

echo "==> WSL noremote download without a source fails"
assert_fails run_wsl_install --wsl --noremote --download --dry-run >/dev/null 2>&1

echo "==> noremote outside WSL fails"
assert_fails "$ROOT_DIR/scripts/install.sh" --linux --noremote --download --source "$release_dir" --prefix "$TMP_DIR/linux-prefix" --dry-run >/dev/null 2>&1

echo "==> Checksum mismatch fails"
bad_dir=$TMP_DIR/bad-release
mkdir -p "$bad_dir"
cp "$release_dir"/* "$bad_dir"/
printf '0000000000000000000000000000000000000000000000000000000000000000  agent-overflow-linux-amd64\n' > "$bad_dir/SHASUMS256"
assert_fails "$ROOT_DIR/scripts/install.sh" --linux --download --source "$bad_dir" --dry-run >/dev/null 2>&1

echo "==> Insecure URL source fails"
assert_fails "$ROOT_DIR/scripts/install.sh" --linux --download --source "http://127.0.0.1/release" --dry-run >/dev/null 2>&1

# gitlab: sources read releases through glab. This stand-in serves releases
# from $glab_releases/TAG/ and behaves like the real CLI where the installer
# depends on it: `release download` needs an existing -D directory and exits 0
# without writing anything when no asset has the name.
glab_bin=$TMP_DIR/fake-glab-bin
glab_releases=$TMP_DIR/glab-releases
glab_log=$TMP_DIR/glab.log
mkdir -p "$glab_bin"
cat > "$glab_bin/glab" <<'EOF'
#!/usr/bin/env sh
printf '%s\n' "$*" >> "$AO_TEST_GLAB_LOG"
case "${1:-} ${2:-}" in
	"api --hostname")
		[ "$3" = example.test ] || { echo "fake glab: unexpected host $3" >&2; exit 1; }
		[ "$4" = "projects/group%2Fsub%2Fagent-overflow/releases/permalink/latest" ] || { echo "fake glab: unexpected path $4" >&2; exit 1; }
		printf '{"name":"Agent Overflow","tag_name":"%s","description":"notes, \\"tag_name\\":\\"v9.9.9\\""}\n' "$AO_TEST_GLAB_LATEST"
		exit 0
		;;
	"release download")
		tag=$3
		shift 3
		while [ "$#" -gt 0 ]; do
			case "$1" in
				-R) repo=$2; shift 2 ;;
				-n) name=$2; shift 2 ;;
				-D) dir=$2; shift 2 ;;
				*) echo "fake glab: unexpected argument $1" >&2; exit 1 ;;
			esac
		done
		[ "$repo" = https://example.test/group/sub/agent-overflow ] || { echo "fake glab: unexpected repo $repo" >&2; exit 1; }
		[ -d "$dir" ] || { echo "fake glab: open $dir/$name: no such file or directory" >&2; exit 1; }
		if [ -f "$AO_TEST_GLAB_RELEASES/$tag/$name" ]; then
			cp "$AO_TEST_GLAB_RELEASES/$tag/$name" "$dir/$name"
		else
			echo "no release assets found!"
		fi
		exit 0
		;;
esac
echo "fake glab: unexpected command $*" >&2
exit 1
EOF
chmod +x "$glab_bin/glab"
for tag in v0.0.1 v0.0.2; do
	mkdir -p "$glab_releases/$tag"
	cp "$noremote_dir"/* "$glab_releases/$tag"/
done
mkdir -p "$glab_releases/v0.0.3" "$glab_releases/v0.0.4"
cp "$noremote_dir/SHASUMS256" "$glab_releases/v0.0.3"/
cp "$bad_noremote"/* "$glab_releases/v0.0.4"/
gitlab_source=gitlab:example.test/group/sub/agent-overflow

# run_gitlab_install runs the installer with the fake glab and a private
# TMPDIR, and fails if the installer leaves anything in it.
run_gitlab_install() {
	gitlab_tmp=$TMP_DIR/gitlab-tmpdir
	rm -rf "$gitlab_tmp" "$glab_log"
	mkdir -p "$gitlab_tmp"
	status=0
	PATH="$glab_bin:$PATH" TMPDIR="$gitlab_tmp" \
		AO_TEST_GLAB_LOG="$glab_log" AO_TEST_GLAB_RELEASES="$glab_releases" AO_TEST_GLAB_LATEST=v0.0.2 \
		run_wsl_install "$@" || status=$?
	if [ -n "$(ls -A "$gitlab_tmp")" ]; then
		echo "ERROR: the installer left files behind:" >&2
		ls -A "$gitlab_tmp" >&2
		exit 1
	fi
	return "$status"
}

echo "==> WSL noremote from a GitLab project's latest release"
gitlab_out=$(run_gitlab_install --wsl --noremote --source "$gitlab_source" --dry-run)
printf '%s\n' "$gitlab_out" | grep -q 'agent-overflow-wsl-noremote-amd64.exe' || {
	echo "ERROR: the gitlab: source did not install the noremote launcher" >&2
	printf '%s\n' "$gitlab_out" >&2
	exit 1
}
[ "$(grep -c '^api ' "$glab_log")" -eq 1 ] || { echo "ERROR: the latest release was not resolved exactly once" >&2; cat "$glab_log" >&2; exit 1; }
[ "$(grep -c '^release download v0.0.2 ' "$glab_log")" -eq 2 ] || { echo "ERROR: the files did not both come from the latest release" >&2; cat "$glab_log" >&2; exit 1; }

echo "==> WSL noremote from a GitLab release named by --version"
run_gitlab_install --wsl --noremote --source "$gitlab_source" --version 0.0.1 --dry-run >/dev/null
! grep -q '^api ' "$glab_log" || { echo "ERROR: --version still looked up the latest release" >&2; exit 1; }
[ "$(grep -c '^release download v0.0.1 ' "$glab_log")" -eq 2 ] || { echo "ERROR: --version 0.0.1 did not download v0.0.1" >&2; cat "$glab_log" >&2; exit 1; }

echo "==> WSL noremote with the installer piped from glab"
gitlab_piped=$(PATH="$glab_bin:$PATH" AO_TEST_GLAB_LOG="$glab_log" AO_TEST_GLAB_RELEASES="$glab_releases" AO_TEST_GLAB_LATEST=v0.0.2 \
	WSL_DISTRO_NAME=TestDistro \
	AO_INSTALL_CMD_EXE="$fake_bin/cmd.exe" \
	AO_INSTALL_APPDATA="C:\\Users\\Test\\AppData\\Roaming" \
	AO_INSTALL_WSLPATH="$fake_bin/wslpath" \
	sh -s -- --wsl --noremote --source "$gitlab_source" --dry-run < "$ROOT_DIR/scripts/install.sh")
printf '%s\n' "$gitlab_piped" | grep -q 'agent-overflow-wsl-noremote-amd64.exe' || {
	echo "ERROR: the piped installer did not install the noremote launcher" >&2
	printf '%s\n' "$gitlab_piped" >&2
	exit 1
}

# assert_gitlab_fails MESSAGE ARGS... runs a gitlab: install that must fail
# with MESSAGE on stderr.
assert_gitlab_fails() {
	message=$1
	shift
	gitlab_err=$TMP_DIR/gitlab.err
	if run_gitlab_install "$@" >/dev/null 2>"$gitlab_err"; then
		echo "ERROR: expected the install to fail: $*" >&2
		exit 1
	fi
	grep -q -- "$message" "$gitlab_err" || {
		echo "ERROR: expected \"$message\" from: $*" >&2
		cat "$gitlab_err" >&2
		exit 1
	}
}

echo "==> GitLab release without the launcher fails"
assert_gitlab_fails 'has no file named agent-overflow-wsl-noremote-amd64.exe' --wsl --noremote --source "$gitlab_source" --version 0.0.3 --dry-run

echo "==> GitLab release checksum mismatch fails"
assert_gitlab_fails 'checksum mismatch' --wsl --noremote --source "$gitlab_source" --version 0.0.4 --dry-run

echo "==> Malformed GitLab sources fail"
assert_gitlab_fails 'needs HOST/GROUP/PROJECT' --wsl --noremote --source gitlab:example.test/agent-overflow --dry-run
for bad_source in gitlab:example.test/group/../agent-overflow 'gitlab:example.test/group/agent overflow' gitlab:example.test//group/agent-overflow gitlab:example.test/group/agent-overflow/; do
	assert_gitlab_fails 'unsafe GitLab project' --wsl --noremote --source "$bad_source" --dry-run
done

echo "==> gitlab: source without glab fails"
no_glab_bin=$TMP_DIR/no-glab-bin
mkdir -p "$no_glab_bin"
for tool in sh dirname mktemp rm; do
	ln -s "$(command -v "$tool")" "$no_glab_bin/$tool"
done
ln -s "$TMP_DIR/tools/uname" "$no_glab_bin/uname"
no_glab_err=$TMP_DIR/no-glab.err
if PATH="$no_glab_bin" WSL_DISTRO_NAME=TestDistro "$(command -v sh)" "$ROOT_DIR/scripts/install.sh" \
	--wsl --noremote --source "$gitlab_source" --dry-run >/dev/null 2>"$no_glab_err"; then
	echo "ERROR: a gitlab: source without glab succeeded" >&2
	exit 1
fi
grep -q 'needs the GitLab CLI' "$no_glab_err" || { echo "ERROR: missing glab was not reported:" >&2; cat "$no_glab_err" >&2; exit 1; }

[ ! -e "$network_marker" ] || {
	echo "ERROR: the installer tried to download:" >&2
	cat "$network_marker" >&2
	exit 1
}

echo "install download smoke test passed"
