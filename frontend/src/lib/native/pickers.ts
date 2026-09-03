// The file and camera pickers, DEFERRED.
//
// The seam exists and answers null so the composer's attachment path has
// one shape on every client rather than growing a shell branch later.
// What is deferred is the native side of it: a Capacitor camera / file
// plugin, its permissions, and the question of what a phone-side pick
// even means for a backend whose filesystem is on another machine.
//
// The shell is NOT without attachments in the meantime. Android's own
// file input handles `<input type="file">` from a WebView — which is what
// the composer already renders — so picking a photo works through the
// platform's chooser and rides the same `uploadAttachmentBytes` path
// every other client uses. What a native plugin would add is the camera
// opening directly and a share-sheet target, neither of which is a
// first-shell need.
//
// When it lands, this file grows the plugin behind the same
// `isNativeShell()` guard its siblings use, and nothing that calls it
// changes.

/** The shape a picked file would take. Nothing produces one yet. */
export interface PickedFile {
  name: string;
  mimeType: string;
  bytes: Blob;
}

/** Always null. See the header: the platform's own chooser covers this. */
export async function pickFile(): Promise<PickedFile | null> {
  return null;
}

/** Always null. See the header. */
export async function captureImage(): Promise<PickedFile | null> {
  return null;
}
