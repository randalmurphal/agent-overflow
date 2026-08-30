# svelte-streamdown (vendored)

- **Upstream:** https://github.com/beynar/svelte-streamdown
- **Vendored baseline:** `svelte-streamdown@3.1.2` (npm tarball contents)
- **Baseline tarball integrity:**
  `sha512-yGbe24Bodv/xiPc36+YRAmhb/js+aIPpNud4GMb4qT93yviln40/qMWBw8VfaHOmQ/oQtuTtpr+pyqzEzQa2Zg==`
  (recovered from the lockfile entry deleted at the vendoring commit — it is
  the only surviving proof of what this tree started as)
- **Vendored on:** 2026-08-07
- **Contents:** the published package's `dist/` tree, `LICENSE`, `README.md`.
  The npm tarball ships **only** `dist/` — there is no parallel `src/` to
  strip, and the package's `exports` map resolves every surviving entry
  point (`.`, `./mermaid`, `./math`) into `dist/`. `./code` was dropped
  with the shiki code component (`DIVERGENCE.md` entry 28).

## Why it lives here

Upstream is dormant. Carrying our fixes as a 13-hunk pnpm patch meant
re-rolling all 13 by hand on every version bump — pure maintenance
overhead for divergence that is permanent either way. This tree is
`svelte-streamdown@3.1.2` **with that patch already applied**; the patch
file and its `patchedDependencies` entry are gone.

The per-fix rationale, drop rules, and regression-test names live in
[`DIVERGENCE.md`](DIVERGENCE.md), beside the code. Keep that ledger current:
it is the only record of what this tree does differently from upstream 3.1.2.

Which also makes it the only remaining integrity control. Vendoring is
trust-on-first-use: once the tarball was unpacked into the repo, no
`pnpm install` re-verifies it against the registry, and the checksum above is
a record rather than an enforced gate. What stands in for it is that every
byte differing from upstream 3.1.2 has a ledger entry — so `diff -ru` against
a freshly fetched baseline (below) is a full audit, and an unexplained hunk in
that diff is the alarm. Since entry 28 this tree is no longer a superset of
upstream: that diff is now mostly DELETIONS, which entry 28 covers as a
block.

## How it is wired in

`frontend/pnpm-workspace.yaml` declares `packages: [vendor/*]` and
`frontend/package.json` depends on `"svelte-streamdown": "workspace:*"`,
so pnpm symlinks `node_modules/svelte-streamdown` → here. Every import
specifier (`svelte-streamdown`, `svelte-streamdown/mermaid`, …) is
unchanged.

`workspace:` rather than `file:` is deliberate. pnpm resolves a `file:`
directory through its content-addressed store and **hardlinks** the tree
in: the files under `vendor/` and the ones the build reads share inodes,
so an in-place edit silently mutates the pnpm store, and any edit that
replaces the file (the normal editor write) does not reach the build
until the next install. A workspace package is a plain symlink — what is
checked in is what compiles. pnpm still installs this package's own
`dependencies` (into `vendor/svelte-streamdown/node_modules/`, gitignored)
and resolves the `svelte` peer to the app's single patched copy, so the
Svelte runtime is not duplicated.

`package.json` here keeps upstream's identity and `peerDependencies`
verbatim. `exports` lost `./code`, and `dependencies` lost `shiki`,
`@shikijs/langs`, `@shikijs/themes` and `@floating-ui/dom`, with the code
that used them (`DIVERGENCE.md` entry 28). Also dropped: `scripts`, `devDependencies`,
`publishConfig`, `packageManager`, `pnpm`, `files` — they describe
upstream's SvelteKit build/publish pipeline, which does not exist in a
dist-only tree (`scripts.prepare` runs `svelte-kit sync`, an install-time
landmine) — and `keywords`, which is registry-discovery metadata for a
package that is never published from here.

Three build-config couplings exist because the code moved out of
`node_modules/`, and all three are load-bearing:

- `vite.config.ts` — the `markdown-vendor` chunk group matches
  `(?:node_modules|vendor)/`; matching only `node_modules/` puts the whole
  library back in the entry chunk.
- `src/app.css` — `@source not "../vendor";`. Tailwind skips
  `node_modules` automatically but not `vendor/`, so without this it starts
  generating utilities for the library's default theme classes, which we
  override wholesale anyway.
- `svelte.config.js` — `onwarn` drops compiler warnings from `vendor/`.
  Vendored code compiles as project source now, so upstream's authoring
  choices would otherwise warn on every build and test run.

## Syncing against a future upstream release

There is no patch file to re-roll; diff the trees instead.

```sh
# from frontend/

# 1. Re-fetch the BASELINE and verify it is the tree this one came from.
#    Vendoring is trust-on-first-use; this is the one moment the recorded
#    integrity can still be checked, so do it before diffing anything.
npm pack svelte-streamdown@3.1.2
printf '%s  %s\n' \
  "$(node -e 'process.stdout.write(require("crypto").createHash("sha512").update(require("fs").readFileSync(process.argv[1])).digest("base64"))' svelte-streamdown-3.1.2.tgz)" \
  svelte-streamdown-3.1.2.tgz
# Must equal the base64 body of the integrity recorded above (the part after
# `sha512-`). If it does not, STOP: the baseline this tree claims to diverge
# from is not what npm is serving, and the ledger cannot be trusted.
mkdir -p /tmp/base && tar -xf svelte-streamdown-3.1.2.tgz -C /tmp/base

# 2. What upstream ships now.
npm pack svelte-streamdown@<new-version>
tar -xf svelte-streamdown-<new-version>.tgz -C /tmp    # -> /tmp/package

# 3. What we changed, on the baseline (this is the divergence ledger, as a
#    diff — every hunk must map to a DIVERGENCE.md entry).
diff -ru /tmp/base/package/dist vendor/svelte-streamdown/dist

# 4. What upstream changed between the two releases.
diff -ru /tmp/base/package/dist /tmp/package/dist
```

Take upstream's tree as the new baseline, replay diff 3 onto it hunk by hunk
against the ledger, and re-run the markdown regression battery named there
(`AssistantMessage.test.ts`, `ChatMarkdown*.test.ts`,
`markdown/alertBlockquote.test.ts`, `markdown/incrementalLex.test.ts`,
`markdown/listMarkerCode.test.ts`,
`chat/streamingIncrementalReuse.browser.test.ts`). Bump `version` in this
package's `package.json`, and update BOTH the baseline version and the
baseline integrity recorded above — a stale checksum is worse than none,
because step 1 would then reject a correct tree.

Fixes made here should still be offered upstream when they are general
bugs rather than deliberate deviations — [`DIVERGENCE.md`](DIVERGENCE.md)
marks which are which.
