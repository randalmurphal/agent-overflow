# The bundle the phone downloads

One job: turn the SPA this backend already embeds into a SHA-256
manifest and a zip of exactly the files that manifest names. Nothing
here knows what HTTP is. `internal/transport` owns the two routes
(`/bundle/manifest.json`, `/bundle/archive.zip`), the credential in
front of them, and the hello fields that advertise the id;
`frontend/src/lib/native/bundleSync.ts` is the consumer, and
`mobile/android/.../BundlePlugin.java` is what puts the bytes on disk.

Design authority: `docs/specs/remote-access.md` §9, "Bundle sync: the
backend is the phone's update server".

## Why a backend serves its own bundle at all

Every other client of this backend is SERVED the bundle it then runs, so
its web code and its wire cannot skew. The APK is the one client that
carries a bundle of its own, frozen at whenever its store build was cut.
The paired backend is the update server: there is no update SaaS, separate
web-bundle release signature, or second distribution channel. APK signing is
independent of this web update path. The paired session uses authenticated
HTTPS, and each file is checked against the manifest served on that same
session, so the device accepts exactly the bundle supplied by the backend it
already trusted when pairing.

## The id is content, not a version

`ID` is the hex SHA-256 over the sorted `path\x00sha256\n` lines of the
manifest. Identical content shares an id across hosts; any changed release
bytes produce a different id. Identity does not establish release order.

The build stamps `bundle-release.json` with the frontend package's complete
SemVer **before** hashing. That file is included in the manifest and archive;
`Version` comes from those same hashed bytes, not the backend executable's
link-time stamp. The metadata is bounded to 4 KiB and must contain exactly a
valid `version` field. A malformed file refuses the manifest; only an absent
file permits the legacy link-time fallback.

The phone accepts automatic updates only when the candidate is strictly newer
than its executing and APK-packaged versions. Equal versions (including builds
with different content or SemVer build metadata), older versions, and unordered
values such as `dev` cannot replace installed code. Native staging verifies the
release metadata against the manifest and checks the version again; a misleading
hello or old backend cannot bypass that boundary. Content ids still identify
exact downloads and failed boots. To test web updates without a release, build a
strictly newer frontend package version; rebuilding the same version requires
installing the APK instead.

## Two implementations of one rule, pinned by one golden

The Go side hashes the embedded `frontend/dist`. `frontend/scripts/
bundleId.ts` stamps `bundle-release.json`, hashes the same tree at build time, then stamps
`bundle-id.txt` into it, because a shell running the bundle its APK
shipped with has no state-file entry naming it and must still be able to
say "the backend's id is the one I am already running".

If those two ever disagree, a phone downloads a bundle it is already
running, forever, on every hello. So both hash `testdata/fixturebundle/`
in their own suite and both compare against `testdata/fixturebundle.id`.
**Changing the rule means changing both and re-stamping that file**; the
Go test's failure message says so.

`Included` states the two exclusions, and they are the whole rule:
`*.map` (emitted only by `AO_SOURCEMAP=1`, requested by no page, and
megabytes on a phone's link) and `bundle-id.txt` itself (the plugin
writes it AFTER hashing, so a walk that counted it would hash a tree the
plugin never saw).

## What is built when, and what is retained

- `Manifest()` walks the tree once under a `sync.Once`. It hashes every
  served byte and retains only the file list. Its first caller is
  normally the first WebSocket connection, because the hello frame
  publishes the id.
- `Archive()` builds the zip on its own first caller and retains it for
  the process's life — one compressed copy of the whole bundle, a few
  MB. **Only a phone ever asks**, so a desktop backend with no shell
  paired must not pay for it. That split is deliberate and is the one
  place this package deviates from a literal "one walk builds both".
- They cannot disagree even so: the archive is built FROM the manifest's
  file list, in its order, and re-verifies each digest as it compresses.
  A file that no longer hashes to what was published is an error here
  rather than a download every phone would reject after paying for it.
- Zip entries carry no modification time, so two builds of one tree
  produce byte-identical archives — the same property the id has.

## Native compatibility is separate from release ordering

`MinShellBuild` is the lowest Android `versionCode` this bundle's
`native/` seams can run on. Web code that calls a Capacitor plugin the
installed APK was never built with does not degrade; it answers null
forever or throws where nothing catches it. So the bundle states a floor
and the shell compares its own build against it before downloading
anything. The current floor is 10, which adds the Android camera-intent visibility
declaration needed by Take photo.

**Bump it in the same change that needs new native code or manifest support, and
never for a web-only change.** A bump costs every phone below it its
updates until the person installs a new APK.

## An empty tree is refused

A dist that was never built, or one holding nothing but source maps,
answers an error rather than an empty manifest. An empty manifest is a
bundle a phone would happily stage over a working app.
