<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { Terminal } from '@xterm/xterm';
  import { FitAddon } from '@xterm/addon-fit';
  import { WebLinksAddon } from '@xterm/addon-web-links';
  import { WebglAddon } from '@xterm/addon-webgl';
  import '@xterm/xterm/css/xterm.css';
  import {
    WriteTerminal,
    ResizeTerminal,
    GetTerminalReplay,
  } from '../../stores/bindings';
  import { getResolvedTheme } from '../../stores/themeMode.svelte';
  import { decodeTerminalOutput, encodeTerminalInput, normalizeTerminalReplay } from '../../types/terminal';
  import { getXtermTheme } from './terminalTheme';
  import {
    notifyTerminalFocus,
    type ThreadTerminalStateHandle,
  } from './terminalStore.svelte';
  import { eventEscapesTerminalToCommand } from '../../stores/keybindings.svelte';
  import { TERMINAL_ESCAPE_COMMAND_IDS } from '../../stores/paneNavCommands';

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

  // Build the xterm instance with its addons attached to the mount node.
  // Extracted from hydrate() so the long renderer-choice rationale doesn't bury
  // the replay/drain ordering that the rest of hydrate() turns on.
  function buildTerminal(mount: HTMLDivElement): { term: Terminal; fit: FitAddon } {
    const terminal = new Terminal({
      convertEol: false,
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace',
      fontSize: 13,
      // 1.0 so Unicode half-block / box-drawing glyphs (▀ ▄ █) tile with no
      // vertical gap — TUI sprite art and box borders render contiguously.
      // A larger line-height makes each cell taller than the glyph and breaks
      // block art into banded strips.
      lineHeight: 1.0,
      scrollback: 4000,
      allowProposedApi: true,
      theme: getXtermTheme(getResolvedTheme()),
    });

    const fitAddon = new FitAddon();
    terminal.loadAddon(fitAddon);
    terminal.loadAddon(new WebLinksAddon());
    terminal.open(mount);

    // Let app chords that stay active inside a focused terminal bubble to
    // App.svelte's window keydown handler instead of being encoded to the PTY:
    // pane navigation (alt+h/l, alt+shift+h/l) and terminal.refresh
    // (alt+shift+r → repaint). Returning false makes xterm skip its own handling
    // WITHOUT preventDefault/stopPropagation, so the event reaches the app.
    // Chords still gated on !terminalFocus (alt+arrow word-motion) are not
    // matched by the predicate and fall through to the shell as before. The
    // predicate reads live bindings, so a user rebind takes effect immediately.
    terminal.attachCustomKeyEventHandler((event) => {
      if (event.type !== 'keydown') return true;
      // Shift+Enter inserts a newline instead of submitting. xterm's default
      // sends CR (which the shell submits); we send LF (\n) — the byte Claude
      // Code / Codex (and anything binding Ctrl+J) read as "new line, don't
      // submit". At a bare shell LF is accept-line, identical to the CR it
      // replaces, so the prompt is unaffected. We write the byte ourselves and
      // fully consume the event: returning false stops xterm's CR,
      // preventDefault stops the browser inserting a stray newline into xterm's
      // helper textarea, and stopPropagation keeps it from reaching App.svelte's
      // window keydown handler. Only the bare chord qualifies — Ctrl/Alt/Meta+
      // Enter fall through so mod+enter (sidebar.cursor.open) isn't stolen.
      if (
        event.key === 'Enter' &&
        event.shiftKey &&
        !event.ctrlKey &&
        !event.altKey &&
        !event.metaKey
      ) {
        event.preventDefault();
        event.stopPropagation();
        WriteTerminal(terminalID, encodeTerminalInput('\n')).catch((err) => {
          console.error('terminal: WriteTerminal (shift+enter) failed', err);
        });
        return false;
      }
      return !eventEscapesTerminalToCommand(event, TERMINAL_ESCAPE_COMMAND_IDS);
    });

    // Renderer choice is load-bearing for TUI art, not just perf. xterm draws
    // box-drawing AND block/quadrant glyphs (U+2500–259F — the ▀ ▄ █ ▌ ▐ and
    // quadrant ▖▗▘▙▚▛▜▝▞▟ that Claude Code's startup sprite is built from) with
    // its own pixel-perfect custom-glyph atlas, but ONLY on the canvas/WebGL
    // renderers. The default DOM renderer emits text and defers those glyphs to
    // the system font, which tiles them with seams/misalignment (the fragmented-
    // sprite bug). WebGL needs a WebGL2 context; construction throws if it's
    // unavailable (headless tests, or any webview that disables 3D APIs —
    // the WSL WebView2 launcher used to ship `--disable-3d-apis`, which
    // silently forced this DOM fallback; see cmd/agent-overflow-windows
    // browserArgs), and onContextLoss reverts to the DOM renderer if the GPU
    // drops the context at runtime.
    try {
      const webgl = new WebglAddon();
      webgl.onContextLoss(() => {
        console.warn('terminal: WebGL context lost; reverting to DOM renderer');
        webgl.dispose();
      });
      terminal.loadAddon(webgl);
    } catch (err) {
      console.warn(
        'terminal: WebGL renderer unavailable; using DOM renderer ' +
          '(block/box glyphs fall back to the system font)',
        err,
      );
    }

    return { term: terminal, fit: fitAddon };
  }

  async function hydrate() {
    if (!mountEl || destroyed) return;

    const built = buildTerminal(mountEl);
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

    dataDisposable = term.onData((data) => {
      WriteTerminal(terminalID, encodeTerminalInput(data)).catch((err) => {
        console.error('terminal: WriteTerminal failed', err);
      });
    });

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
        ResizeTerminal(terminalID, rows, cols).catch((err) => {
          console.error('terminal: ResizeTerminal failed', err);
        });
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

  // Follow the app theme. Writing term.options.theme applies live —
  // background, foreground, and ANSI palette swap in place.
  $effect(() => {
    const mode = getResolvedTheme();
    if (!term) return;
    term.options.theme = getXtermTheme(mode);
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
</div>
