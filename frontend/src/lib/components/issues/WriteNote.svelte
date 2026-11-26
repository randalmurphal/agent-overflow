<script lang="ts">
  // What became of a control's last tracker write, under the control: the
  // pending word while it is out, then the outcome's sentence, with Save
  // anyway for a refusal over what a rich text edit would lose, and a
  // dismiss. Nothing shows once a plain success settled. The write store
  // that sent it words the outcome (`text`).
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import type { WriteOutcome } from '../../stores/trackerWriteOutcome';

  interface Props {
    write: {
      readonly pending: boolean;
      readonly outcome: WriteOutcome | null;
      readonly saveAnyway: (() => Promise<unknown>) | null;
    } | null;
    /** What shows while the write is out: "Saving…". */
    pendingText: string;
    /** The settled outcome's sentence; empty shows nothing. */
    text: string;
    onDismiss: () => void;
    testId: string;
    /** The control's target, as `data-target`. */
    target?: string;
  }

  let { write, pendingText, text, onDismiss, testId, target }: Props = $props();

  const outcome = $derived(write?.outcome ?? null);
  // A create made with a later step not applied is said as a failure.
  const plain = $derived(outcome?.kind === 'applied' && !outcome.unplaced);
  const tone = $derived(plain ? 'text-fg-muted' : 'text-error');
</script>

{#if write?.pending}
  <p class="text-xs text-fg-muted" role="status" data-testid="{testId}-pending" data-target={target}>{pendingText}</p>
{:else if outcome && text}
  <div class="flex items-start gap-1.5 text-xs {tone}" role={plain ? 'status' : 'alert'} data-testid="{testId}-outcome" data-target={target} data-kind={outcome.kind}>
    <span class="min-w-0 flex-1 break-words">{text}</span>
    {#if write?.saveAnyway}
      {@const again = write.saveAnyway}
      <button type="button" class="shrink-0 text-accent hover:underline" data-testid="{testId}-save-anyway" onclick={() => { void again(); }}>Save anyway</button>
    {/if}
    <IconButton label="Dismiss" size="xs" variant="tint" testId="{testId}-dismiss" onClick={onDismiss}>
      {#snippet children()}<Icon icon={X} size={11} strokeWidth={2.25} />{/snippet}
    </IconButton>
  </div>
{/if}
