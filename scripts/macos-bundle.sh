# Shared by the build and the standalone installer. The caller prepares a
# complete, signed bundle on the destination filesystem before publishing it.
publish_macos_bundle() (
	set -eu
	source_app=$1
	destination=$2
	parent=$(CDPATH= cd -- "$(dirname -- "$destination")" && pwd -P)
	name=$(basename -- "$destination")
	destination=$parent/$name
	[ -d "$source_app" ] && [ ! -L "$destination" ] || {
		echo "ERROR: expected a prepared app bundle and a non-symlink destination" >&2
		exit 1
	}
	[ ! -e "$destination" ] || [ -d "$destination" ] || {
		echo "ERROR: app destination is not a directory: $destination" >&2
		exit 1
	}

	previous=""
	rollback() {
		if [ -n "$previous" ] && [ -d "$previous/$name" ] && [ ! -e "$destination" ]; then
			mv "$previous/$name" "$destination"
		fi
	}
	# Signal handling follows main_shell_signals_unix_test.go. A previous
	# directory that a signal leaves empty is reclaimed with the retired
	# ones by the next publish.
	trap rollback EXIT
	case ${BASH_VERSION:-} in 5.2.*) ;; *) trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM ;; esac
	if [ -d "$destination" ]; then
		previous=$(mktemp -d "$parent/.$name.previous.XXXXXX")
		mv "$destination" "$previous/$name"
	fi
	mv "$source_app" "$destination"

	# Keeping the old executable alone is insufficient: macOS validates the
	# running process against its complete original bundle, even after rename.
	for retired in "$parent/.$name.previous."*; do
		[ -d "$retired" ] && [ ! -L "$retired" ] || continue
		if [ "$(uname -s)" = Darwin ]; then
			# Be conservative on inspection errors; a later install can retry.
			in_use=$(/usr/sbin/lsof -nP -t +D "$retired" 2>&1 || true)
			[ -z "$in_use" ] || continue
		fi
		rm -rf "$retired"
	done
)
