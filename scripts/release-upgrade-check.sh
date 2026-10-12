#!/usr/bin/env sh
# release-upgrade-check: the candidate migrates the corpus the previous
# release published, boots on it, tops it up and archives it for the next
# release (docs/architecture/release-upgrade-check.md).
#
#   scripts/release-upgrade-check.sh [--previous TAG] [--bootstrap] [--scale N]
#       [--archive-out FILE] [--corpus DATA_ROOT] [--report FILE] [--keep]
#       [--root DIR]
#
# --previous names the release to upgrade from (default: the newest GitHub
# release of this repository). Its upgrade-corpus.tar.zst asset is the
# corpus; when the release has none, or with --bootstrap, its headless Linux
# binary seeds a synthetic corpus at --scale (1 is about 2 GB, 0.02 is a
# smoke slice) instead. Downloads land in bin/releases/<tag>/ and are
# verified against the release's SHASUMS256. --archive-out writes the
# migrated, topped-up corpus for the next release. --corpus runs an existing
# data root instead: an `ao-harness clone` of a real database, which stays
# on the machine that made it and is never archived. --report names the JSON
# report; --keep leaves the sandbox under --root (default: /tmp) for
# inspection. The candidate is bin/agent-overflow, or AO_HARNESS_BIN.
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BINARY_ASSET=agent-overflow-headless-linux-amd64
CORPUS_ASSET=upgrade-corpus.tar.zst

previous=
bootstrap=
scale=1
archive_out=
corpus=
report=
keep=
sandbox_root=
while [ $# -gt 0 ]; do
	case $1 in
	--previous) previous=$2; shift 2 ;;
	--bootstrap) bootstrap=1; shift ;;
	--scale) scale=$2; shift 2 ;;
	--archive-out) archive_out=$2; shift 2 ;;
	--corpus) corpus=$2; shift 2 ;;
	--report) report=$2; shift 2 ;;
	--root) sandbox_root=$2; shift 2 ;;
	--keep) keep=1; shift ;;
	*) echo "release-upgrade-check: unknown argument $1" >&2; exit 2 ;;
	esac
done

if [ "$(uname -s)" != Linux ]; then
	echo "release-upgrade-check: the previous release's headless binary is Linux-only; run this on Linux" >&2
	exit 2
fi

# download ASSET: fetch one asset of the previous release into its
# directory, verified against the release's SHASUMS256, once.
download() {
	asset=$1
	if [ -e "$dir/$asset" ]; then
		return 0
	fi
	echo "==> downloading $asset of $previous from $repo"
	mkdir -p "$dir"
	[ -e "$dir/SHASUMS256" ] || gh release download "$previous" -R "$repo" -D "$dir" -p SHASUMS256
	if ! grep -qE " (\./)?$asset\$" "$dir/SHASUMS256"; then
		return 1
	fi
	gh release download "$previous" -R "$repo" -D "$dir" -p "$asset"
	(cd "$dir" && grep -E " (\./)?$asset\$" SHASUMS256 | sha256sum -c -)
}

if [ -z "$corpus" ]; then
	repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
	if [ -z "$previous" ]; then
		previous=$(gh release view -R "$repo" --json tagName --jq .tagName)
	fi
	if [ -z "$previous" ]; then
		echo "release-upgrade-check: $repo has no release to upgrade from; pass --previous TAG" >&2
		exit 2
	fi
	dir="$ROOT_DIR/bin/releases/$previous"
	if [ -z "$bootstrap" ] && download "$CORPUS_ASSET"; then
		export AO_RELEASE_UPGRADE_ARCHIVE="$dir/$CORPUS_ASSET"
		echo "==> previous release $previous, its published corpus"
	else
		if ! download "$BINARY_ASSET"; then
			echo "release-upgrade-check: $previous publishes no $BINARY_ASSET" >&2
			exit 2
		fi
		chmod +x "$dir/$BINARY_ASSET"
		export AO_RELEASE_UPGRADE_PREVIOUS_BIN="$dir/$BINARY_ASSET"
		export AO_RELEASE_UPGRADE_SCALE="$scale"
		if [ -n "$bootstrap" ]; then
			echo "==> previous release $previous, bootstrapping a synthetic corpus at scale $scale"
		else
			echo "==> previous release $previous publishes no $CORPUS_ASSET; bootstrapping a synthetic corpus at scale $scale"
		fi
	fi
else
	if [ -n "$archive_out" ]; then
		echo "release-upgrade-check: a real data root is never archived; drop --archive-out" >&2
		exit 2
	fi
	export AO_RELEASE_UPGRADE_CORPUS="$corpus"
	echo "==> corpus $corpus"
fi
[ -n "$archive_out" ] && export AO_RELEASE_UPGRADE_ARCHIVE_OUT="$archive_out"
[ -n "$report" ] && export AO_RELEASE_UPGRADE_REPORT="$report"
[ -n "$keep" ] && export AO_RELEASE_UPGRADE_KEEP=1
[ -n "$sandbox_root" ] && export AO_RELEASE_UPGRADE_ROOT="$sandbox_root"

cd "$ROOT_DIR"
exec bin/ao-harness-e2e --config=playwright.manual.config.ts tests/upgrade-corpus.manual.spec.ts
