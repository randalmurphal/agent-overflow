<script lang="ts">
  /*
   * The compact terminal's modifier surface: one scrollable row of the keys a
   * phone keyboard has no way to produce, docked at the bottom of the terminal
   * so it sits directly above the on-screen keyboard.
   *
   * Every button hands its bytes back to the host (TerminalBody), which feeds
   * them to `term.input(...)` — the same path a typed key takes. That keeps a
   * single writer on the PTY and lets sticky Ctrl live on the input path, where
   * it also catches letters typed on the soft keyboard.
   *
   * A press must never take focus off the terminal, or the soft keyboard
   * dismisses on every tap. `tabindex="-1"` keeps the buttons out of the tab
   * order and the pointerdown preventDefault stops the browser's
   * focus-on-mousedown before it happens.
   *
   * While the terminal's find bar holds focus the host sends nothing to the
   * PTY: Esc closes the bar, the character keys and Paste type into the
   * query, and the keys with no meaning there (Tab, Ctrl, the arrows) are
   * disabled.
   */
  interface Props {
    /** Deliver these bytes as terminal input. */
    onKey: (data: string) => void;
    /** Whether sticky Ctrl is currently armed. */
    ctrlArmed: boolean;
    /** Arm / disarm sticky Ctrl. */
    onToggleCtrl: () => void;
    /** Paste the clipboard through the terminal's own paste route. The
     *  right-click paste is off under compact (a long press must not
     *  paste), so this key is the layout's only paste. */
    onPaste: () => void;
    /** The terminal's find bar holds focus. */
    findFocused?: boolean;
  }

  let { onKey, ctrlArmed, onToggleCtrl, onPaste, findFocused = false }: Props = $props();

  interface KeyDef {
    id: string;
    label: string;
    name: string;
    data: string;
    /** Meaningful in a find query: Esc closes it, a character types. */
    findKey?: true;
  }

  // Module-constant in spirit: the row never varies, so the array is built once
  // per instance and never re-derived.
  const KEYS: readonly KeyDef[] = [
    { id: 'esc', label: 'Esc', name: 'Escape', data: '\x1b', findKey: true },
    { id: 'tab', label: 'Tab', name: 'Tab', data: '\t' },
    { id: 'up', label: '↑', name: 'Up arrow', data: '\x1b[A' },
    { id: 'down', label: '↓', name: 'Down arrow', data: '\x1b[B' },
    { id: 'left', label: '←', name: 'Left arrow', data: '\x1b[D' },
    { id: 'right', label: '→', name: 'Right arrow', data: '\x1b[C' },
    { id: 'dash', label: '-', name: 'Hyphen', data: '-', findKey: true },
    { id: 'slash', label: '/', name: 'Slash', data: '/', findKey: true },
    { id: 'pipe', label: '|', name: 'Pipe', data: '|', findKey: true },
    { id: 'tilde', label: '~', name: 'Tilde', data: '~', findKey: true },
  ];

  // Ctrl is rendered between Tab and the arrows, so the row splits here rather
  // than carrying a sentinel entry through the data table.
  const BEFORE_CTRL = 2;

  const BUTTON_CLASS =
    'h-10 min-w-10 px-3 shrink-0 rounded border font-mono text-xs leading-none ' +
    'select-none aria-disabled:opacity-40';

  function press(key: KeyDef): void {
    if (findFocused && !key.findKey) return;
    onKey(key.data);
  }

  function toggleCtrl(): void {
    if (!findFocused) onToggleCtrl();
  }

  // Suppress the browser's focus-on-pointerdown so the xterm textarea (or the
  // find input) keeps focus, and the soft keyboard stays up, across a tap.
  // The keys are aria-disabled, not disabled, so a tap on one still lands here.
  function keepTerminalFocus(event: PointerEvent): void {
    event.preventDefault();
  }
</script>

<div
  class="flex items-center gap-1 px-1 py-1 pb-[max(0.25rem,env(safe-area-inset-bottom))] shrink-0 overflow-x-auto bg-surface-1
         border-t border-border"
  style="touch-action: pan-x; overscroll-behavior-x: contain;"
  data-testid="terminal-key-row"
>
  {#each KEYS.slice(0, BEFORE_CTRL) as key (key.id)}
    <button
      type="button"
      tabindex="-1"
      class="{BUTTON_CLASS} bg-surface-2 border-border text-text-secondary"
      data-testid={`terminal-key-${key.id}`}
      aria-label={key.name}
      aria-disabled={findFocused && !key.findKey}
      onpointerdown={keepTerminalFocus}
      onclick={() => press(key)}>{key.label}</button
    >
  {/each}

  <!-- Sticky modifier, not a key: it emits nothing itself. The host converts
       the next character on the input path (see terminalKeys.ts). -->
  <button
    type="button"
    tabindex="-1"
    class="{BUTTON_CLASS} {ctrlArmed
      ? 'bg-accent/25 border-accent text-text-primary'
      : 'bg-surface-2 border-border text-text-secondary'}"
    data-testid="terminal-key-ctrl"
    aria-label="Control modifier"
    aria-pressed={ctrlArmed}
    aria-disabled={findFocused}
    onpointerdown={keepTerminalFocus}
    onclick={toggleCtrl}>Ctrl</button
  >

  {#each KEYS.slice(BEFORE_CTRL) as key (key.id)}
    <button
      type="button"
      tabindex="-1"
      class="{BUTTON_CLASS} bg-surface-2 border-border text-text-secondary"
      data-testid={`terminal-key-${key.id}`}
      aria-label={key.name}
      aria-disabled={findFocused && !key.findKey}
      onpointerdown={keepTerminalFocus}
      onclick={() => press(key)}>{key.label}</button
    >
  {/each}

  <!-- Not a key: it reads the clipboard rather than emitting bytes. -->
  <button
    type="button"
    tabindex="-1"
    class="{BUTTON_CLASS} bg-surface-2 border-border text-text-secondary"
    data-testid="terminal-key-paste"
    aria-label="Paste"
    onpointerdown={keepTerminalFocus}
    onclick={onPaste}>Paste</button
  >
</div>
