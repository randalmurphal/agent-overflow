// The boot phases a backend's current process reports failed, as the
// sentence the transport strip shows (components/shared/
// ComputerTransportStatus.svelte). A dismissal holds for that launch's
// failures: a later launch that reports failures shows them again, and a
// page reload shows them again, as the bundle notice does.

import { SvelteMap } from 'svelte/reactivity';
import type { BackendKey } from '../transport/backendKey';
import { bootFailureText } from '../transport/bootFailures';
import { getTransportHelloFor } from './transportStatus.svelte';

// Per backend, the launch and sentence last dismissed.
const dismissed = new SvelteMap<BackendKey, string>();

function currentNotice(backend: BackendKey): { key: string; text: string } {
  const hello = getTransportHelloFor(backend);
  const text = bootFailureText(hello?.bootFailures ?? []);
  return { key: `${hello?.launchId ?? ''}\n${text}`, text };
}

/** The sentence for `backend`'s failed boot phases, or '' when none failed
 *  or the person dismissed them. Reactive. */
export function getBootFailureNotice(backend: BackendKey): string {
  const { key, text } = currentNotice(backend);
  return text !== '' && dismissed.get(backend) !== key ? text : '';
}

export function dismissBootFailureNotice(backend: BackendKey): void {
  dismissed.set(backend, currentNotice(backend).key);
}

/** Test seam: forget every dismissal. */
export function __resetBootFailureNoticeForTest(): void {
  dismissed.clear();
}
