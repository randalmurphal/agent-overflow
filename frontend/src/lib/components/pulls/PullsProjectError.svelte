<script lang="ts">
  import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ErrorReportActions from '../shared/ErrorReportActions.svelte';
  import ErrorReportDetails from '../shared/ErrorReportDetails.svelte';
  import { rateLimitMessage, type ForgeFailure } from '../../utils/forgeFailure';
  import { capturedCause } from './causeCapture';

  // A project whose list read failed shows the cause under its own group
  // header, so the rest of the page keeps working. A login to fix outside
  // the app (setup, forbidden) reads in the error tone with no Retry: no
  // retry changes it until the user does. A rate limit reads as paused.
  // Anything else retries on the next tick and offers Retry now. Details
  // expands the correlation id and the forge's raw error behind the
  // sentence, and copy takes them with the backend log.

  interface Props {
    message: string;
    failure: ForgeFailure | null;
    forge: string;
    /** The failure behind the message: a failed call's error, or the
     * list's per-project detail as one. */
    cause?: unknown;
    onRetry: () => void;
    retrying?: boolean;
  }

  let { message, failure, forge, cause = null, onRetry, retrying = false }: Props = $props();

  const kind = $derived(failure?.kind ?? 'forge');
  const paused = $derived(kind === 'rate_limited');
  const fixOutside = $derived(kind === 'setup' || kind === 'forbidden');
  const text = $derived(paused && failure ? rateLimitMessage(forge, failure) : message);
  const toneClass = $derived(
    paused || fixOutside
      ? (paused ? 'border-warning/30 bg-warning/8 text-warning' : 'border-error/30 bg-error/8 text-error')
      : 'border-border-subtle bg-surface-1/60 text-fg-muted',
  );
  const captured = $derived(capturedCause(cause));
  let expanded = $state(false);
</script>

<div
  class="mx-3 my-2 rounded-[var(--radius-field)] border px-2.5 py-2 text-[0.75rem] leading-snug {toneClass}"
  data-testid="pulls-project-error"
  data-error-kind={kind}
>
  <div class="flex items-start gap-2">
    <span class="mt-px shrink-0"><Icon icon={TriangleAlert} size={13} strokeWidth={1.75} /></span>
    <span class="min-w-0 flex-1 break-words">{text}</span>
    {#if !fixOutside && !paused}
      <Button variant="ghost" size="xs" class="shrink-0" onclick={onRetry} loading={retrying} testId="pulls-project-retry">Retry</Button>
    {/if}
    {#if captured}
      <span class="-my-0.5 flex shrink-0 items-center gap-0.5">
        <ErrorReportActions {captured} {expanded} onToggle={() => { expanded = !expanded; }} />
      </span>
    {/if}
  </div>
  {#if captured && expanded}
    <ErrorReportDetails {captured} />
  {/if}
</div>
