#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=""
SKIP_MACOS=0
SKIP_WSL=0

usage() {
	cat <<'USAGE'
Usage: scripts/build-release.sh [--version VERSION] [--skip-macos] [--skip-wsl]

Builds release artifacts from the current checkout into dist/release/VERSION.
The tree must be clean before and after the build so generated metadata cannot
silently drift.
USAGE
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version)
			VERSION=${2:-}
			[ -n "$VERSION" ] || { echo "ERROR: --version requires a value" >&2; exit 2; }
			shift 2
			;;
		--skip-macos)
			SKIP_MACOS=1
			shift
			;;
		--skip-wsl)
			SKIP_WSL=1
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

if [ -z "$VERSION" ]; then
	VERSION=$(sed -n 's/^  version: "\([^"]*\)"/\1/p' "$ROOT_DIR/build/config.yml")
fi
[ -n "$VERSION" ] || { echo "ERROR: could not read build/config.yml info.version" >&2; exit 1; }

validate_version() {
	case "$VERSION" in
		""|.*|-*|*..*|*[!0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz._+-]*)
			echo "ERROR: unsafe release version: $VERSION" >&2
			exit 2
			;;
	esac
}

require_clean_tree() {
	if [ -n "$(git -C "$ROOT_DIR" status --short)" ]; then
		echo "ERROR: git tree is dirty; commit or stash changes before building release artifacts." >&2
		git -C "$ROOT_DIR" status --short >&2
		exit 1
	fi
}

sync_version() {
	"$ROOT_DIR/scripts/sync-release-version.sh" "$VERSION"
	if [ -n "$(git -C "$ROOT_DIR" status --short)" ]; then
		echo "ERROR: release metadata was not synced for version $VERSION." >&2
		echo "Run ./scripts/sync-release-version.sh $VERSION, review the changes, and commit them before building." >&2
		git -C "$ROOT_DIR" status --short >&2
		exit 1
	fi
}

copy_file() {
	src=$1
	dst=$2
	[ -f "$src" ] || { echo "ERROR: missing expected artifact: $src" >&2; exit 1; }
	mkdir -p "$(dirname "$dst")"
	cp "$src" "$dst"
}

validate_elf() {
	path=$1
	[ -f "$path" ] || { echo "ERROR: missing Linux payload: $path" >&2; exit 1; }
	size=$(wc -c < "$path" | tr -d ' ')
	[ "$size" -gt 1048576 ] || { echo "ERROR: Linux payload is too small; refusing placeholder: $path" >&2; exit 1; }
	if ! LC_ALL=C dd if="$path" bs=4 count=1 2>/dev/null | od -An -tx1 | grep -q '7f 45 4c 46'; then
		echo "ERROR: Linux payload is not an ELF executable: $path" >&2
		exit 1
	fi
}

validate_launcher() {
	path=$1
	[ -f "$path" ] || { echo "ERROR: missing Windows launcher: $path" >&2; exit 1; }
	if LC_ALL=C strings "$path" | grep -q 'PLACEHOLDER - replace with cross-compiled Linux ELF before shipping'; then
		echo "ERROR: Windows launcher contains the placeholder Linux payload: $path" >&2
		exit 1
	fi
}

# The darwin zip is arch-labeled and the in-app updater matches assets by that
# label, so a mislabeled slice would ship an unrunnable build to updaters. The
# bundle must be UNIVERSAL (arm64 + x86_64) before it may be published under an
# arch name — the check stays even though only arm64 is published, because the
# asset name promises a bundle that runs on the machines the label covers.
validate_universal_macho() {
	path=$1
	[ -f "$path" ] || { echo "ERROR: missing macOS binary: $path" >&2; exit 1; }
	if command -v lipo >/dev/null 2>&1; then
		archs=$(lipo -archs "$path" 2>/dev/null || true)
		case "$archs" in
			*arm64*x86_64*|*x86_64*arm64*) ;;
			*)
				echo "ERROR: macOS binary is not universal (archs: ${archs:-unreadable}); refusing to publish arch-labeled assets: $path" >&2
				exit 1
				;;
		esac
	else
		desc=$(file -b "$path" 2>/dev/null || true)
		case "$desc" in
			*universal*) ;;
			*)
				echo "ERROR: macOS binary is not universal (file: ${desc:-unreadable}); refusing to publish arch-labeled assets: $path" >&2
				exit 1
				;;
		esac
	fi
}

# The universal bundle ships under the darwin-arm64 name only — Intel Macs are
# no longer a target. The name stays exactly that: the updater's asset matcher
# requires BOTH the platform and arch tokens, so a "universal"-named zip would
# match nobody and a rename would strand already-shipped clients.
package_darwin_zips() {
	validate_universal_macho "$ROOT_DIR/bin/agent-overflow.app/Contents/MacOS/agent-overflow"
	( cd "$ROOT_DIR/bin" && zip -qr "$OUT_DIR/agent-overflow-darwin-arm64.zip" "agent-overflow.app" )
}

validate_version
require_clean_tree
sync_version

OUT_DIR="$ROOT_DIR/dist/release/$VERSION"
release_root="$ROOT_DIR/dist/release"
mkdir -p "$release_root"
case "$(cd "$release_root" && pwd)/$VERSION" in
	"$(cd "$release_root" && pwd)"/*) ;;
	*) echo "ERROR: refusing to write outside dist/release: $OUT_DIR" >&2; exit 1 ;;
esac
rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

echo "==> Building Linux artifact"
case "$(uname -s):$(uname -m)" in
	Linux:x86_64|Linux:amd64)
		make -C "$ROOT_DIR" build VERSION="$VERSION"
		validate_elf "$ROOT_DIR/bin/agent-overflow"
		copy_file "$ROOT_DIR/bin/agent-overflow" "$OUT_DIR/agent-overflow-linux-amd64"
		;;
	*)
		echo "ERROR: Linux amd64 release artifacts must be built on Linux amd64/WSL." >&2
		echo "       Rerun there, or add an explicit cross-build path before releasing." >&2
		exit 1
		;;
esac

if [ "$SKIP_WSL" -eq 0 ]; then
	case "$(uname -s)" in
		Linux)
			echo "==> Building Windows WSL launcher"
			make -C "$ROOT_DIR" build-wsl WSL_VERSION="$VERSION" WSL_FORCE_RELINK=1
			validate_elf "$ROOT_DIR/bin/agent-overflow-linux"
			validate_launcher "$ROOT_DIR/bin/agent-overflow.exe"
			copy_file "$ROOT_DIR/bin/agent-overflow.exe" "$OUT_DIR/agent-overflow-wsl-amd64.exe"
			;;
		*)
			echo "==> Skipping WSL launcher build on $(uname -s); rerun on Linux/WSL for Windows artifacts."
			;;
	esac
fi

if [ "$SKIP_MACOS" -eq 0 ]; then
	case "$(uname -s)" in
		Darwin)
			echo "==> Building macOS app bundle (universal)"
			( cd "$ROOT_DIR" && VERSION="$VERSION" wails3 task darwin:package:universal )
			package_darwin_zips
			;;
		*)
			if command -v docker >/dev/null 2>&1 && docker image inspect wails-cross >/dev/null 2>&1; then
				echo "==> Building macOS app bundle with Docker cross image (universal)"
				( cd "$ROOT_DIR" && VERSION="$VERSION" wails3 task darwin:package:universal )
				package_darwin_zips
			else
				echo "==> Skipping macOS app bundle; Docker image wails-cross is not available."
			fi
			;;
	esac
fi

"$ROOT_DIR/scripts/package-release-assets.sh" "$OUT_DIR"

require_clean_tree
echo "Release artifacts written to $OUT_DIR"
