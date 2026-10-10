#!/usr/bin/env sh
# Writes the change under test as a unified diff and a file list, so a
# shallow checkout is enough: the base commit comes from the GitHub API and
# is fetched by itself.
#
#   scripts/ci-change.sh OUT_DIR
#
# Reads GITHUB_EVENT_NAME, GITHUB_REPOSITORY and the event payload
# (GITHUB_EVENT_PATH); needs GH_TOKEN and an origin remote for the
# repository. Produces OUT_DIR/base (the commit), OUT_DIR/change.diff and
# OUT_DIR/files (one path per line, repository-relative, added, copied,
# modified or renamed files only). A pull request diffs against its merge
# base with the base branch; a push diffs the pushed range. Any other event
# is an error: the lint gates only run where a base exists.
set -eu

out=${1:?usage: ci-change.sh OUT_DIR}
mkdir -p "$out"

case "${GITHUB_EVENT_NAME:-}" in
pull_request|pull_request_target)
	base_sha=$(jq -r '.pull_request.base.sha' "$GITHUB_EVENT_PATH")
	head_sha=$(jq -r '.pull_request.head.sha' "$GITHUB_EVENT_PATH")
	base=$(gh api "repos/$GITHUB_REPOSITORY/compare/$base_sha...$head_sha" --jq '.merge_base_commit.sha')
	;;
push)
	base=$(jq -r '.before' "$GITHUB_EVENT_PATH")
	case "$base" in
	0000000000000000000000000000000000000000)
		echo "ci-change.sh: push of a new branch has no base" >&2
		exit 2
		;;
	esac
	;;
*)
	echo "ci-change.sh: no base for event ${GITHUB_EVENT_NAME:-unset}" >&2
	exit 2
	;;
esac

case "$base" in
*[!0-9a-f]*|"")
	echo "ci-change.sh: no base commit resolved" >&2
	exit 2
	;;
esac

printf '%s\n' "$base" > "$out/base"
git fetch --no-tags --depth=1 --quiet origin "$base"
git diff --no-color --no-ext-diff "$base" HEAD > "$out/change.diff"
git diff --name-only --no-renames --diff-filter=ACM "$base" HEAD > "$out/files"

printf 'ci-change.sh: base %s, %s changed files, %s diff bytes\n' "$base" "$(wc -l < "$out/files")" "$(wc -c < "$out/change.diff")"
