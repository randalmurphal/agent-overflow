<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { SendMessage, InterruptTurn } from '../../stores/bindings';

  let { pane }: { pane: ThreadPane } = $props();

  let message = $state('');
  let textarea: HTMLTextAreaElement | undefined = $state(undefined);

  let isRunning = $derived(pane.sessionStatus === 'running');
  let isDisabled = $derived(!pane.threadId);

  async function send() {
    const text = message.trim();
    if (!text || !pane.threadId) return;

    // Optimistic: show the pending message immediately and clear the input.
    pane.setPendingMessage(text);
    const savedText = message;
    message = '';

    // Reset textarea height after clearing.
    if (textarea) {
      textarea.style.height = 'auto';
    }

    try {
      await SendMessage(pane.threadId, text);
    } catch (err) {
      console.error('Failed to send message:', err);
      // Restore input text and remove optimistic pending message on failure.
      message = savedText;
      pane.setPendingMessage(null);
      pane.setError(`Failed to send message: ${err}`);
    }
  }

  async function stop() {
    if (!pane.threadId) return;

    try {
      await InterruptTurn(pane.threadId);
    } catch (err) {
      console.error('Failed to interrupt turn:', err);
      pane.setError(`Failed to interrupt: ${err}`);
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  }

  function handleInput() {
    if (!textarea) return;
    textarea.style.height = 'auto';
    textarea.style.height = Math.min(textarea.scrollHeight, 200) + 'px';
  }
</script>

<div class="border-t border-border bg-surface-1 px-4 py-3">
  <div class="flex gap-2 items-end">
    <textarea
      bind:this={textarea}
      bind:value={message}
      onkeydown={handleKeydown}
      oninput={handleInput}
      disabled={isDisabled}
      placeholder={isDisabled ? 'Select or create a thread to start' : 'Send a message... (Shift+Enter for newline)'}
      aria-label="Message input"
      rows={1}
      class="flex-1 resize-none rounded-lg border border-border bg-surface-0 px-3 py-2.5 text-sm text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
    ></textarea>

    {#if isRunning}
      <button
        onclick={stop}
        class="shrink-0 rounded-lg px-4 py-2.5 text-sm font-medium bg-error/30 text-error hover:bg-error/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-error/50"
      >
        Stop
      </button>
    {:else}
      <button
        onclick={send}
        disabled={isDisabled || !message.trim()}
        class="shrink-0 rounded-lg px-4 py-2.5 text-sm font-medium bg-accent text-surface-0 hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Send
      </button>
    {/if}
  </div>
</div>
