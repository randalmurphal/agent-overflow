<script lang="ts">
  interface Props {
    onSend: (content: string) => void;
  }

  let { onSend }: Props = $props();

  let input = $state("");

  function handleSubmit() {
    const trimmed = input.trim();
    if (!trimmed) return;
    onSend(trimmed);
    input = "";
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      handleSubmit();
    }
  }
</script>

<div class="border-t border-border px-4 py-3">
  <div class="max-w-2xl mx-auto flex gap-2">
    <textarea
      bind:value={input}
      onkeydown={handleKeydown}
      placeholder="Send a message..."
      rows={1}
      class="flex-1 resize-none bg-surface-1 border border-border rounded-lg px-3 py-2
             text-text-primary placeholder:text-text-secondary
             focus:outline-none focus:border-accent"
    ></textarea>
    <button
      onclick={handleSubmit}
      disabled={!input.trim()}
      class="px-4 py-2 bg-accent text-white rounded-lg
             disabled:opacity-40 hover:opacity-90 transition-opacity"
    >
      Send
    </button>
  </div>
</div>
