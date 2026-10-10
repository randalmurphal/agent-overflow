#!/usr/bin/env sh
# Writes the change under test as a unified diff and a file list, from the
# GitHub API rather than git history, so a shallow checkout is enough.
#
#   scripts/ci-change.sh OUT_DIR
#
# Reads GITHUB_EVENT_NAME, GITHUB_REPOSITORY and the event payload
# (GITHUB_EVENT_PATH); needs GH_TOKEN. Produces OUT_DIR/change.diff and
# OUT_DIR/files (one path per line, repository-relative, added, copied,
# modified or renamed files only). A pull request diffs against its merge
# base; a push diffs the pushed range. Any other event is an error: the
# lint gates only run where a base exists.
set -eu

out=${1:?usage: ci-change.sh OUT_DIR}
mkdir -p "$out"

case "${GITHUB_EVENT_NAME:-}" in
pull_request|pull_request_target)
	number=$(jq -r '.pull_request.number' "$GITHUB_EVENT_PATH")
	gh api -H 'Accept: application/vnd.github.diff' "repos/$GITHUB_REPOSITORY/pulls/$number" > "$out/change.diff"
	gh api --paginate "repos/$GITHUB_REPOSITORY/pulls/$number/files?per_page=100" \
		--jq '.[] | select(.status != "removed") | .filename' > "$out/files"
	;;
push)
	before=$(jq -r '.before' "$GITHUB_EVENT_PATH")
	after=$(jq -r '.after' "$GITHUB_EVENT_PATH")
	case "$before" in
	0000000000000000000000000000000000000000)
		echo "ci-change.sh: push of a new branch has no base" >&2
		exit 2
		;;
	esac
	gh api -H 'Accept: application/vnd.github.diff' "repos/$GITHUB_REPOSITORY/compare/$before...$after" > "$out/change.diff"
	gh api --paginate "repos/$GITHUB_REPOSITORY/compare/$before...$after?per_page=100" \
		--jq '.files[] | select(.status != "removed") | .filename' > "$out/files"
	;;
*)
	echo "ci-change.sh: no base for event ${GITHUB_EVENT_NAME:-unset}" >&2
	exit 2
	;;
esac

printf 'ci-change.sh: %s changed files, %s diff bytes\n' "$(wc -l < "$out/files")" "$(wc -c < "$out/change.diff")"
