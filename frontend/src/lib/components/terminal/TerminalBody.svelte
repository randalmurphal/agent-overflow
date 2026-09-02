<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import type { Terminal } from '@xterm/xterm';
  import type { FitAddon } from '@xterm/addon-fit';
  import {
    WriteTerminal,
    ResizeTerminal,
    GetTerminalReplay,
  } from '../../stores/bindings';
  import { getResolvedTheme } from '../../stores/themeMode.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { decodeTerminalOutput, encodeTerminalInput, normalizeTerminalReplay } from '../../types/terminal';
  import { getXtermTheme } from './terminalTheme';
  import {
    notifyTerminalFocus,
    type ThreadTerminalStateHandle,
  } from './terminalStore.svelte';
  import { buildTerminal } from './terminalXterm';
  import { applyStickyCtrl } from './terminalKeys';
  import TerminalKeyRow from './TerminalKeyRow.svelte';
  import {
    createTerminalInputWriter,
    createTerminalResizeWriter,
  } from './terminalIoQueue';

  interface Props {
    handle: ThreadTerminalStateHandle;
    terminalID: string;
    // The owning pane's id. The focus registry is keyed by pane so two
    // terminal panes gate their `terminalFocus` chords independently.
    paneId: string;
  }

  let { handle, terminalID, paneId }: Props = $props();

  let mountEl: HTMLDivElement | undefined = $state();
  let term: Terminal | null = null;
  let fit: FitAddon | null = null;
  let resizeObserver: ResizeObserver | null = null;
  let dataDisposable: { dispose(): void } | null = null;
  let destroyed = false;
  // Track whether we've already told the focus registry we're focused so
  // we don't double-decrement on teardown.
  let focusCounted = false;
  // A focus() request can arrive before hydrate() has opened the xterm (it
  // awaits a replay round-trip). Latch it here; hydrate()'s tail focuses once
  // the terminal exists.
  let pendingFocus = false;
  // hydrate() awaits the replay round-trip before draining buffered output, so
  // there's a window where `term` exists but the replay hasn't been written.
  // The drain $effect must stay closed across that window: output that lands
  // mid-await has to flow through hydrate()'s post-markReplayed drain (so the
  // replay buffer lands first and replay-covered chunks get deduped), not the
  // effect. Reactive so flipping it re-runs the effect to drain what queued.
  let hydrated = $state(false);

  const inputWriter = createTerminalInputWriter(
    (data) => WriteTerminal(terminalID, encodeTerminalInput(data)),
    (err) => console.error('terminal: WriteTerminal failed', err),
  );
  const resizeWriter = createTerminalResizeWriter(
    (rows, cols) => ResizeTerminal(terminalID, rows, cols),
    (err) => console.error('terminal: ResizeTerminal failed', err),
  );

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

  // Compact (phone) layout docks a key row under the terminal for the keys a
  // soft keyboard has no way to produce. Nothing below this point runs on
  // desktop, where `compact` is false and the row never mounts.
  const compact = $derived(isCompactLayout());
  // Sticky Ctrl, armed by the key row's Ctrl button and spent by the next input
  // chunk on the path below — whatever its source.
  let ctrlArmed = $state(false);

  // Every keystroke (main stream via term.onData, plus the Shift+Enter newline
  // the widget produces internally, plus every compact key-row press, which
  // enters through term.input so it lands on onData too) routes through here to
  // the app terminal manager. Passed to buildTerminal as `onInput` and wired to
  // term.onData so a single path owns input — and so sticky Ctrl catches soft-
  // keyboard letters, not just key-row ones.
  function writeInput(data: string): void {
    if (ctrlArmed) {
      // Armed is the rare branch; the common path stays one boolean read.
      const spent = applyStickyCtrl(data, true);
      ctrlArmed = spent.armed;
      inputWriter.write(spent.data);
      return;
    }
    inputWriter.write(data);
  }

  // Key-row press. Goes in through term.input (wasUserInput: true) rather than
  // straight to inputWriter, so the row shares the keyboard's path: one PTY
  // writer, one place sticky Ctrl applies, and xterm's own user-input side
  // effects (selection clear, scroll-to-bottom) still happen.
  function pressKeyRow(data: string): void {
    term?.input(data, true);
  }

  function toggleStickyCtrl(): void {
    ctrlArmed = !ctrlArmed;
  }

  async function hydrate() {
    if (!mountEl || destroyed) return;

    const built = buildTerminal(mountEl, { onInput: writeInput, isDisposed: () => destroyed });
    term = built.term;
    fit = built.fit;

    // Wire focus/blur listeners on the xterm mount. xterm puts a focusable
    // textarea inside mountEl, so focusin/focusout bubble up reliably.
    mountEl.addEventListener('focusin', handleFocusIn);
    mountEl.addEventListener('focusout', handleFocusOut);

    // Size the grid to the width the provider last drew at BEFORE writing the
    // replay. A fresh xterm defaults to 80x24; the replay blob is the provider's
    // most recent frame, laid out for the PTY's current (rows, cols). Writing it
    // into an 80-col grid bakes the provider's cursor/erase escapes against the
    // wrong width — the later fit() reflow rewraps cells but can't re-run those
    // already-consumed escapes, so the frame stays mangled until a real resize
    // forces a repaint. That is the close→reopen corruption: a same-size reopen
    // never changes the PTY winsize, so no SIGWINCH ever arrives to heal it. The
    // backend reports its cached size in the summary (== the width the frame was
    // drawn for), so sizing to it here makes the replay land correctly no matter
    // when flex layout settles. scheduleFit() below then measures the real pane;
    // if it genuinely differs, that resize round-trips a SIGWINCH and the
    // provider repaints at the new width.
    const summary = handle.tabs.find((t) => t.terminalID === terminalID)?.summary;
    if (summary && summary.cols > 0 && summary.rows > 0) {
      try {
        term.resize(summary.cols, summary.rows);
      } catch (err) {
        console.error('terminal: pre-replay resize to backend size failed', err);
      }
    }

    // Load replay buffer before draining pending output so order is correct.
    try {
      const replay = normalizeTerminalReplay(await GetTerminalReplay(terminalID) as unknown);
      if (!destroyed && replay.data) {
        term.write(decodeTerminalOutput(replay.data));
      }
      handle.markReplayed(terminalID, replay.fromSequence, replay.throughSequence);
    } catch (err) {
      console.error('terminal: GetTerminalReplay failed', err);
    }

    if (destroyed || !term) return;

    // Drain output buffered during the await, after markReplayed so
    // replay-covered chunks are already dropped. Each write is guarded: a throw
    // escaping here would reject the un-awaited hydrate() silently and leave the
    // gate (`hydrated`, set below) shut forever, stranding all future output.
    // The steady-state $effect needs no guard — it runs with the gate already
    // open and re-drains on the next event. Log and continue so the gate opens.
    for (const chunk of handle.drainOutput(terminalID)) {
      try {
        term.write(chunk);
      } catch (err) {
        console.error('terminal: write during hydrate drain failed', err);
      }
    }

    // Replay applied and we're caught up — open the drain $effect for live
    // output from here on. (Flipping this re-runs the effect, which finds the
    // queue empty after the drain above; subsequent output drains normally.)
    hydrated = true;

    dataDisposable = term.onData(writeInput);

    attachResizeObserver();
    scheduleFit();

    if (pendingFocus) {
      pendingFocus = false;
      focus();
    }
  }

  function attachResizeObserver() {
    if (!mountEl || resizeObserver) return;
    resizeObserver = new ResizeObserver(() => scheduleFit());
    resizeObserver.observe(mountEl);
  }

  let fitPending = false;
  function scheduleFit() {
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
        console.error('terminal: fit failed', err);
      }
    });
  }

  // React to output chunks accumulated in the store. An $effect fires whenever
  // the tab's pendingOutput array is mutated. Gated on `hydrated` so output that
  // arrives mid-hydrate stays queued for hydrate()'s ordered drain — see the
  // `hydrated` declaration for the ordering/dedup hazard this closes.
  $effect(() => {
    if (!hydrated) return;
    const tab = handle.tabs.find((t) => t.terminalID === terminalID);
    if (!tab || !term || tab.pendingOutput.length === 0) return;
    for (const chunk of handle.drainOutput(terminalID)) {
      term.write(chunk);
    }
  });

  // Follow the app palette. Writing term.options.theme applies live —
  // background, foreground, and ANSI palette swap in place.
  //
  // The theme is resolved BEFORE the `term` guard on purpose: `getXtermTheme`
  // is what reads the palette identity, so a tick that finds no terminal yet
  // must still register the dependency, or the pane that mounts next never
  // re-themes when a theme file changes under an unchanged mode.
  $effect(() => {
    const theme = getXtermTheme(getResolvedTheme());
    if (!term) return;
    term.options.theme = theme;
  });

  onMount(() => {
    hydrate();
  });

  onDestroy(() => {
    destroyed = true;
    if (mountEl) {
      mountEl.removeEventListener('focusin', handleFocusIn);
      mountEl.removeEventListener('focusout', handleFocusOut);
    }
    // If the terminal was focused when the drawer closed, drop this pane's
    // count so terminalFocus doesn't remain sticky in the keybindings context.
    if (focusCounted) {
      focusCounted = false;
      notifyTerminalFocus(paneId, false);
    }
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

  export function focus() {
    if (destroyed) return;
    if (!term) {
      // hydrate() hasn't opened the terminal yet; its tail focuses on completion.
      pendingFocus = true;
      return;
    }
    // xterm attaches its helper textarea synchronously inside term.open(), and
    // that textarea is immediately focusable — it's hidden via opacity/offscreen
    // position, never display:none, so focus() is not gated on the first render.
    // A single call lands; no deferral or retry needed. (Verified against the
    // installed @xterm/xterm 6.0.0: Terminal.focus -> core.focus ->
    // this.textarea.focus(), guarded only by the textarea existing.)
    term.focus();
  }
</script>

<div class="flex-1 min-h-0 flex flex-col bg-terminal-bg" data-testid={`terminal-body-${terminalID}`}>
  <div bind:this={mountEl} class="flex-1 min-h-0 bg-terminal-bg"></div>
  {#if compact}
    <TerminalKeyRow onKey={pressKeyRow} {ctrlArmed} onToggleCtrl={toggleStickyCtrl} />
  {/if}
</div>
