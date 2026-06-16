// Shared xterm.js construction for every terminal surface in the app — the
// app-owned thread terminal (TerminalBody) and the claude-tui take-control
// mirror (TakeControlTerminal). Both need the SAME load-bearing setup:
//
//   - The WebGL renderer (with DOM fallback) — required, not just for perf, so
//     box-drawing and block/quadrant glyphs (the ▀ ▄ █ ▌ ▐ TUI sprite art) tile
//     seamlessly instead of falling back to a seam-prone system font.
//   - The custom key handler: Shift+Enter → newline (not submit), Cmd/
//     Ctrl+Shift+C/V clipboard, and the app-chord escape predicate that lets
//     pane-nav / refresh / tab chords bubble to the window handler.
//
// Keeping this in one place means the two surfaces can't drift on glyph
// rendering or key handling. The only thing the caller owns is where input
// goes: `onInput` receives every keystroke the widget produces internally
// (currently the Shift+Enter newline), and the caller separately wires
// `term.onData(onInput)` for the main keystroke stream — so a single gate (e.g.
// the take-control read-only lease) governs both paths.

import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebLinksAddon } from '@xterm/addon-web-links';
import { WebglAddon } from '@xterm/addon-webgl';
import '@xterm/xterm/css/xterm.css';
import { getResolvedTheme } from '../../stores/themeMode.svelte';
import { getXtermTheme } from './terminalTheme';
import { eventEscapesTerminalToCommand } from '../../stores/keybindings.svelte';
import { TERMINAL_ESCAPE_COMMAND_IDS } from '../../stores/paneNavCommands';
import { copyToClipboard } from '../../utils/clipboard';
import { addToast } from '../../stores/toast.svelte';
import { errString } from '../../utils/errors';
import { isClipboardChord } from './terminalKeys';

const isMac =
  typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.platform);

export interface BuildTerminalOptions {
  // Receives input the widget produces internally (the Shift+Enter newline).
  // Wire the same function into `term.onData` for the main keystroke stream so
  // one gate governs both paths.
  onInput: (data: string) => void;
  // Reports whether the owning surface has been torn down. Paste is the one
  // handler that writes back to the terminal AFTER an async gap (the clipboard
  // read), so if the component unmounts mid-read this guard drops the late
  // write rather than touching a disposed xterm. Lifecycle stays owned by the
  // caller; buildTerminal just consults it. Optional — omit for surfaces with
  // no teardown race.
  isDisposed?: () => boolean;
}

// Build the xterm instance with its addons attached to the mount node. Extracted
// so the long renderer-choice rationale doesn't bury the replay/drain ordering
// in each terminal surface, and so both surfaces share identical glyph + key
// handling.
export function buildTerminal(
  mount: HTMLDivElement,
  { onInput, isDisposed }: BuildTerminalOptions,
): { term: Terminal; fit: FitAddon } {
  const terminal = new Terminal({
    convertEol: false,
    cursorBlink: true,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace',
    fontSize: 13,
    // 1.0 so Unicode half-block / box-drawing glyphs (▀ ▄ █) tile with no
    // vertical gap — TUI sprite art and box borders render contiguously. A
    // larger line-height makes each cell taller than the glyph and breaks block
    // art into banded strips.
    lineHeight: 1.0,
    scrollback: 4000,
    allowProposedApi: true,
    theme: getXtermTheme(getResolvedTheme()),
  });

  const fitAddon = new FitAddon();
  terminal.loadAddon(fitAddon);
  terminal.loadAddon(new WebLinksAddon());
  terminal.open(mount);

  // Runs on every keydown in a focused xterm. It first fully consumes the
  // in-widget special cases handled below — Shift+Enter (newline) and copy/
  // paste (Cmd, or Ctrl+Shift+C/V) — so they never reach the shell. Everything
  // else falls to the escape predicate: app chords that stay active inside a
  // focused terminal bubble to App.svelte's window keydown handler instead of
  // being encoded to the PTY — pane navigation (alt+h/l, alt+shift+h/l),
  // terminal.refresh (alt+shift+r → repaint), terminal tab management
  // (mod+shift+t/w, ctrl+tab/ctrl+shift+tab), and new pane (mod+shift+~).
  // Returning false makes xterm skip its own handling WITHOUT preventDefault/
  // stopPropagation, so the event reaches the app. Chords still gated on
  // !terminalFocus are not matched by the predicate and fall through to the
  // shell as before. The predicate reads live bindings, so a user rebind takes
  // effect immediately.
  terminal.attachCustomKeyEventHandler((event) => {
    if (event.type !== 'keydown') return true;
    // Shift+Enter inserts a newline instead of submitting. xterm's default
    // sends CR (which the shell submits); we send LF (\n) — the byte Claude
    // Code / Codex (and anything binding Ctrl+J) read as "new line, don't
    // submit". At a bare shell LF is accept-line, identical to the CR it
    // replaces, so the prompt is unaffected. We route the byte through onInput
    // (so the caller's input gate governs it) and fully consume the event:
    // returning false stops xterm's CR, preventDefault stops the browser
    // inserting a stray newline into xterm's helper textarea, and
    // stopPropagation keeps it from reaching App.svelte's window keydown
    // handler. Only the bare chord qualifies — Ctrl/Alt/Meta+Enter fall through
    // so mod+enter (sidebar.cursor.open) isn't stolen.
    if (
      event.key === 'Enter' &&
      event.shiftKey &&
      !event.ctrlKey &&
      !event.altKey &&
      !event.metaKey
    ) {
      event.preventDefault();
      event.stopPropagation();
      onInput('\n');
      return false;
    }
    // Copy: Cmd+C (macOS) / Ctrl+Shift+C. Copy the selection to the clipboard
    // and fully consume the event so it never reaches the PTY. A copy chord
    // with no selection is a no-op but still consumed (never meant for the
    // shell).
    if (isClipboardChord(event, 'c', isMac)) {
      event.preventDefault();
      event.stopPropagation();
      if (terminal.hasSelection()) {
        void copyToClipboard(terminal.getSelection()).then((ok) => {
          if (!ok) addToast('error', 'Copy failed: clipboard unavailable');
        });
      }
      return false;
    }
    // Paste: Cmd+V (macOS) / Ctrl+Shift+V. Read the clipboard and feed it
    // through term.paste so xterm honors bracketed-paste mode (multi-line
    // safety). readText can reject (permission/focus) — surface it, never
    // swallow.
    if (isClipboardChord(event, 'v', isMac)) {
      event.preventDefault();
      event.stopPropagation();
      navigator.clipboard
        .readText()
        .then((text) => {
          // The surface may have unmounted while the clipboard read was in
          // flight; never write into a disposed xterm.
          if (text && !isDisposed?.()) terminal.paste(text);
        })
        .catch((err) => {
          console.error('terminal: clipboard paste failed', err);
          addToast('error', `Paste failed: ${errString(err)}`);
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
  // unavailable (headless tests, or any webview that disables 3D APIs), and
  // onContextLoss reverts to the DOM renderer if the GPU drops the context at
  // runtime.
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
