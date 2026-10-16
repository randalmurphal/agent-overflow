#!/usr/bin/env sh
# Checks scripts/ci-change.sh against a pull request whose base branch moved
# after the branch point: the change it reports must be the files the merge
# changes (git merge-tree), not everything the base branch gained since.
set -eu

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
CI_CHANGE=${CI_CHANGE:-$ROOT_DIR/scripts/ci-change.sh}
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT HUP INT TERM

export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
export GIT_CONFIG_NOSYSTEM=1 HOME="$TMP_DIR/home"
mkdir -p "$HOME"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# The remote: main gains base-only.txt and edits shared.txt after the
# branch point; the pull request edits feature.txt. GitHub's merge ref is
# main merged with the head.
remote=$TMP_DIR/remote
git init -q -b main "$remote"
git -C "$remote" config uploadpack.allowAnySHA1InWant true
printf 'one\n' > "$remote/shared.txt"
printf 'one\n' > "$remote/feature.txt"
git -C "$remote" add . && git -C "$remote" commit -qm base
git -C "$remote" checkout -qb feature
printf 'two\n' >> "$remote/feature.txt"
git -C "$remote" commit -qam feature
head_sha=$(git -C "$remote" rev-parse HEAD)
git -C "$remote" checkout -q main
printf 'two\n' >> "$remote/shared.txt"
printf 'new\n' > "$remote/base-only.txt"
git -C "$remote" add . && git -C "$remote" commit -qm 'main moves on'
main_sha=$(git -C "$remote" rev-parse HEAD)
merge_base=$(git -C "$remote" merge-base main feature)
git -C "$remote" checkout -q --detach main
git -C "$remote" merge -q --no-ff -m merge feature
git -C "$remote" update-ref refs/pull/1/merge HEAD
git -C "$remote" checkout -q main

# What the merge changes, by git merge-tree.
expected=$TMP_DIR/expected
merged_tree=$(git -C "$remote" merge-tree --write-tree "$main_sha" "$head_sha")
git -C "$remote" diff --name-only "$main_sha" "$merged_tree" > "$expected"
[ "$(cat "$expected")" = feature.txt ] || fail "merge-tree reports $(tr '\n' ' ' < "$expected")"

# gh answers the compare API with the merge base, as GitHub would.
bin=$TMP_DIR/bin
mkdir -p "$bin"
printf '#!/usr/bin/env sh\necho %s\n' "$merge_base" > "$bin/gh"
chmod +x "$bin/gh"

event=$TMP_DIR/event.json
printf '{"pull_request":{"base":{"ref":"main"},"head":{"sha":"%s"}}}\n' "$head_sha" > "$event"

# check NAME REF: a shallow checkout of REF, as actions/checkout makes it.
check() {
	name=$1
	ref=$2
	work=$TMP_DIR/$name
	git init -q "$work"
	git -C "$work" remote add origin "file://$remote"
	git -C "$work" fetch -q --no-tags --depth=1 origin "$ref"
	git -C "$work" checkout -q --detach FETCH_HEAD
	(
		cd "$work"
		PATH="$bin:$PATH" GITHUB_EVENT_NAME=pull_request GITHUB_EVENT_PATH="$event" \
			GITHUB_REPOSITORY=example/repo sh "$CI_CHANGE" "$TMP_DIR/$name-out" > /dev/null
	)
	cmp -s "$expected" "$TMP_DIR/$name-out/files" ||
		fail "$name: files are $(tr '\n' ' ' < "$TMP_DIR/$name-out/files"), want $(tr '\n' ' ' < "$expected")"
	grep -q '^+++ b/feature.txt' "$TMP_DIR/$name-out/change.diff" ||
		fail "$name: change.diff does not carry feature.txt"
	! grep -q 'shared.txt\|base-only.txt' "$TMP_DIR/$name-out/change.diff" ||
		fail "$name: change.diff carries the base branch's own changes"
}

check merge-ref refs/pull/1/merge
check head-commit "$head_sha"
echo "ci-change.sh tests passed"
