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

## Testing

- Validation changes need rejection tests for malformed, hostless, relative,
  and unsupported-scheme inputs.
- Platform command selection should stay deterministic through injected lookup
  and start functions. Do not let unit tests depend on the developer machine's
  installed browser opener.
