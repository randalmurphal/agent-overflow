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

  interface Props {
    handle: ThreadTerminalStateHandle;
    terminalID: string;
  }

  let { handle, terminalID }: Props = $props();

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

  function handleFocusIn(): void {
    if (focusCounted) return;
    focusCounted = true;
    notifyTerminalFocus(true);
  }

  function handleFocusOut(): void {
    if (!focusCounted) return;
    focusCounted = false;
    notifyTerminalFocus(false);
  }

  async function hydrate() {
    if (!mountEl || destroyed) return;

    term = new Terminal({
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

    fit = new FitAddon();
    term.loadAddon(fit);
    term.loadAddon(new WebLinksAddon());
    term.open(mountEl);

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
      term.loadAddon(webgl);
    } catch (err) {
      console.warn(
        'terminal: WebGL renderer unavailable; using DOM renderer ' +
          '(block/box glyphs fall back to the system font)',
        err,
      );
    }

    // Wire focus/blur listeners on the xterm mount. xterm puts a focusable
    // textarea inside mountEl, so focusin/focusout bubble up reliably.
    mountEl.addEventListener('focusin', handleFocusIn);
    mountEl.addEventListener('focusout', handleFocusOut);

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

    // Drain any output that arrived while we were mounting.
    for (const chunk of handle.drainOutput(terminalID)) {
      term.write(chunk);
    }

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
  // the tab's pendingOutput array is mutated.
  $effect(() => {
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
    // If the terminal was focused when the drawer closed, drop the counter
    // so terminalFocus doesn't remain sticky in the keybindings context.
    if (focusCounted) {
      focusCounted = false;
      notifyTerminalFocus(false);
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
