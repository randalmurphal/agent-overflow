#!/usr/bin/env sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT_DIR=${1:-}

usage() {
	cat <<'USAGE'
Usage: scripts/package-release-assets.sh OUT_DIR

Copies install-time assets into an existing release directory and writes a
unified SHASUMS256 for every file in that directory.
USAGE
}

[ -n "$OUT_DIR" ] || { usage >&2; exit 2; }
[ -d "$OUT_DIR" ] || { echo "ERROR: release directory does not exist: $OUT_DIR" >&2; exit 1; }

copy_file() {
	src=$1
	dst=$2
	[ -f "$src" ] || { echo "ERROR: missing expected asset: $src" >&2; exit 1; }
	cp "$src" "$dst"
}

checksum_dir() {
	dir=$1
	(
		cd "$dir"
		rm -f SHASUMS256
		hash_cmd=sha256sum
		if ! command -v sha256sum >/dev/null 2>&1; then
			hash_cmd="shasum -a 256"
		fi
		find . -maxdepth 1 -type f ! -name SHASUMS256 -print | sort | while IFS= read -r file; do
			$hash_cmd "$file"
		done > SHASUMS256
	)
}

copy_file "$ROOT_DIR/scripts/install.sh" "$OUT_DIR/install.sh"
copy_file "$ROOT_DIR/build/appicon.png" "$OUT_DIR/appicon.png"
chmod +x "$OUT_DIR/install.sh"
checksum_dir "$OUT_DIR"
