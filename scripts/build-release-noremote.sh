#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=""
UPDATE_SOURCE=""

usage() {
	cat <<'USAGE'
Usage: scripts/build-release-noremote.sh --update-source HOST/GROUP/PROJECT [--version VERSION]

Builds the Windows/WSL release without remote access (the `noremote` build
tag, internal/buildvariant) into dist/release-noremote/VERSION. The in-app
updater of this build installs only agent-overflow-wsl-noremote-amd64.exe
from the releases of the GitLab project HOST/GROUP/PROJECT, read through the
user's glab login. The tree must be clean before and after the build.
USAGE
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version)
			VERSION=${2:-}
			[ -n "$VERSION" ] || { echo "ERROR: --version requires a value" >&2; exit 2; }
			shift 2
			;;
		--update-source)
			UPDATE_SOURCE=${2:-}
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

# Same release stamping as scripts/build-release.sh.
export AO_RELEASE_BUILD=1

if [ -z "$VERSION" ]; then
	VERSION=$(sed -n 's/^  version: "\([^"]*\)"/\1/p' "$ROOT_DIR/build/config.yml")
fi
[ -n "$VERSION" ] || { echo "ERROR: could not read build/config.yml info.version" >&2; exit 1; }

. "$ROOT_DIR/scripts/release-common.sh"

# The project path is linked into the binary and handed to `glab api`, so it
# is held to a plain host/namespace/project shape here. The updater validates
# the same shape at startup.
case "$UPDATE_SOURCE" in
	"")
		echo "ERROR: --update-source HOST/GROUP/PROJECT is required" >&2
		exit 2
		;;
	*[!0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz._/-]*|/*|*/|*//*|*..*)
		echo "ERROR: unsafe update source: $UPDATE_SOURCE" >&2
		exit 2
		;;
	*/*/*) ;;
	*)
		echo "ERROR: update source must be HOST/GROUP/PROJECT: $UPDATE_SOURCE" >&2
		exit 2
		;;
esac

# The release must not link the remote-access dependencies at all. `go version
# -m` reads the module list Go recorded in each binary.
require_no_remote_modules() {
	path=$1
	modules=$(go version -m "$path")
	for forbidden in tailscale.com github.com/hashicorp/mdns; do
		if printf '%s\n' "$modules" | awk '$1 == "dep" || $1 == "=>" { print $2 }' | grep -qx "$forbidden"; then
			echo "ERROR: $path links $forbidden; it was not built with -tags noremote" >&2
			exit 1
		fi
	done
	if ! printf '%s\n' "$modules" | grep -q -- '-tags=.*noremote'; then
		echo "ERROR: $path was not built with -tags noremote" >&2
		exit 1
	fi
}

validate_version
require_clean_tree
sync_version

OUT_DIR="$ROOT_DIR/dist/release-noremote/$VERSION"
release_root="$ROOT_DIR/dist/release-noremote"
mkdir -p "$release_root"
case "$(cd "$release_root" && pwd)/$VERSION" in
	"$(cd "$release_root" && pwd)"/*) ;;
	*) echo "ERROR: refusing to write outside dist/release-noremote: $OUT_DIR" >&2; exit 1 ;;
esac
rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

case "$(uname -s):$(uname -m)" in
	Linux:x86_64|Linux:amd64) ;;
	*)
		echo "ERROR: the WSL release must be built on Linux amd64/WSL." >&2
		exit 1
		;;
esac

echo "==> Building Windows WSL launcher without remote access"
make -C "$ROOT_DIR" build-wsl WSL_VERSION="$VERSION" WSL_FORCE_RELINK=1 \
	WSL_GO_TAGS=noremote \
	WSL_LDFLAGS="-X agent-overflow/internal/appupdate.gitlabProject=$UPDATE_SOURCE"
validate_elf "$ROOT_DIR/bin/agent-overflow-linux"
validate_launcher "$ROOT_DIR/bin/agent-overflow.exe"
require_no_remote_modules "$ROOT_DIR/bin/agent-overflow-linux"
require_no_remote_modules "$ROOT_DIR/bin/agent-overflow.exe"
copy_file "$ROOT_DIR/bin/agent-overflow.exe" "$OUT_DIR/agent-overflow-wsl-noremote-amd64.exe"

"$ROOT_DIR/scripts/package-release-assets.sh" "$OUT_DIR" --wsl-only

require_clean_tree
