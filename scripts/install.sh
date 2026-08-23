#!/usr/bin/env sh
set -eu

MODE=""
ARTIFACT=""
PREFIX=""
SYSTEM=0
DRY_RUN=0
UNINSTALL=0
DOWNLOAD=0
VERSION=""
SOURCE=""
REPO=${AO_INSTALL_REPO:-randalmurphal/agent-overflow}
DOWNLOAD_DIR=""
ASSET_DIR=""
CLEANUP_PATHS=""

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." 2>/dev/null && pwd || printf '%s' "$SCRIPT_DIR")

usage() {
	cat <<'USAGE'
Usage:
  ./install.sh [--linux|--macos|--wsl] [PATH] [--download] [--version VERSION] [--source DIR_OR_URL] [--repo OWNER/REPO] [--dry-run]
  ./install.sh --linux PATH [--prefix DIR|--system] [--dry-run]
  ./install.sh --macos PATH [--system] [--dry-run]
  ./install.sh --wsl PATH [--dry-run]
  ./install.sh --uninstall [--linux|--macos|--wsl] [--prefix DIR|--system] [--dry-run]

With no PATH, the installer auto-detects the platform, downloads the matching
GitHub release artifact, verifies SHASUMS256, and installs it.

Examples:
  curl -fsSL https://github.com/randalmurphal/agent-overflow/releases/latest/download/install.sh | sh
  curl -fsSL https://github.com/randalmurphal/agent-overflow/releases/download/v0.0.1/install.sh | sh -s -- --version 0.0.1
  ./scripts/install.sh --wsl --download --source ./dist/release/0.0.1

The installer copies already-built Agent Overflow artifacts. It does not build
from source.
USAGE
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--linux|--macos|--wsl)
			[ -z "$MODE" ] || { echo "ERROR: choose one install mode" >&2; exit 2; }
			MODE=${1#--}
			if [ "${2:-}" ] && [ "${2#--}" = "$2" ]; then
				ARTIFACT=$2
				shift 2
			else
				shift
			fi
			;;
		--prefix)
			PREFIX=${2:-}
			[ -n "$PREFIX" ] || { echo "ERROR: --prefix requires a value" >&2; exit 2; }
			shift 2
			;;
		--system)
			SYSTEM=1
			shift
			;;
		--dry-run)
			DRY_RUN=1
			shift
			;;
		--uninstall)
			UNINSTALL=1
			shift
			;;
		--download)
			DOWNLOAD=1
			shift
			;;
		--version)
			VERSION=${2:-}
			[ -n "$VERSION" ] || { echo "ERROR: --version requires a value" >&2; exit 2; }
			shift 2
			;;
		--source)
			SOURCE=${2:-}
			[ -n "$SOURCE" ] || { echo "ERROR: --source requires a value" >&2; exit 2; }
			shift 2
			;;
		--repo)
			REPO=${2:-}
			[ -n "$REPO" ] || { echo "ERROR: --repo requires a value" >&2; exit 2; }
			shift 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			echo "ERROR: unknown argument: $1" >&2
			usage >&2
			exit 2
			;;
	esac
done

run() {
	printf '%s\n' "$*"
	if [ "$DRY_RUN" -eq 0 ]; then
		"$@"
	fi
}

cleanup() {
	[ -n "$CLEANUP_PATHS" ] || return 0
	printf '%s\n' "$CLEANUP_PATHS" | while IFS= read -r path; do
		[ -n "$path" ] && rm -rf "$path"
	done
}

cleanup_and_exit() {
	cleanup
	exit 1
}

register_cleanup_path() {
	path=$1
	CLEANUP_PATHS=$(printf '%s\n%s' "$CLEANUP_PATHS" "$path")
	trap cleanup EXIT
	trap cleanup_and_exit HUP INT TERM
}

is_url() {
		case "$1" in
			http://*|https://*) return 0 ;;
			*) return 1 ;;
		esac
}

is_inside_wsl() {
	[ -n "${WSL_DISTRO_NAME:-}" ] && return 0
	grep -qi 'microsoft\|wsl' /proc/version 2>/dev/null
}

detect_mode() {
	case "$(uname -s)" in
		Darwin)
			printf '%s\n' macos
			;;
		Linux)
			if is_inside_wsl; then
				printf '%s\n' wsl
			else
				printf '%s\n' linux
			fi
			;;
		*)
			echo "ERROR: could not auto-detect platform; pass --linux, --macos, or --wsl" >&2
			exit 2
			;;
	esac
}

validate_repo() {
	case "$REPO" in
		*/*) ;;
		*) echo "ERROR: --repo must look like OWNER/REPO" >&2; exit 2 ;;
	esac
	case "$REPO" in
		*[!0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz._/-]*)
			echo "ERROR: unsafe GitHub repo: $REPO" >&2
			exit 2
			;;
	esac
}

normalize_version() {
	version=${1:-latest}
	case "$version" in
		latest)
			printf '%s\n' latest
			return
			;;
		v*)
			version=${version#v}
			;;
	esac
	case "$version" in
		""|.*|-*|*..*|*[!0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz._+-]*)
			echo "ERROR: unsafe release version: $version" >&2
			exit 2
			;;
	esac
	printf '%s\n' "$version"
}

require_amd64() {
	case "$(uname -m)" in
		x86_64|amd64) ;;
		*) echo "ERROR: $MODE release artifacts are currently published for amd64 only" >&2; exit 1 ;;
	esac
}

artifact_name_for_mode() {
	case "$MODE" in
		linux)
			require_amd64
			printf '%s\n' agent-overflow-linux-amd64
			;;
		wsl)
			require_amd64
			printf '%s\n' agent-overflow-wsl-amd64.exe
			;;
		macos)
			printf '%s\n' agent-overflow-darwin-arm64.zip
			;;
		*) echo "ERROR: unsupported mode: $MODE" >&2; exit 2 ;;
	esac
}

release_base_url() {
	validate_repo
	version=$(normalize_version "$VERSION")
	if [ "$version" = latest ]; then
		printf 'https://github.com/%s/releases/latest/download\n' "$REPO"
	else
		printf 'https://github.com/%s/releases/download/v%s\n' "$REPO" "$version"
	fi
}

download_url() {
	url=$1
	dst=$2
	printf 'Downloading %s\n' "$url"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 --retry-delay 1 -o "$dst" "$url"
	elif command -v wget >/dev/null 2>&1; then
		wget -O "$dst" "$url"
	else
		echo "ERROR: curl or wget is required to download release artifacts" >&2
		exit 1
	fi
}

copy_release_file() {
	name=$1
	dst=$2
	if [ -n "$SOURCE" ]; then
		if is_url "$SOURCE"; then
			case "$SOURCE" in
				https://*) ;;
				*) echo "ERROR: --source URL must use https" >&2; exit 2 ;;
			esac
			download_url "${SOURCE%/}/$name" "$dst"
		else
			[ -d "$SOURCE" ] || { echo "ERROR: --source directory does not exist: $SOURCE" >&2; exit 1; }
			[ -f "$SOURCE/$name" ] || { echo "ERROR: --source is missing $name" >&2; exit 1; }
			cp "$SOURCE/$name" "$dst"
		fi
	else
		base_url=$(release_base_url)
		download_url "$base_url/$name" "$dst"
	fi
}

hash_file() {
	file=$1
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$file" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$file" | awk '{print $1}'
	else
		echo "ERROR: sha256sum or shasum is required to verify release artifacts" >&2
		exit 1
	fi
}

ps_single_quote() {
	printf '%s' "$1" | sed "s/'/''/g"
}

verify_checksum() {
	file=$1
	name=$2
	checksums=$3
	[ -f "$checksums" ] || { echo "ERROR: missing checksum file: $checksums" >&2; exit 1; }
	expected=$(awk -v file="$name" '($2 == file || $2 == "./" file) { print $1; exit }' "$checksums")
	[ -n "$expected" ] || { echo "ERROR: SHASUMS256 has no entry for $name" >&2; exit 1; }
	actual=$(hash_file "$file")
	if [ "$actual" != "$expected" ]; then
		echo "ERROR: checksum mismatch for $name" >&2
		echo "       expected $expected" >&2
		echo "       actual   $actual" >&2
		exit 1
	fi
}

prepare_download_artifact() {
	release_artifact_name=$(artifact_name_for_mode)
	DOWNLOAD_DIR=$(mktemp -d)
	register_cleanup_path "$DOWNLOAD_DIR"

	copy_release_file SHASUMS256 "$DOWNLOAD_DIR/SHASUMS256"
	copy_release_file "$release_artifact_name" "$DOWNLOAD_DIR/$release_artifact_name"
	verify_checksum "$DOWNLOAD_DIR/$release_artifact_name" "$release_artifact_name" "$DOWNLOAD_DIR/SHASUMS256"

	if [ "$MODE" = linux ]; then
		copy_release_file appicon.png "$DOWNLOAD_DIR/appicon.png"
		verify_checksum "$DOWNLOAD_DIR/appicon.png" appicon.png "$DOWNLOAD_DIR/SHASUMS256"
	fi

	ASSET_DIR=$DOWNLOAD_DIR
	ARTIFACT=$DOWNLOAD_DIR/$release_artifact_name
}

copy_mode() {
	src=$1
	dst=$2
	mode=$3
	[ -f "$src" ] || { echo "ERROR: missing source file: $src" >&2; exit 1; }
	run mkdir -p "$(dirname "$dst")"
	run cp "$src" "$dst"
	run chmod "$mode" "$dst"
}

write_linux_desktop() {
	dst=$1
	binary=$2
	icon=$3
	run mkdir -p "$(dirname "$dst")"
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '%s\n' "write desktop entry $dst"
		printf '%s\n' "  Exec=$binary"
		printf '%s\n' "  Icon=$icon"
		return
	fi
	tmp=$(mktemp "${TMPDIR:-/tmp}/agent-overflow-shortcut.XXXXXX.desktop")
	register_cleanup_path "$tmp"
	cat > "$tmp" <<EOF
[Desktop Entry]
Version=1.0
Type=Application
Name=Agent Overflow
Comment=Desktop app for using coding agents with a shared UX
Exec=$binary
Icon=$icon
Categories=Development;
Terminal=false
Keywords=AI;Agent;Coding;Development;
StartupNotify=true
StartupWMClass=agent-overflow
EOF
	cp "$tmp" "$dst"
	chmod 0644 "$dst"
	rm -f "$tmp"
}

asset_path() {
	name=$1
	if [ -n "$ASSET_DIR" ] && [ -f "$ASSET_DIR/$name" ]; then
		printf '%s/%s\n' "$ASSET_DIR" "$name"
	elif [ -f "$SCRIPT_DIR/$name" ]; then
		printf '%s/%s\n' "$SCRIPT_DIR" "$name"
	elif [ -f "$REPO_ROOT/build/linux/$name" ]; then
		printf '%s/build/linux/%s\n' "$REPO_ROOT" "$name"
	elif [ -f "$REPO_ROOT/build/$name" ]; then
		printf '%s/build/%s\n' "$REPO_ROOT" "$name"
	else
		echo "ERROR: could not find asset $name" >&2
		exit 1
	fi
}

install_linux() {
	if [ "$SYSTEM" -eq 1 ]; then
		bin_dir=${PREFIX:-/usr/local}/bin
		share_dir=${PREFIX:-/usr/local}/share
	else
		prefix=${PREFIX:-"$HOME/.local"}
		bin_dir=$prefix/bin
		if [ -n "$PREFIX" ]; then
			share_dir=$prefix/share
		else
			share_dir=${XDG_DATA_HOME:-"$HOME/.local/share"}
		fi
	fi
	desktop_file=$share_dir/applications/agent-overflow.desktop
	icon_file=$share_dir/icons/hicolor/128x128/apps/agent-overflow.png
	binary=$bin_dir/agent-overflow

	if [ "$UNINSTALL" -eq 1 ]; then
		run rm -f "$binary" "$desktop_file" "$icon_file"
		return
	fi

	[ -n "$ARTIFACT" ] || { echo "ERROR: --linux requires a binary path or --download" >&2; exit 2; }
	copy_mode "$ARTIFACT" "$binary" 0755
	copy_mode "$(asset_path appicon.png)" "$icon_file" 0644
	write_linux_desktop "$desktop_file" "$binary" "$icon_file"
	command -v update-desktop-database >/dev/null 2>&1 && run update-desktop-database "$(dirname "$desktop_file")" || true
	command -v gtk-update-icon-cache >/dev/null 2>&1 && run gtk-update-icon-cache -q "$(dirname "$(dirname "$(dirname "$icon_file")")")" || true
	printf 'Installed Agent Overflow to %s\n' "$binary"
}

install_macos() {
	apps_dir=$HOME/Applications
	[ "$SYSTEM" -eq 0 ] || apps_dir=/Applications
	dest=$apps_dir/Agent\ Overflow.app

	if [ "$UNINSTALL" -eq 1 ]; then
		run rm -rf "$dest"
		return
	fi

	[ -n "$ARTIFACT" ] || { echo "ERROR: --macos requires an .app/.zip path or --download" >&2; exit 2; }
	run mkdir -p "$apps_dir"
	case "$ARTIFACT" in
		*.app)
			run rm -rf "$dest"
			run cp -R "$ARTIFACT" "$dest"
			;;
		*.zip)
			if [ "$DRY_RUN" -eq 1 ]; then
				printf '%s\n' "unzip -q $ARTIFACT -d <tempdir>"
				printf '%s\n' "rm -rf $dest"
				printf '%s\n' "cp -R <tempdir>/Agent Overflow.app $dest"
				printf 'Installed Agent Overflow to %s\n' "$dest"
				return
			fi
			tmp=$(mktemp -d)
			register_cleanup_path "$tmp"
			run unzip -q "$ARTIFACT" -d "$tmp"
			app=$(find "$tmp" -maxdepth 2 -name '*.app' -type d | head -n 1)
			[ -n "$app" ] || { echo "ERROR: zip did not contain an .app bundle" >&2; exit 1; }
			run rm -rf "$dest"
			run cp -R "$app" "$dest"
			;;
		*)
			echo "ERROR: --macos artifact must be an .app directory or .zip" >&2
			exit 2
			;;
	esac
	printf 'Installed Agent Overflow to %s\n' "$dest"
}

resolve_windows_localappdata() {
	if [ -n "${AO_INSTALL_LOCALAPPDATA:-}" ]; then
		printf '%s\n' "$AO_INSTALL_LOCALAPPDATA"
		return
	fi
	cmd_exe=${AO_INSTALL_CMD_EXE:-/mnt/c/Windows/System32/cmd.exe}
	[ -x "$cmd_exe" ] || { echo "ERROR: cmd.exe is not reachable; run --wsl from inside WSL with interop enabled" >&2; exit 1; }
	"$cmd_exe" /c 'echo %LOCALAPPDATA%' 2>/dev/null | tr -d '\r\n'
}

resolve_windows_appdata() {
	if [ -n "${AO_INSTALL_APPDATA:-}" ]; then
		printf '%s\n' "$AO_INSTALL_APPDATA"
		return
	fi
	cmd_exe=${AO_INSTALL_CMD_EXE:-/mnt/c/Windows/System32/cmd.exe}
	[ -x "$cmd_exe" ] || { echo "ERROR: cmd.exe is not reachable; run --wsl from inside WSL with interop enabled" >&2; exit 1; }
	"$cmd_exe" /c 'echo %APPDATA%' 2>/dev/null | tr -d '\r\n'
}

create_windows_shortcut() {
	target_win=$1
	shortcut_win=$2
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '%s\n' "create shortcut $shortcut_win -> $target_win"
		return
	fi

	powershell_exe=${AO_INSTALL_POWERSHELL:-/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe}
	[ -x "$powershell_exe" ] || { echo "ERROR: powershell.exe is not reachable; could not create Start Menu shortcut" >&2; exit 1; }
	command -v iconv >/dev/null 2>&1 || { echo "ERROR: iconv is required to create the Windows Start Menu shortcut" >&2; exit 1; }
	command -v base64 >/dev/null 2>&1 || { echo "ERROR: base64 is required to create the Windows Start Menu shortcut" >&2; exit 1; }

	target_ps=$(ps_single_quote "$target_win")
	shortcut_ps=$(ps_single_quote "$shortcut_win")
ps_script=$(cat <<EOF
\$ErrorActionPreference = 'Stop'
\$ProgressPreference = 'SilentlyContinue'
\$target = '$target_ps'
\$shortcut = '$shortcut_ps'
\$shortcutDir = Split-Path -Parent \$shortcut
New-Item -ItemType Directory -Force -Path \$shortcutDir | Out-Null
\$shell = New-Object -ComObject WScript.Shell
\$link = \$shell.CreateShortcut(\$shortcut)
\$link.TargetPath = \$target
\$link.WorkingDirectory = Split-Path -Parent \$target
\$link.IconLocation = "\$target,0"
\$link.Save()
EOF
)
	encoded_script=$(printf '%s' "$ps_script" | iconv -f UTF-8 -t UTF-16LE | base64 | tr -d '\n')
	"$powershell_exe" -NoProfile -NonInteractive -OutputFormat Text -ExecutionPolicy Bypass -EncodedCommand "$encoded_script"
	shortcut_linux=$("$wslpath_bin" -u "$shortcut_win")
	[ -f "$shortcut_linux" ] || { echo "ERROR: PowerShell did not create Start Menu shortcut: $shortcut_win" >&2; exit 1; }
}

install_wsl() {
	is_inside_wsl || { echo "ERROR: --wsl must be run from inside WSL" >&2; exit 1; }
	wslpath_bin=${AO_INSTALL_WSLPATH:-wslpath}
	command -v "$wslpath_bin" >/dev/null 2>&1 || { echo "ERROR: wslpath not found" >&2; exit 1; }
	localappdata=$(resolve_windows_localappdata)
	[ -n "$localappdata" ] || { echo "ERROR: could not resolve %LOCALAPPDATA%" >&2; exit 1; }
	appdata=$(resolve_windows_appdata)
	[ -n "$appdata" ] || { echo "ERROR: could not resolve %APPDATA%" >&2; exit 1; }
	localappdata_linux=$("$wslpath_bin" -u "$localappdata")
	appdata_linux=$("$wslpath_bin" -u "$appdata")
	install_dir=$localappdata_linux/Programs/Agent\ Overflow
	exe_path=$install_dir/agent-overflow.exe
	exe_win=$localappdata\\Programs\\Agent\ Overflow\\agent-overflow.exe
	shortcut_win=$appdata\\Microsoft\\Windows\\Start\ Menu\\Programs\\Agent\ Overflow.lnk
	shortcut_path=$appdata_linux/Microsoft/Windows/Start\ Menu/Programs/Agent\ Overflow.lnk

	if [ "$UNINSTALL" -eq 1 ]; then
		run rm -f "$exe_path" "$shortcut_path"
		printf '%s\n' "rmdir $install_dir"
		if [ "$DRY_RUN" -eq 0 ]; then
			rmdir "$install_dir" 2>/dev/null || true
		fi
		return
	fi

	[ -n "$ARTIFACT" ] || { echo "ERROR: --wsl requires agent-overflow.exe path or --download" >&2; exit 2; }
	copy_mode "$ARTIFACT" "$exe_path" 0755
	create_windows_shortcut "$exe_win" "$shortcut_win"
	printf 'Installed Agent Overflow launcher to %s\n' "$exe_win"
	printf 'Created Start Menu shortcut at %s\n' "$shortcut_win"
	printf 'Launcher config and logs live under %%APPDATA%%\\agent-overflow\n'
}

if [ -z "$MODE" ]; then
	MODE=$(detect_mode)
fi

if [ "$UNINSTALL" -eq 0 ] && [ -z "$ARTIFACT" ]; then
	DOWNLOAD=1
fi

if [ "$UNINSTALL" -eq 0 ] && [ "$DOWNLOAD" -eq 1 ]; then
	prepare_download_artifact
fi

case "$MODE" in
	linux) install_linux ;;
	macos) install_macos ;;
	wsl) install_wsl ;;
	*) echo "ERROR: unsupported mode: $MODE" >&2; exit 2 ;;
esac
