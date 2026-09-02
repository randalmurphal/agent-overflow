# Waking a phone that is not connected

One job: hand Google a message a paired phone's own service can render
into a tray notification. Nothing here knows what a thread is, what a
setting is, or when a notification is worth sending — `internal/notify`
decides what happened, `internal/app/app_notification_mapping.go`
decides who hears about it, and this package is the last hop.

Design authority: `docs/specs/remote-access.md` §9, "Push" and
"Notification semantics"; §17 item 4; §18 item 1.

Consumers: `internal/app/app_push.go` (the fan-out and the RPCs),
`internal/store/push.go` (the tokens and the credential),
`mobile/android/.../push/` (the renderer on the other end).

## Owner-only, this wave

The owner's backend sends with the app's own Firebase credential. A
friend's backend has no `push_sender` row, so its `Sender` is nil: it
records registrations and sends nothing. That branch is one nil check
and it is deliberately the WHOLE of the difference, because the design's
next step is a home backend acting as a wake relay for the backends
attached to it. When that lands, a friend's backend gains a `Sender`
that forwards instead of a `Sender` that dials Google, and no caller
here changes.

## Data-only, always

A message carries `data` and never `notification`. Google composes
nothing, which buys the two properties the feature is built on:

- **The phone's service runs on every message**, foreground or
  background, so it can DROP one it should not show. A
  Google-composed notification is posted by the system before any of our
  code sees it, and the in-app case would double up.
- **A retraction is a message like any other.** `retract` is not a
  Google concept; withdrawing a tray notification means our own service
  calling `cancel(tag)`, and it can only do that if it is handed the
  message.

`@capacitor/push-notifications` renders only Google-composed
notifications and cannot cancel one, which is why the shell has its own
plugin instead.

## What a message is allowed to say

The payload transits Google, so §9's redaction rule applies and the
type shape enforces it the same way `notify`'s does: there is nowhere in
`Message` to put a thread title. `MessageFor` is the only builder, and
it composes exactly:

| key | value |
|---|---|
| `id` | the `notify.Send` id, so the tray can replace and withdraw |
| `kind` | the notification kind, for the renderer's channel and icon |
| `retract` | `"1"` on a withdrawal, absent otherwise |
| `title` | `notify.KindPhrase(kind)`: one of six FIXED phrases, the same words the desktop shows |
| `body` | the backend's display name and nothing else |
| `target` | the tap route, as its own JSON document |

A retraction carries `id`, `kind` and `retract` and stops there — it
names no phrase, no backend and no target, because a withdrawal is not a
presentation and none of that is needed to cancel by tag.

`Tag` is the send id. That single choice is what makes replace-in-place
and retract-by-id work at the tray, and it is why the id must stay
STABLE across the states of one fact, which is `notify`'s guarantee
(`thread:<id>`, `approval:<thread>:<request>`).

### Why the target is one key and not several

`notify.TargetToMap` spells the route's fields for the Windows launcher,
and one of them is `kind` — the ROUTE's kind, not the notification's.
Flattening those fields alongside `kind` above would silently overwrite
one with the other. So the target rides as a single `target` key holding
the JSON of the same struct, with the same field spellings the SPA's
`parseNotificationTarget` already reads. The Java renderer forwards the
string through to the launch intent without needing to know a single
target field name.

## One error is actionable, and only one

`ErrTokenGone` means the registration is dead: Google answered 404, or
`UNREGISTERED` in the error details. The fan-out deletes the row, and
the phone re-registers on its next launch.

Every other failure is a plain error — recorded, logged once, and
otherwise ignored. In particular `INVALID_ARGUMENT` is NOT
`ErrTokenGone`, however much it looks like a bad token: a bug in the
payload WE build produces it for every device at once, and mapping it to
"gone" would unregister every phone the owner has over one of our own
mistakes.

## The tests reach nothing

`ParseCredential` validates SHAPE only: `type == "service_account"`, a
project, an account, and a `private_key` that parses as a PEM-wrapped
RSA key. It never dials, because it runs inside `SetPushSenderCredential`
and the first real send is what reports the rest through
`GetPushSenderStatus().lastError`.

The sender's tests are the same discipline from the other side: the
fixture credential's `token_uri` points at the `httptest` server, and
`newFCMSenderAt` injects the send endpoint, so the OAuth exchange and
the send are both local. **Nothing in `make go-test` may reach the
network**, and there is no code path here that would try.
