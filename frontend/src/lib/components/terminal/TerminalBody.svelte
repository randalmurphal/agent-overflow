<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { Terminal } from '@xterm/xterm';
  import { FitAddon } from '@xterm/addon-fit';
  import { WebLinksAddon } from '@xterm/addon-web-links';
  import '@xterm/xterm/css/xterm.css';
  import {
    WriteTerminal,
    ResizeTerminal,
    GetTerminalReplay,
  } from '../../stores/bindings';
  import { decodeTerminalOutput, encodeTerminalInput } from '../../types/terminal';
  import {
    notifyTerminalFocus,
    type ThreadTerminalStateHandle,
  } from './terminalStore.svelte';

  interface SendToComposerChip {
    id: string;
    label: string;
    preview: string;
    content: string;
    createdAt: number;
  }

  interface Props {
    handle: ThreadTerminalStateHandle;
    terminalID: string;
    onSendToComposer?: (chip: SendToComposerChip) => void;
  }

  let { handle, terminalID, onSendToComposer }: Props = $props();

  let selection = $state<string>('');

  let mountEl: HTMLDivElement | undefined = $state();
  let term: Terminal | null = null;
  let fit: FitAddon | null = null;
  let resizeObserver: ResizeObserver | null = null;
  let dataDisposable: { dispose(): void } | null = null;
  let destroyed = false;
  // Track whether we've already told the focus registry we're focused so
  // we don't double-decrement on teardown.
  let focusCounted = false;

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
      lineHeight: 1.2,
      scrollback: 4000,
      allowProposedApi: true,
    });

    fit = new FitAddon();
    term.loadAddon(fit);
    term.loadAddon(new WebLinksAddon());
    term.open(mountEl);

    // Wire focus/blur listeners on the xterm mount. xterm puts a focusable
    // textarea inside mountEl, so focusin/focusout bubble up reliably.
    mountEl.addEventListener('focusin', handleFocusIn);
    mountEl.addEventListener('focusout', handleFocusOut);

    // Load replay buffer before draining pending output so order is correct.
    try {
      const replayB64 = await GetTerminalReplay(terminalID);
      if (!destroyed && replayB64) {
        term.write(decodeTerminalOutput(replayB64));
      }
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

    term.onSelectionChange(() => {
      selection = term?.getSelection() ?? '';
    });

    attachResizeObserver();
    scheduleFit();
  }

  function handleSendSelection() {
    const text = selection.trim();
    if (!text || !onSendToComposer) return;
    const preview = text.split('\n')[0]?.slice(0, 60) ?? text.slice(0, 60);
    onSendToComposer({
      id: `chip-${Date.now()}-${Math.floor(Math.random() * 10000)}`,
      label: `terminal ${terminalID.slice(0, 8)}`,
      preview,
      content: text,
      createdAt: Date.now(),
    });
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
    term?.focus();
  }
</script>

<div class="flex-1 min-h-0 flex flex-col bg-[#111]" data-testid={`terminal-body-${terminalID}`}>
  {#if onSendToComposer}
    <div class="flex items-center justify-end border-b border-border/30 bg-black/30 px-2 py-1 text-xs">
      <button
        type="button"
        disabled={!selection.trim()}
        onclick={handleSendSelection}
        aria-label="Send Selection to Composer"
        data-testid="terminal-send-to-composer"
        class="rounded px-2 py-0.5 font-medium text-text-primary hover:bg-accent/30 disabled:opacity-40 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Send Selection to Composer
      </button>
    </div>
  {/if}
  <div bind:this={mountEl} class="flex-1 min-h-0"></div>
</div>
