# internal/externalurl

Opens user-visible HTTP(S) links in the host operating system's browser.

## Ownership

- Keep URL validation here as the backend trust boundary. Frontend checks are
  UX only and must not be treated as authorization.
- Only `http` and `https` URLs are allowed. Do not expand this to arbitrary
  schemes without a threat-model pass.
- Do not invoke a shell. Build commands as argv slices so URLs cannot become
  shell syntax.
- WSL opens through Windows interop because the visible desktop is Windows.
  Native Linux uses desktop opener commands in fallback order.
- The opener inherits our environment minus the AppImage launch artifacts
  (`appimage.ScrubInherited` in `startCommand`; nil on every other launch
  shape, so `exec.Cmd` inherits directly). The browser it hands the URL to
  outlives Agent Overflow, and a mount-local `LD_LIBRARY_PATH` /
  `XDG_DATA_DIRS` points at a squashfs that is gone once we exit.

## Frontend entry points

Every UI path that opens a URL goes through `utils/externalLinks.ts`
(`handleExternalURL`), which picks the `OpenExternalURL` binding or
`window.open` by run mode. There is no second wrapper. Current callers:

- the document-level anchor click delegate (`installExternalLinkDelegate`),
- the terminal's xterm link provider — `WebLinksAddon` is constructed WITH a
  handler in `components/terminal/terminalXterm.ts`; its default handler
  calls `window.open` directly and would bypass the WSL → Windows bridge,
- `components/chat/DevServerChip.svelte`, the "open in browser" chip on a
  command row that announced a loopback dev server.

`loopbackDevServerURL` in the same module narrows a URL to the dev-server
subset (localhost / 127.0.0.0-8 / `[::1]`). Wildcard bind addresses are
rejected there on purpose: triage rewrites `0.0.0.0` and `::` to
`localhost` before the URL ever reaches the UI, because a browser cannot
navigate to the raw form.

## Testing

- Validation changes need rejection tests for malformed, hostless, relative,
  and unsupported-scheme inputs.
- Platform command selection should stay deterministic through injected lookup
  and start functions. Do not let unit tests depend on the developer machine's
  installed browser opener.
