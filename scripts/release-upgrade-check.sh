#!/usr/bin/env sh
# release-upgrade-check: the candidate in bin/ migrates a large database the
# previous release wrote and boots on it (docs/architecture/release-upgrade-check.md).
#
#   scripts/release-upgrade-check.sh [--previous TAG] [--scale N]
#       [--corpus DATA_ROOT] [--report FILE] [--keep] [--root DIR]
#
# --previous names the release whose headless Linux binary seeds the corpus
# (default: the newest GitHub release of this repository). The binary is
# downloaded once to bin/releases/<tag>/ and verified against that release's
# SHASUMS256. --scale sizes the synthetic corpus (1 is about 2 GB, 0.02 is a
# smoke slice). --corpus runs an existing data root instead of seeding one:
# an `ao-harness clone` of a real database, which stays on the machine that
# made it. --report names the JSON report; --keep leaves the sandbox under
# --root (default: the system temp dir) for inspection.
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
ASSET=agent-overflow-headless-linux-amd64

previous=
scale=1
corpus=
report=
keep=
sandbox_root=
while [ $# -gt 0 ]; do
	case $1 in
	--previous) previous=$2; shift 2 ;;
	--scale) scale=$2; shift 2 ;;
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

if [ -z "$corpus" ]; then
	repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
	if [ -z "$previous" ]; then
		previous=$(gh release view -R "$repo" --json tagName --jq .tagName)
	fi
	if [ -z "$previous" ]; then
		echo "release-upgrade-check: $repo has no release to seed from; pass --previous TAG" >&2
		exit 2
	fi
	dir="$ROOT_DIR/bin/releases/$previous"
	if [ ! -x "$dir/$ASSET" ]; then
		echo "==> downloading $ASSET of $previous from $repo"
		rm -rf "$dir"
		mkdir -p "$dir"
		gh release download "$previous" -R "$repo" -D "$dir" -p "$ASSET" -p SHASUMS256
		(cd "$dir" && grep -E " (\./)?$ASSET\$" SHASUMS256 | sha256sum -c -)
		chmod +x "$dir/$ASSET"
	fi
	export AO_RELEASE_UPGRADE_PREVIOUS_BIN="$dir/$ASSET"
	export AO_RELEASE_UPGRADE_SCALE="$scale"
	echo "==> previous release $previous, corpus scale $scale"
else
	export AO_RELEASE_UPGRADE_CORPUS="$corpus"
	echo "==> corpus $corpus"
fi
[ -n "$report" ] && export AO_RELEASE_UPGRADE_REPORT="$report"
[ -n "$keep" ] && export AO_RELEASE_UPGRADE_KEEP=1
[ -n "$sandbox_root" ] && export AO_RELEASE_UPGRADE_ROOT="$sandbox_root"

cd "$ROOT_DIR"
exec bin/ao-harness-e2e --config=playwright.manual.config.ts tests/release-upgrade.manual.spec.ts
