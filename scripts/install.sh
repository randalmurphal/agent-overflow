#!/usr/bin/env sh
set -eu

MODE=""
ARTIFACT=""
PREFIX=""
SYSTEM=0
DRY_RUN=0
UNINSTALL=0

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." 2>/dev/null && pwd || printf '%s' "$SCRIPT_DIR")

usage() {
	cat <<'USAGE'
Usage:
  ./install.sh --linux PATH [--prefix DIR|--system] [--dry-run]
  ./install.sh --macos PATH [--system] [--dry-run]
  ./install.sh --wsl PATH [--dry-run]
  ./install.sh --uninstall --linux|--macos|--wsl [--prefix DIR|--system] [--dry-run]

Installs already-built Agent Overflow artifacts. It does not build from source.
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

[ -n "$MODE" ] || { usage >&2; exit 2; }

run() {
	printf '%s\n' "$*"
	if [ "$DRY_RUN" -eq 0 ]; then
		"$@"
	fi
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
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT HUP INT TERM
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
	trap - EXIT HUP INT TERM
}

asset_path() {
	name=$1
	if [ -f "$SCRIPT_DIR/$name" ]; then
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

	[ -n "$ARTIFACT" ] || { echo "ERROR: --linux requires a binary path" >&2; exit 2; }
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

	[ -n "$ARTIFACT" ] || { echo "ERROR: --macos requires an .app or .zip path" >&2; exit 2; }
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
			trap 'rm -rf "$tmp"' EXIT
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

install_wsl() {
	[ -n "${WSL_DISTRO_NAME:-}" ] || { echo "ERROR: --wsl must be run from inside WSL" >&2; exit 1; }
	wslpath_bin=${AO_INSTALL_WSLPATH:-wslpath}
	command -v "$wslpath_bin" >/dev/null 2>&1 || { echo "ERROR: wslpath not found" >&2; exit 1; }
	localappdata=$(resolve_windows_localappdata)
	[ -n "$localappdata" ] || { echo "ERROR: could not resolve %LOCALAPPDATA%" >&2; exit 1; }
	localappdata_linux=$("$wslpath_bin" -u "$localappdata")
	install_dir=$localappdata_linux/Programs/Agent\ Overflow
	exe_path=$install_dir/agent-overflow.exe

	if [ "$UNINSTALL" -eq 1 ]; then
		run rm -f "$exe_path"
		printf '%s\n' "rmdir $install_dir"
		if [ "$DRY_RUN" -eq 0 ]; then
			rmdir "$install_dir" 2>/dev/null || true
		fi
		return
	fi

	[ -n "$ARTIFACT" ] || { echo "ERROR: --wsl requires agent-overflow.exe path" >&2; exit 2; }
	copy_mode "$ARTIFACT" "$exe_path" 0755
	printf 'Installed Agent Overflow launcher to %s\n' "$localappdata\\Programs\\Agent Overflow\\agent-overflow.exe"
	printf 'Launcher config and logs live under %%APPDATA%%\\agent-overflow\n'
}

case "$MODE" in
	linux) install_linux ;;
	macos) install_macos ;;
	wsl) install_wsl ;;
	*) echo "ERROR: unsupported mode: $MODE" >&2; exit 2 ;;
esac
