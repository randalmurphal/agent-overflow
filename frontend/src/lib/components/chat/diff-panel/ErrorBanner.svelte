<script lang="ts">
  interface Props {
    message: string;
    onDismiss: () => void;
  }

  let { message, onDismiss }: Props = $props();
</script>

<div
  class="flex items-start gap-2 border-b border-error/40 bg-error/10 px-3 py-2 text-xs text-error"
  role="alert"
  data-testid="diff-panel-error"
>
  <span class="flex-1 break-words">{message}</span>
  <button
    type="button"
    class="shrink-0 rounded px-1 py-0.5 text-error/80 hover:bg-error/15 hover:text-error cursor-pointer"
    aria-label="Dismiss error"
    data-testid="diff-panel-error-dismiss"
    onclick={onDismiss}
  >
    ✕
  </button>
</div>
