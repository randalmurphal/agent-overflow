---
name: release
description: Cut an Agent Overflow release from a chosen commit, gate it, and publish it by tag.
disable-model-invocation: true
---

# Release

A release is the bytes of one tested candidate, published by a tag at that candidate's exact commit. [Release candidates](../../../docs/architecture/release-candidates.md) is the contract for building, selecting and promoting candidates; this skill is the order of operations. Every release goes through a `release-<version>` branch, so the same flow covers main's head and an older commit that leaves out later merges.

## 1. Choose the commit

`git fetch origin`, then list what the release carries: `git log --oneline <previous tag>..<commit>`. The commit is origin/main's head unless the user names another.

Brief the user on every PR in that range, one line each, flagging the risky ones: a schema migration (one-way: a database it upgrades refuses every older build), behavior that is on by default, a destructive action, a change to forge or provider traffic.

Done when the user has named the commit and the version.

## 2. Prepare the branch

```sh
git switch -c release-<version> <commit>
scripts/sync-release-version.sh <version>
```

Increment `mobile/shell-build.txt` by one: it is the APK version code, and Android installs a new APK over an old one only when the code is higher. Commit as `Prepare the <version> release candidate`.

Done when `git show --stat HEAD` lists exactly the synced version files plus `mobile/shell-build.txt`.

## 3. Gate locally

Run `make verify` on the release branch. It runs the build, Go, Vitest and browser tests, and fails when a regenerated file (bindings, icons) differs from the commit, which the release workflow's clean-tree guard would otherwise reject in CI. A failure is fixed on main first, then cherry-picked onto the release branch, and the gate reruns; rebasing onto main would pull in the merges the chosen commit leaves out.

Done when `make verify` exits 0.

## 4. Build the candidate and run CI on it

```sh
git push -u origin release-<version>
gh workflow run ci.yml --ref release-<version>
gh workflow run release-build.yml --ref release-<version>
```

CI runs on push only for main, so the branch's commit gets CI only from this dispatch. The release build compiles every platform, runs the [release upgrade check](../../../docs/architecture/release-upgrade-check.md) against the previous release's published corpus, and uploads `agent-overflow-release-<version>`.

Watch both runs to completion. Expected skips: `Dependency review` (pull requests only) and `Publish tested candidate` (tags only). Any other non-success job stops the release; fix it as in step 3, and the new branch head needs a new candidate.

Done when both runs conclude success, with each job's conclusion reported to the user.

## 5. Test the candidate

The user downloads `agent-overflow-release-<version>` from the release build run and tests it. Promotion needs exactly one unexpired candidate artifact for that commit and version; if a rerun produced a second, delete the untested one in Actions.

Done when the user says the candidate is good.

## 6. Publish

Pushing the tag publishes a write-once release. Get the user's explicit go for this step, then:

```sh
git tag -a v<version> <candidate commit> -m "Agent Overflow v<version>"
git push origin v<version>
```

The tag workflow verifies the tag against `frontend/package.json`, downloads the tested candidate and publishes it with generated notes. Watch that run.

Done when the run succeeds and `gh release view v<version>` lists the assets.

## 7. Bring the version back to main

Open a PR from `release-<version>` into main and merge it once its checks pass, so main and the next release start from this version. Main's ruleset requires a code-owner review, so a plain merge reports BLOCKED; the owner decides how it merges.

Done when the PR is merged and the release branch is deleted.
