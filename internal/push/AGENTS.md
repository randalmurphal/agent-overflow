# internal/push

Owner-configured Firebase Cloud Messaging sender used to wake a registered
phone. It sends bounded data-only messages; notification policy and registration
ownership remain in the application.

- A sender credential is owner-only state. Parse and validate its service-account
  shape without logging or returning private fields.
- Send data-only payloads so the application's authenticated event path remains
  authoritative for content and action handling.
- Push data may contain only the bounded notification identity, presentation
  fields, route target, and backend identity required by the mobile client.
  Never include provider output, credentials, proofs, or arbitrary peer error
  text.
- Use `<backend>|<notification-id>` as the cross-source tray identity so the
  socket and push paths replace the same notification without colliding across
  computers. Keep the platform collapse key stable for one logical fact.
- Encode the notification target as one JSON `target` value through
  `notify.TargetToMap`; flattening it would collide with message fields.
- Return `ErrTokenGone` only for an FCM response that specifically identifies
  an unregistered token. Other failures remain visible errors and must not
  delete registrations.

Tests inject OAuth and FCM endpoints or use the harness recorder. No test may
contact Google or use a real credential.
