<script lang="ts">
  // Stub TerminalBody used by the TerminalSurface tests. The real
  // TerminalBody mounts an xterm instance (WebGL, addons) that does not run
  // under happy-dom, and its focus() forwards to term.focus(). The surface
  // binds the body and calls bodyEl.focus() from its rAF focus effect, so the
  // focus outcome tests need an `export function focus()` they can observe.
  // We record the call by terminalID into a shared module so the test can
  // assert which tab's body got focused after a remount.
  //
  // Like the real body, it registers a find provider under `findKey` while
  // mounted and offers its xterm textarea stand-in as `focusTarget()`; the
  // provider records what it is asked, by terminal, in the same module.
  import { onDestroy } from 'svelte';
  import { registerPaneFindProvider } from '../../lib/stores/paneFind.svelte';
  import { recordTerminalFind, recordTerminalFocus } from './terminalBodyFocusRecorder';

  interface Props {
    terminalID: string;
    findKey?: string;
  }

  let { terminalID, findKey }: Props = $props();

  let textarea: HTMLTextAreaElement | undefined = $state();

  // svelte-ignore state_referenced_locally
  const id = terminalID;
  let count = $state(0);
  // svelte-ignore state_referenced_locally
  const release = findKey
    ? registerPaneFindProvider(findKey, {
        noun: 'terminal',
        result: { get count() { return count; }, get current() { return count > 0 ? 0 : -1; }, pending: false },
        search(query: string) {
          recordTerminalFind(id, `search:${query}`);
          count = query ? 1 : 0;
        },
        next: () => recordTerminalFind(id, 'next'),
        previous: () => recordTerminalFind(id, 'previous'),
        clear() {
          recordTerminalFind(id, 'clear');
          count = 0;
        },
      })
    : null;

  onDestroy(() => release?.());

  export function focus(): void {
    recordTerminalFocus(terminalID);
  }

  export function focusTarget(): HTMLElement | null {
    return textarea ?? null;
  }
</script>

<div data-testid={`stub-terminal-body-${terminalID}`}>
  <textarea bind:this={textarea} data-testid={`stub-terminal-input-${terminalID}`}></textarea>
</div>
