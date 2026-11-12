<script lang="ts">
  import { onMount, onDestroy, untrack } from 'svelte';
  import type { Terminal } from '@xterm/xterm';
  import type { FitAddon } from '@xterm/addon-fit';
  import {
    WriteTerminal,
    ResizeTerminal,
    GetTerminalReplay,
  } from '../../stores/bindings';
  import { getResolvedTheme } from '../../stores/themeMode.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { decodeTerminalOutput, encodeTerminalInput, normalizeTerminalReplay } from '../../types/terminal';
  import { getXtermTheme } from './terminalTheme';
  import { registerTerminalFind, type TerminalFindRegistration } from './terminalFind.svelte';
  import {
    terminalFocusRegion,
    type ThreadTerminalStateHandle,
  } from './terminalStore.svelte';
  import { closePaneFind, isPaneFindBarFocused } from '../../stores/paneFind.svelte';
  import { insertIntoFocusedFindInput } from '../panes/PaneFindBar.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
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
    // This terminal's find key: its find provider registers under it while
    // the xterm lives (stores/paneFind.svelte.ts).
    findKey: string;
  }

  let { handle, terminalID, paneId, findKey }: Props = $props();

  let mountEl: HTMLDivElement | undefined = $state();
  let term: Terminal | null = null;
  let fit: FitAddon | null = null;
  // The widget's clipboard paste and read, for the compact key row. Null
  // until hydrate() has built the term; a press before then does nothing.
  let paste: (() => void) | null = null;
  let readClipboard: (() => Promise<string>) | null = null;
  let resizeObserver: ResizeObserver | null = null;
  let dataDisposable: { dispose(): void } | null = null;
  // Unregisters this xterm from the store handle (terminal.clear reaches the
  // active tab's xterm through it). Null until hydrate() has built the term.
  let detachXterm: (() => void) | null = null;
  // The xterm's registered find. Null until hydrate()'s writes are parsed.
  let find: TerminalFindRegistration | null = null;
  let destroyed = false;
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

  // Compact (phone) layout docks a key row under the terminal for the keys a
  // soft keyboard has no way to produce. Nothing below this point runs on
  // desktop, where `compact` is false and the row never mounts.
  const compact = $derived(isCompactLayout());
  // Sticky Ctrl, armed by the key row's Ctrl button and spent by the next input
  // chunk on the path below — whatever its source.
  let ctrlArmed = $state(false);
  // While this terminal's find bar holds keyboard focus, nothing the reader
  // does reaches the PTY: the key row types into the query instead, and a
  // paste whose clipboard read outlived the xterm's focus is dropped.
  // xterm's own replies to the program (cursor and focus reports) still go
  // through writeInput, so the gate is on the reader's entries, not there.
  const findFocused = $derived(isPaneFindBarFocused(findKey));

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
    if (findFocused) {
      if (data === '\x1b') closePaneFind(findKey);
      else insertIntoFocusedFindInput(data);
      return;
    }
    term?.input(data, true);
  }

  function toggleStickyCtrl(): void {
    ctrlArmed = !ctrlArmed;
  }

  function pasteKeyRow(): void {
    if (!findFocused) {
      paste?.();
      return;
    }
    readClipboard?.()
      .then((text) => {
        if (!destroyed) insertIntoFocusedFindInput(text);
      })
      .catch((err) => {
        console.error('terminal: clipboard paste into find failed', err);
        addErrorToast(`Paste failed: ${errString(err)}`, err);
      });
  }

  async function hydrate() {
    if (!mountEl || destroyed) return;

    const built = buildTerminal(mountEl, {
      onInput: writeInput,
      isDisposed: () => destroyed,
      acceptsPaste: () => !isPaneFindBarFocused(findKey),
    });
    term = built.term;
    fit = built.fit;
    paste = built.paste;
    readClipboard = built.readClipboard;
    detachXterm = handle.attachXterm(terminalID, term);

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

    // Writes parse asynchronously; find registers after the last of
    // hydrate()'s writes is parsed, so a kept query searches the restored
    // buffer rather than an empty one.
    let wrote = false;
    // Load replay buffer before draining pending output so order is correct.
    try {
      const replay = normalizeTerminalReplay(await GetTerminalReplay(terminalID) as unknown);
      if (!destroyed && replay.data) {
        term.write(decodeTerminalOutput(replay.data));
        wrote = true;
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
        wrote = true;
      } catch (err) {
        console.error('terminal: write during hydrate drain failed', err);
      }
    }

    // Replay applied and we're caught up — open the drain $effect for live
    // output from here on. (Flipping this re-runs the effect, which finds the
    // queue empty after the drain above; subsequent output drains normally.)
    hydrated = true;

    // Find registers once the grid is fitted to the parsed buffer, a frame
    // after the fit: a change of rows after the search drops its match, and
    // xterm's viewport, catching up with a resize later, can scroll back a
    // search made right after the fit.
    const registerFind = () => {
      if (!destroyed) find = registerTerminalFind(findKey, built);
    };
    const fitThenRegister = () => {
      requestAnimationFrame(() => {
        fitNow();
        requestAnimationFrame(registerFind);
      });
    };
    if (wrote) term.write('', fitThenRegister);
    else fitThenRegister();

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
      fitNow();
    });
  }

  function fitNow() {
    if (!fit || !term || destroyed) return;
    try {
      fit.fit();
      const { rows, cols } = term;
      resizeWriter.resize(rows, cols);
    } catch (err) {
      console.error('terminal: fit failed', err);
    }
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
    // Match grounds come from the same palette; repaint them with it.
    untrack(() => find?.restyle());
  });

  // Follow the app font-size setting (the mod+/- zoom chords) live. Writing
  // options.fontSize re-measures the cell grid; the mount box hasn't changed,
  // so no ResizeObserver tick follows and the refit has to be requested here
  // (it also round-trips the new rows/cols to the PTY). buildTerminal read the
  // setting for the initial size, so a tick without a term registers only the
  // dependency.
  $effect(() => {
    const fontSize = getSettings().fontSize;
    if (!term) return;
    term.options.fontSize = fontSize;
    scheduleFit();
  });

  onMount(() => {
    hydrate();
  });

  onDestroy(() => {
    destroyed = true;
    dataDisposable?.dispose();
    dataDisposable = null;
    // Before the xterm goes: the release clears the provider's decorations.
    find?.release();
    find = null;
    detachXterm?.();
    detachXterm = null;
    inputWriter.dispose();
    resizeWriter.dispose();
    resizeObserver?.disconnect();
    resizeObserver = null;
    term?.dispose();
    term = null;
    fit = null;
    paste = null;
    readClipboard = null;
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

  /** The element that holds the terminal's keyboard focus, once built. */
  export function focusTarget(): HTMLElement | null {
    return term?.textarea ?? null;
  }
</script>

<div class="flex-1 min-h-0 flex flex-col bg-terminal-bg" data-testid={`terminal-body-${terminalID}`}>
  <div bind:this={mountEl} class="flex-1 min-h-0 bg-terminal-bg" {@attach terminalFocusRegion(paneId)}></div>
  {#if compact}
    <TerminalKeyRow onKey={pressKeyRow} {ctrlArmed} onToggleCtrl={toggleStickyCtrl} onPaste={pasteKeyRow} {findFocused} />
  {/if}
</div>
