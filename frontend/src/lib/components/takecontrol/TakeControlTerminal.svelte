<script lang="ts">
  // The take-control terminal surface: a live xterm mirror of a claude-tui
  // session's PTY. Output arrives out-of-band on the `provider:terminal_output`
  // event (the backend installs the fan-out sink on attach); input is gated on
  // the take-control lease so a read-only attach can't inject keystrokes. The
  // xterm construction (renderer, glyph + key handling) is shared with the
  // app terminal via terminalXterm.buildTerminal — only the I/O backend differs.
  //
  // The attach belongs to THIS connection. A second pane elsewhere attaches
  // alongside rather than displacing this one, its take-control request is
  // refused while this one holds the lease, and a socket that dies without
  // unmounting gives the lease back on its own — so the detach below is the
  // tidy path, not the only one.
  import { onMount, onDestroy } from 'svelte';
  import type { Terminal } from '@xterm/xterm';
  import type { FitAddon } from '@xterm/addon-fit';
  import Hand from '@lucide/svelte/icons/hand';
  import Eye from '@lucide/svelte/icons/eye';
  import {
    ProviderTerminalAttach,
    ProviderTerminalDetach,
    ProviderTerminalReplay,
    ProviderTerminalInput,
    ProviderTerminalResize,
    ProviderTerminalSetControl,
  } from '../../stores/bindings';
  import { getResolvedTheme } from '../../stores/themeMode.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import {
    decodeTerminalOutput,
    encodeTerminalInput,
    normalizeTerminalReplay,
    type TerminalOutputEventPayload,
  } from '../../types/terminal';
  import { getXtermTheme } from '../terminal/terminalTheme';
  import { buildTerminal } from '../terminal/terminalXterm';
  import {
    createTerminalInputWriter,
    createTerminalResizeWriter,
  } from '../terminal/terminalIoQueue';
  import { notifyTerminalFocus } from '../terminal/terminalStore.svelte';
  import { wailsEventOn } from '../../stores/wailsEvents';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';

  let { paneId, threadId }: { paneId: string; threadId: string } = $props();

  let mountEl: HTMLDivElement | undefined = $state();
  let term: Terminal | null = null;
  let fit: FitAddon | null = null;
  let resizeObserver: ResizeObserver | null = null;
  let dataDisposable: { dispose(): void } | null = null;
  let cancelOutput: (() => void) | null = null;
  let destroyed = false;
  let focusCounted = false;

  // The human take-control lease. Read-only by default: keystrokes are swallowed
  // until the user acquires control, so AO's programmatic Send and the human
  // never drive the PTY at once.
  let controlHeld = $state(false);
  let controlTransitionPending = $state(false);
  let attaching = $state(true);
  let attachError = $state<string | null>(null);

  const inputWriter = createTerminalInputWriter(
    (data) => ProviderTerminalInput(threadId, encodeTerminalInput(data)),
    (err) => console.error('take-control: ProviderTerminalInput failed', err),
  );
  const resizeWriter = createTerminalResizeWriter(
    (rows, cols) => ProviderTerminalResize(threadId, rows, cols),
    (err) => console.error('take-control: ProviderTerminalResize failed', err),
  );

  // Output buffered during the replay round-trip, drained in order afterward.
  // Mirrors TerminalBody's replay/drain hazard: chunks that land mid-hydrate
  // must wait for the replay frame to be written first, then dedupe against the
  // replay watermark so a chunk already in the replay isn't written twice. The
  // sequence is kept with each chunk because the backend ring keeps buffering
  // across attach — events in the attach→replay window appear in BOTH the live
  // stream AND the replay snapshot, so the drain must drop those at/under the
  // watermark (the arrival-time guard can't, since replayThrough is still 0 then).
  let hydrated = false;
  const pendingOutput: Array<{ sequence: number; bytes: Uint8Array }> = [];
  let replayThrough = 0;

  function writeInput(data: string): void {
    // Read-only attach: drop keystrokes entirely rather than round-trip to a
    // backend that would reject them.
    if (!controlHeld || destroyed) return;
    inputWriter.write(data);
  }

  function handleOutput(payload: TerminalOutputEventPayload): void {
    if (payload.threadID !== threadId) return;
    // Replay already carried everything up to its watermark; skip the overlap.
    if (payload.sequence <= replayThrough) return;
    // Decode defensively so one malformed chunk can't throw out of the event
    // callback and strand the live stream (the replay path is already guarded).
    let bytes: Uint8Array;
    try {
      bytes = decodeTerminalOutput(payload.data);
    } catch (err) {
      console.error('take-control: decode output chunk failed', err);
      return;
    }
    if (!hydrated) {
      pendingOutput.push({ sequence: payload.sequence, bytes });
      return;
    }
    if (term && !destroyed) term.write(bytes);
  }

  function handleFocusIn(): void {
    if (focusCounted) return;
    focusCounted = true;
    notifyTerminalFocus(paneId, true);
  }

  function handleFocusOut(): void {
    if (!focusCounted) return;
    focusCounted = false;
    notifyTerminalFocus(paneId, false);
  }

  async function hydrate(): Promise<void> {
    if (!mountEl || destroyed) return;

    // Subscribe to output BEFORE attaching so no chunk is missed between the
    // backend installing its fan-out sink (on attach) and our handler existing.
    cancelOutput = wailsEventOn<TerminalOutputEventPayload>(
      'provider:terminal_output',
      handleOutput,
    );

    let summary: { cols: number; rows: number } | null = null;
    try {
      const handle = await ProviderTerminalAttach(threadId);
      // SessionSummary.cols/rows are optional on the wire; 0 falls through the
      // `> 0` guard below to leave xterm at its default until the first fit().
      summary = { cols: handle.summary.cols ?? 0, rows: handle.summary.rows ?? 0 };
    } catch (err) {
      attaching = false;
      attachError = errString(err);
      cancelOutput?.();
      cancelOutput = null;
      return;
    }
    if (destroyed) return;
    attaching = false;

    const built = buildTerminal(mountEl, { onInput: writeInput, isDisposed: () => destroyed });
    term = built.term;
    fit = built.fit;

    mountEl.addEventListener('focusin', handleFocusIn);
    mountEl.addEventListener('focusout', handleFocusOut);

    // Size to the width the provider last drew at BEFORE writing the replay, so
    // the frame's cursor/erase escapes land against the right grid (same
    // reasoning as TerminalBody — a same-size reopen never triggers a SIGWINCH
    // to heal a mis-sized frame).
    if (summary && summary.cols > 0 && summary.rows > 0) {
      try {
        term.resize(summary.cols, summary.rows);
      } catch (err) {
        console.error('take-control: pre-replay resize failed', err);
      }
    }

    try {
      const replay = normalizeTerminalReplay(await ProviderTerminalReplay(threadId));
      if (!destroyed && replay.data) {
        term.write(decodeTerminalOutput(replay.data));
      }
      replayThrough = replay.throughSequence;
    } catch (err) {
      console.error('take-control: ProviderTerminalReplay failed', err);
    }

    if (destroyed || !term) return;

    // Drain output buffered during the await, after the replay so dedup by the
    // watermark holds. Chunks at/under the watermark are already in the replay
    // frame just written, so skip them. Guard each write so a throw can't strand
    // the gate shut.
    for (const chunk of pendingOutput.splice(0)) {
      if (chunk.sequence <= replayThrough) continue;
      try {
        term.write(chunk.bytes);
      } catch (err) {
        console.error('take-control: write during hydrate drain failed', err);
      }
    }
    hydrated = true;

    dataDisposable = term.onData(writeInput);
    attachResizeObserver();
    scheduleFit();
  }

  function attachResizeObserver(): void {
    if (!mountEl || resizeObserver) return;
    resizeObserver = new ResizeObserver(() => scheduleFit());
    resizeObserver.observe(mountEl);
  }

  let fitPending = false;
  function scheduleFit(): void {
    if (fitPending || destroyed) return;
    fitPending = true;
    requestAnimationFrame(() => {
      fitPending = false;
      if (!fit || !term || destroyed) return;
      try {
        fit.fit();
        const { rows, cols } = term;
        resizeWriter.resize(rows, cols);
      } catch (err) {
        console.error('take-control: fit failed', err);
      }
    });
  }

  // Follow the app palette live — mode flips AND theme-file edits, since
  // `getXtermTheme` reads the palette identity. Resolved before the `term`
  // guard so the dependency is registered even on a tick with no terminal.
  $effect(() => {
    const theme = getXtermTheme(getResolvedTheme());
    if (!term) return;
    term.options.theme = theme;
  });

  // Follow the app font-size setting (the mod+/- zoom chords) live; same
  // shape as TerminalBody — the mount box doesn't change, so the refit (and
  // its PTY resize) has to be requested here.
  $effect(() => {
    const fontSize = getSettings().fontSize;
    if (!term) return;
    term.options.fontSize = fontSize;
    scheduleFit();
  });

  async function toggleControl(): Promise<void> {
    if (controlTransitionPending) return;
    controlTransitionPending = true;
    const next = !controlHeld;
    const releasing = !next;
    try {
      if (releasing) {
        // Close the local gate before waiting: the transport executes unrelated
        // RPCs concurrently, so the lease-release call must not overtake an
        // earlier input write or accept more keystrokes while that write drains.
        controlHeld = false;
        await inputWriter.idle();
        if (destroyed) return;
      }
      await ProviderTerminalSetControl(threadId, next);
      if (destroyed) {
        // Detach normally releases the lease, but an acquire RPC that was
        // already in flight can complete after detach and re-establish it.
        // Issue a compensating release after that response so false is the
        // deterministic final backend state for an unmounted pane.
        if (next) {
          try {
            await ProviderTerminalSetControl(threadId, false);
          } catch (err) {
            console.error('take-control: late lease release failed', err);
          }
        }
        return;
      }
      controlHeld = next;
      if (next) term?.focus();
    } catch (err) {
      if (destroyed) {
        console.error('take-control: control transition failed after destroy', err);
      } else {
        if (releasing) controlHeld = true;
        addToast('error', `Take control failed: ${errString(err)}`);
      }
    } finally {
      controlTransitionPending = false;
    }
  }

  onMount(() => {
    void hydrate();
  });

  onDestroy(() => {
    destroyed = true;
    cancelOutput?.();
    cancelOutput = null;
    if (mountEl) {
      mountEl.removeEventListener('focusin', handleFocusIn);
      mountEl.removeEventListener('focusout', handleFocusOut);
    }
    if (focusCounted) {
      focusCounted = false;
      notifyTerminalFocus(paneId, false);
    }
    // Detach releases this connection's take-control lease and drops it from
    // the output fan-out in one call. Another pane attached to the same
    // session keeps both.
    ProviderTerminalDetach(threadId).catch((err) => {
      console.error('take-control: ProviderTerminalDetach failed', err);
    });
    dataDisposable?.dispose();
    dataDisposable = null;
    inputWriter.dispose();
    resizeWriter.dispose();
    resizeObserver?.disconnect();
    resizeObserver = null;
    term?.dispose();
    term = null;
    fit = null;
  });
</script>

<div class="flex h-full min-h-0 flex-col bg-terminal-bg" data-testid={`take-control-terminal-${paneId}`}>
  <div
    class="flex items-center gap-2 px-2 py-1 border-b border-border-subtle/70 shrink-0"
    data-thread-id={threadId}
  >
    <Button
      variant={controlHeld ? 'primary' : 'secondary'}
      size="xs"
      pressed={controlHeld}
      ariaLabel={controlHeld ? 'Release control' : 'Take control'}
      title={controlHeld
        ? 'Release control — return the terminal to read-only'
        : 'Take control — drive the live Claude TUI'}
      onclick={() => void toggleControl()}
      disabled={attaching || attachError !== null || controlTransitionPending}
    >
      {#snippet leading()}
        <Icon icon={controlHeld ? Hand : Eye} size={12} strokeWidth={2} />
      {/snippet}
      {#snippet children()}{controlHeld ? 'Control' : 'Read-only'}{/snippet}
    </Button>
    <span class="text-[11px] text-fg-muted">
      {#if attaching}Attaching…{:else if controlHeld}You are driving the terminal{:else}Live mirror{/if}
    </span>
  </div>

  {#if attachError}
    <div class="flex flex-1 min-h-0 items-center justify-center px-4 text-center text-sm text-error">
      Could not attach to the Claude TUI session: {attachError}
    </div>
  {:else}
    <div bind:this={mountEl} class="flex-1 min-h-0 bg-terminal-bg" data-testid="take-control-mount"></div>
  {/if}
</div>
