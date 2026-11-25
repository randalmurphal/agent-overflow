<script lang="ts">
  // What became of a control's last write, under the control: "Saving…"
  // while it is out, then the tracker's refusal with each field's message,
  // a part whose answer was lost (and where to look), the call's error as
  // it is, or what the ticket does not show yet of a write that was made.
  // A refusal for what a rich text edit would lose offers Save anyway,
  // which sends the same write accepting the loss. Nothing shows once a
  // plain success settled.
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import { dismissTicketWrite, ticketWrite } from '../../stores/trackerTicketWrites.svelte';
  import { refusalSentence } from '../../stores/trackerTicketModel';
  import { errString } from '../../utils/errors';

  interface Props {
    ticketKey: string;
    target: string;
    /** A field's name as the ticket shows it, for a refusal naming fields. */
    fieldName: (id: string) => string;
  }

  let { ticketKey, target, fieldName }: Props = $props();

  const write = $derived(ticketWrite(ticketKey, target));
  const outcome = $derived(write?.outcome ?? null);
  const text = $derived.by(() => {
    if (!outcome) return '';
    switch (outcome.kind) {
      case 'applied':
        return outcome.note ?? '';
      case 'refused':
        return refusalSentence(outcome.refusal, fieldName);
      case 'unknown':
      case 'not_sent':
      case 'lost':
        return outcome.message;
      case 'failed':
        return errString(outcome.error);
    }
  });
  const tone = $derived(outcome?.kind === 'applied' ? 'text-fg-muted' : 'text-error');
</script>

{#if write?.pending}
  <p class="text-xs text-fg-muted" role="status" data-testid="ticket-write-pending" data-target={target}>Saving…</p>
{:else if outcome && text}
  <div class="flex items-start gap-1.5 text-xs {tone}" role={outcome.kind === 'applied' ? 'status' : 'alert'} data-testid="ticket-write-outcome" data-target={target} data-kind={outcome.kind}>
    <span class="min-w-0 flex-1 break-words">{text}</span>
    {#if write?.saveAnyway}
      {@const again = write.saveAnyway}
      <button type="button" class="shrink-0 text-accent hover:underline" data-testid="ticket-write-save-anyway" onclick={() => { void again(); }}>Save anyway</button>
    {/if}
    <IconButton label="Dismiss" size="xs" variant="tint" testId="ticket-write-dismiss" onClick={() => dismissTicketWrite(ticketKey, target)}>
      {#snippet children()}<Icon icon={X} size={11} strokeWidth={2.25} />{/snippet}
    </IconButton>
  </div>
{/if}
