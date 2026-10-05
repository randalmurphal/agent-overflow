#!/usr/bin/env sh
# Shared helpers for the release build scripts. Sourced, not executed; the
# caller sets ROOT_DIR and VERSION first.

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
