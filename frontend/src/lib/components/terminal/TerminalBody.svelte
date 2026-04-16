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
  import type { ThreadTerminalStateHandle } from './terminalStore.svelte';

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

    attachResizeObserver();
    scheduleFit();
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

<div
  bind:this={mountEl}
  class="flex-1 min-h-0 bg-[#111]"
  data-testid={`terminal-body-${terminalID}`}
></div>
