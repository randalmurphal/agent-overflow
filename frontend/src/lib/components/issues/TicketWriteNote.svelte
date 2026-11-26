<script lang="ts">
  // What became of a ticket control's last write, under the control
  // (WriteNote): "Saving…" while it is out, then the tracker's refusal
  // with each field's message, a part whose answer was lost (and where to
  // look), the call's error as it is, or what the ticket does not show yet
  // of a write that was made. A refusal for what a rich text edit would
  // lose offers Save anyway, which sends the same write accepting the
  // loss; once it is made, the write store drops the editor's draft, which
  // closes it. Nothing shows once a plain success settled. While mounted
  // it is a control that draws its target's outcome
  // (`trackerTicketWrites.svelte.ts`, where an outcome shows).
  import WriteNote from './WriteNote.svelte';
  import { ticketWriteSurface } from './ticketWriteSurface';
  import { dismissTicketWrite, drawTicketWrite, ticketWrite } from '../../stores/trackerTicketWrites.svelte';
  import { refusalSentence } from '../../stores/trackerWriteOutcome';
  import { errString } from '../../utils/errors';

  interface Props {
    ticketKey: string;
    target: string;
    /** A field's name as the ticket shows it, for a refusal naming fields. */
    fieldName: (id: string) => string;
  }

  let { ticketKey, target, fieldName }: Props = $props();

  const shown = ticketWriteSurface();
  $effect(() => drawTicketWrite(ticketKey, target, shown));

  const write = $derived(ticketWrite(ticketKey, target));
  const text = $derived.by(() => {
    const outcome = write?.outcome;
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
</script>

<WriteNote {write} pendingText="Saving…" {text} {target} testId="ticket-write" onDismiss={() => dismissTicketWrite(ticketKey, target)} />
