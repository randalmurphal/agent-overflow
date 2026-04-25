<script lang="ts">
  /*
   * Small status pill for terminal tool-call rows. One shape, two
   * variants — green check for `success`, red alert for `failure` —
   * sits in the same slot wherever the chat needs to communicate "this
   * tool is done." Replaces the old per-component mix of status text
   * labels and `exit N` pills.
   *
   * The pill uses the repo's `success` / `error` theme tokens (oklch
   * defined in app.css) rather than literal emerald/rose so light and
   * dark themes stay consistent.
   */
  import Check from 'lucide-svelte/icons/check';
  import AlertCircle from 'lucide-svelte/icons/alert-circle';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    status: 'success' | 'failure';
    /** Optional native tooltip text. Also used as the aria-label so
     * screen readers announce the badge meaning. */
    title?: string;
    class?: string;
  }

  let { status, title, class: className = '' }: Props = $props();

  const isSuccess = $derived(status === 'success');
  const palette = $derived(isSuccess ? 'bg-success/10 text-success' : 'bg-error/10 text-error');
  const iconComponent = $derived(isSuccess ? Check : AlertCircle);
  const ariaLabel = $derived(title ?? (isSuccess ? 'Completed successfully' : 'Failed'));
</script>

<span
  class="inline-flex shrink-0 items-center rounded px-1 py-px text-[11px] {palette} {className}"
  data-testid="completion-badge"
  data-status={status}
  title={title ?? ariaLabel}
  aria-label={ariaLabel}
>
  <Icon icon={iconComponent} size={10} strokeWidth={2.5} class="opacity-100" />
</span>
