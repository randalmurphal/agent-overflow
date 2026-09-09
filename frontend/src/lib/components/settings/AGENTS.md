# components/settings/

Settings presents frontend preferences, computer-owned configuration, provider
accounts, themes, notifications, remote access, and device pairing. The owning
store performs reads and writes; pages bind controls to that owner.

## Controls and navigation

- Decide ownership before adding a control. Frontend preferences stay local to
  this client; provider and backend settings belong to the selected computer.
- Reconcile saved values from the write response. Keep errors visible beside the
  control and do not silently restore an old value after failure.
- Controls with enable and disable modes must clear state created by the prior
  mode. Test on-to-off, off-to-on, repeated save, and teardown.
- Register sections and deep links through the shared settings navigation. A
  deep link must select the target computer and section before focusing a
  control.
- Compact settings is a stack: section rail, then page. Preserve the rail and
  page while navigating and keep every control within the viewport.

Notification permission, native capability, and app preference are separate
states. Show the platform result and retain a denied or unavailable outcome;
do not present an enabled toggle when delivery cannot occur.

Provider pages remain provider-specific. Account, model, MCP, and login controls
route to their owning computer and respect caller scope. Do not infer capability
from layout or from another provider's fields.

## Pairing and remote computers

Pairing displays secrets only in the intended setup surface. Do not place
invitations, tokens, grants, or credentials in browser storage, diagnostics, or
copied error details. Pairing and removal errors remain visible and name the
affected computer.

Before changing computer nickname fields or visibility, read
[`decisions.md`](../../../../../docs/decisions.md#decisions-still-required).

SSH setup is a guided capability check, not an alternate transport. Validate
host input, surface command failures, and keep generated commands free of
secrets that do not need to appear.
