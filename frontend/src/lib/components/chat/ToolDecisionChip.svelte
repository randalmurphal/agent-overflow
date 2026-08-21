<script lang="ts">
  import type { Item } from '../../types/models';

  // `reason` is the deciding component's own words for an auto-denied tool
  // call (Claude `system/permission_denied`). The chip stays the visible
  // state; the reason is what a reader hovers for when the notice row that
  // carries it in full has scrolled away.
  let { decision, reason }: { decision?: Item['decision']; reason?: string } = $props();

  const label = $derived.by(() => {
    switch (decision) {
      case 'approved': return 'Approved';
      case 'declined': return 'Declined';
      case 'amended': return 'Amended';
      case 'lost': return 'Lost';
      default: return '';
    }
  });

  const classes = $derived.by(() => {
    switch (decision) {
      case 'approved': return 'border-success/30 bg-success/10 text-success';
      case 'declined': return 'border-error/30 bg-error/10 text-error';
      case 'amended': return 'border-warning/30 bg-warning/10 text-warning';
      case 'lost': return 'border-text-secondary/30 bg-surface-0 text-text-secondary';
      default: return '';
    }
  });
</script>

{#if label}
  <span
    class="rounded-full border px-1.5 py-0.5 text-[0.6875rem] font-medium {classes}"
    data-testid="tool-decision-chip"
    data-decision={decision}
    title={reason || undefined}
  >
    {label}
  </span>
{/if}
