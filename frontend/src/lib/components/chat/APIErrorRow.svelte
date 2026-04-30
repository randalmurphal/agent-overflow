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
  import AlertCircle from 'lucide-svelte/icons/alert-circle';
  import Icon from '../primitives/Icon.svelte';
  import type { Item } from '../../types/models';
  import { parseJsonObject } from '../../utils/parseJsonObject';

  let { item }: { item: Item } = $props();

  const meta = $derived(parseJsonObject(item.meta));
  const errorEnum = $derived(typeof meta?.error === 'string' ? meta.error : '');

  type ActionLink = { href: string; label: string };

  const action = $derived.by<ActionLink | null>(() => {
    switch (errorEnum) {
      case 'rate_limit':
      case 'billing_error':
        return {
          href: 'https://console.anthropic.com/settings/billing',
          label: 'Add credits at console.anthropic.com',
        };
      default:
        return null;
    }
  });

  const hint = $derived.by<string>(() => {
    switch (errorEnum) {
      case 'authentication_failed':
        return 'Run /login to reauthenticate.';
      case 'server_error':
        return 'Anthropic API server error — try again in a moment.';
      case 'max_output_tokens':
        return 'The model hit its max-output-tokens cap. Re-prompt or split the request.';
      default:
        return '';
    }
  });
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
      {#if hint}
        <div class="text-[12px] text-error/80">{hint}</div>
      {/if}
      {#if action}
        <div>
          <a
            href={action.href}
            class="text-[12px] underline hover:no-underline"
            target="_blank"
            rel="noopener noreferrer"
          >
            {action.label}
          </a>
        </div>
      {/if}
    </div>
  </div>
</div>
