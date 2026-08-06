<script lang="ts">
  /*
   * Retry-exhausted assistant API error row. Rendered for items whose
   * `kind === 'api_error'` — these come from `EventError` envelopes
   * tagged with the SDK's `assistant.error` enum (rate_limit,
   * authentication_failed, billing_error, invalid_request,
   * server_error, max_output_tokens, unknown). The row branches its
   * actionable copy on the enum so a rate-limit hit can deep-link to
   * billing while an auth failure can prompt `/login`.
   *
   * No "Retry this turn" button — Claude Code's TUI doesn't have one
   * either. The user re-prompts. The error copy itself is the call to
   * action.
   *
   * Reference: claude-code-source-code/src/components/messages/
   *   AssistantTextMessage.tsx:60-227.
   */
  import AlertCircle from '@lucide/svelte/icons/alert-circle';
  import Icon from '../primitives/Icon.svelte';
  import type { Item } from '../../types/models';
  import { parseJsonObject } from '../../utils/parseJsonObject';

  let { item }: { item: Item } = $props();

  const meta = $derived(parseJsonObject(item.meta));
  const errorEnum = $derived(typeof meta?.error === 'string' ? meta.error : '');

  type ActionLink = { href: string; label: string };
  type ErrorPresentation = { action?: ActionLink; hint?: string };

  // Per-enum presentation table. Keep `action` and `hint` colocated so
  // adding a new SDK error enum is a single-row edit, not two parallel
  // switch arms.
  const BILLING_ACTION: ActionLink = {
    href: 'https://console.anthropic.com/settings/billing',
    label: 'Add credits at console.anthropic.com',
  };
  const ERROR_PRESENTATIONS: Record<string, ErrorPresentation> = {
    rate_limit: { action: BILLING_ACTION },
    billing_error: { action: BILLING_ACTION },
    authentication_failed: { hint: 'Run /login to reauthenticate.' },
    server_error: { hint: 'Anthropic API server error — try again in a moment.' },
    max_output_tokens: {
      hint: 'The model hit its max-output-tokens cap. Re-prompt or split the request.',
    },
  };

  const presentation = $derived<ErrorPresentation>(ERROR_PRESENTATIONS[errorEnum] ?? {});
</script>

<div
  class="mb-3 rounded-[var(--radius-control)] border border-error/30 bg-error/10 px-3 py-2 text-sm text-error"
  data-testid="api-error-row"
  data-error-enum={errorEnum}
  role="alert"
>
  <div class="flex items-start gap-2">
    <Icon icon={AlertCircle} size={14} strokeWidth={2} class="mt-0.5 shrink-0 opacity-90" />
    <div class="flex-1 space-y-1">
      <div>{item.summary || 'API error'}</div>
      {#if presentation.hint}
        <div class="text-[0.75rem] text-error/80">{presentation.hint}</div>
      {/if}
      {#if presentation.action}
        <div>
          <a
            href={presentation.action.href}
            class="text-[0.75rem] underline hover:no-underline"
            target="_blank"
            rel="noopener noreferrer"
          >
            {presentation.action.label}
          </a>
        </div>
      {/if}
    </div>
  </div>
</div>
