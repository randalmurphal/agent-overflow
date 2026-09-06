# Test a candidate, then publish its bytes

`Release build` has two separate paths. A manual run builds all desktop/server
artifacts and the signed Android APK, packages them with installation assets,
and uploads `agent-overflow-release-<version>`. It creates no tag or release.
Download that artifact from the successful Actions run to test production builds.
The APK uses the persistent signing key, so it upgrades earlier signed installs.

Before dispatch, commit synchronized version metadata and run the local release
gates. The workflow's optional version input must match those committed files.
The candidate includes `CANDIDATE.json` identifying the repository, exact commit,
version, workflow run and attempt; `SHASUMS256` covers it and every install asset.
Record which run you tested. Candidates are retained for 90 days, subject to the
repository's Actions retention limits.

When ready to publish, create and push `v<version>` at **that exact tested
commit**. The tag workflow runs only promotion; platform build jobs are skipped.
It finds successful manual runs of this workflow for that commit and selects
the single remaining, unexpired artifact for that version. It verifies GitHub's
immutable artifact SHA-256 before extraction, the exact expected file set, the
candidate provenance, and every file checksum. It also refuses a run whose
attempt/status changed during the download. Then it publishes those same files,
including the provenance and checksums, without rebuilding or repackaging.
The published release remains write-once; retries refuse an existing release.

Promotion fails rather than guessing when:

- No eligible artifact exists, or retention expired: build and test a new manual
  candidate for that commit/version, then rerun the failed promotion workflow.
- Multiple eligible candidates remain: the error lists run IDs. Keep the tested
  candidate artifact and explicitly delete the unwanted packaged candidate
  artifacts in Actions, then retry. Promotion never deletes candidates itself.
- Metadata, hashes, archive contents or run provenance disagree: investigate the
  failed check; do not regenerate checksums to make the saved candidate pass.

A newer source commit needs its own candidate and testing. An APK icon/native
change also increments `mobile/shell-build.txt` before building that candidate;
frontend-only bundle compatibility does not change just for a cosmetic APK edit.

The promotion script requires Node, `gh`, and `unzip` on the Linux workflow runner.
`node --test scripts/release-candidate.test.mjs` exercises selection, exact-byte
promotion and failure paths with a fake GitHub API and local archives; it neither
contacts GitHub nor publishes. The package job runs it before uploading a
candidate. Go workflow contract tests keep tags out of the build/package jobs.
