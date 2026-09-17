/**
 * The tag one presented notification is known by: `<backendId>|<id>`.
 *
 * ONE SPELLING, THREE PRESENTERS. `push.TrayTag` and `TrayNotifier.tagFor`
 * compose the same string on the phone, the phone's wire presenter uses it
 * (`stores/pushPresenter.svelte.ts`) and so does the remote browser's
 * (`stores/browserNotificationPresenter.svelte.ts`). It is what makes a later
 * state change REPLACE a notification and a retraction cancel exactly it, and
 * a second spelling would mean one surface failing to withdraw what another
 * raised.
 *
 * NAMESPACED BY BACKEND, home included. Not every notification id is unique
 * across machines — `provider-auth:claude` is the same string on every backend
 * the owner runs — and without the prefix one machine's sign-out notice would
 * silently replace another's. The pushed path composes the SAME tag from the
 * message's own `backend` key, so a backgrounded phone told about one moment
 * on the wire and again through Google shows one notification, the second
 * replacing the first.
 *
 * An unknown origin (empty) keeps the bare id, the same fallback the renderer
 * makes for a message with no backend key.
 *
 * Its own leaf module rather than an export of either presenter: the phone's
 * is behind a lazy boundary (`native/boot.ts` imports it dynamically), and a
 * static import of it from the browser presenter would pull the whole phone
 * path into the eager entry chunk.
 */
export function notificationTag(id: string, backendId: string): string {
  return backendId === '' ? id : `${backendId}|${id}`;
}
