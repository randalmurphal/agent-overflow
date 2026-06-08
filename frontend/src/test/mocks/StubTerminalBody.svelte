<script lang="ts">
  // Stub TerminalBody used by TerminalSurface.focus.test.ts. The real
  // TerminalBody mounts an xterm instance (WebGL, addons) that does not run
  // under happy-dom, and its focus() forwards to term.focus(). The surface
  // binds the body and calls bodyEl.focus() from its rAF focus effect, so the
  // only contract the outcome test needs is an `export function focus()` it can
  // observe. We record the call by terminalID into a shared module so the test
  // can assert which tab's body got focused after a remount.
  import { recordTerminalFocus } from './terminalBodyFocusRecorder';

  interface Props {
    terminalID: string;
  }

  let { terminalID }: Props = $props();

  export function focus(): void {
    recordTerminalFocus(terminalID);
  }
</script>

<div data-testid={`stub-terminal-body-${terminalID}`}></div>
