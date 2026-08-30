# Build Assets

This directory contains the supported desktop build assets for Agent Overflow.

- `darwin/` holds the macOS `.app` bundle metadata and icons.
- `linux/` holds the Linux desktop entry and optional package metadata.
- `windows/` holds the Windows WSL launcher icon, manifest, and version info.
- `docker/Dockerfile.cross` builds the optional cross-compilation image.

The first direct release supports native Linux, macOS `.app`, and Windows via
the WSL launcher. Android, iOS, native Windows installers, and server Docker
packaging are intentionally not present.

## macOS release signing

Development `make build` bundles are ad-hoc signed. A distributable macOS
artifact must go through `scripts/sign-notarize-macos.sh`, which Developer-ID
signs with the hardened runtime, submits to Apple's notary service, staples the
ticket, and verifies the result with both `stapler` and Gatekeeper. The release
builder does this automatically and refuses to package an unsigned bundle.

Set `AO_MACOS_SIGN_IDENTITY` plus either
`AO_MACOS_NOTARY_KEYCHAIN_PROFILE`, or the three direct notary credentials
`AO_MACOS_NOTARY_APPLE_ID`, `AO_MACOS_NOTARY_TEAM_ID`, and
`AO_MACOS_NOTARY_PASSWORD`.

The GitHub release workflow imports a temporary Developer ID `.p12` and uses
the direct notary credentials. Configure these repository secrets:
`MACOS_CERTIFICATE_BASE64`, `MACOS_CERTIFICATE_PASSWORD`,
`MACOS_SIGN_IDENTITY`, `MACOS_NOTARY_APPLE_ID`, `MACOS_NOTARY_TEAM_ID`, and
`MACOS_NOTARY_PASSWORD`. A tag build fails instead of publishing an ad-hoc
signed Mac artifact when any credential is absent.

On macOS, `make release-macos` builds only that signed/notarized arm64 asset;
the cross-platform `make release` still expects the Linux/WSL legs as well.
