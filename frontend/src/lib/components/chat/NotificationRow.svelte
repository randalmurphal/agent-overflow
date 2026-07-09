<script lang="ts">
  import Bell from 'lucide-svelte/icons/bell';
  import AlertTriangle from 'lucide-svelte/icons/alert-triangle';
  import ClipboardList from 'lucide-svelte/icons/clipboard-list';
  import SearchCheck from 'lucide-svelte/icons/search-check';
  import Icon from '../primitives/Icon.svelte';
  import type { Item } from '../../types/models';
  import { parseJsonObject } from '../../utils/parseJsonObject';

  let { item }: { item: Item } = $props();

  let meta = $derived(parseJsonObject(item.meta));
  let kind = $derived(typeof meta?.kind === 'string' ? meta.kind : item.toolName || '');
  let icon = $derived.by(() => {
    if (
      kind === 'warning' ||
      kind === 'deprecation_notice' ||
      kind === 'model_verification' ||
      kind === 'model_refusal_fallback'
    ) {
      return AlertTriangle;
    }
    if (kind === 'plan_update') return ClipboardList;
    if (kind === 'review_status') return SearchCheck;
    return Bell;
  });
  let isWarning = $derived(
    kind === 'warning' ||
    kind === 'deprecation_notice' ||
    kind === 'model_verification' ||
    kind === 'model_refusal_fallback',
  );
  let fallbackReason = $derived.by(() => {
    if (kind !== 'model_refusal_fallback') return '';
    const explanation = typeof meta?.explanation === 'string' ? meta.explanation.trim() : '';
    if (explanation) return explanation;
    const category = typeof meta?.category === 'string' ? meta.category.trim().toLowerCase() : '';
    if (category === 'cyber') return 'Cybersecurity safety classifier';
    if (category === 'bio') return 'Biology safety classifier';
    return category ? `${category} safety classifier` : '';
  });

  let plan = $derived.by<Array<{ step: string; status: string }>>(() => {
    if (kind !== 'plan_update') return [];
    const raw = meta?.plan;
    if (!Array.isArray(raw)) return [];
    return raw.flatMap((entry) => {
      if (!entry || typeof entry !== 'object') return [];
      const record = entry as Record<string, unknown>;
      const step = typeof record.step === 'string' ? record.step : '';
      const status = typeof record.status === 'string' ? record.status : '';
      return step ? [{ step, status }] : [];
    });
  });

  let hookEntries = $derived.by<Array<string>>(() => {
    if (kind !== 'hook') return [];
    const run = meta?.run;
    if (!run || typeof run !== 'object' || Array.isArray(run)) return [];
    const entries = (run as Record<string, unknown>).entries;
    if (!Array.isArray(entries)) return [];
    return entries.slice(0, maxHookEntries).flatMap((entry) => {
      if (!entry || typeof entry !== 'object' || Array.isArray(entry)) return [];
      const record = entry as Record<string, unknown>;
      const text = typeof record.text === 'string' ? record.text.trim() : '';
      if (!text) return [];
      const entryKind = typeof record.kind === 'string' ? record.kind.trim() : '';
      const displayText = truncateText(text, maxHookEntryChars);
      return [entryKind ? `${entryKind}: ${displayText}` : displayText];
    });
  });

  let hiddenHookEntryCount = $derived.by(() => {
    if (kind !== 'hook') return 0;
    const run = meta?.run;
    if (!run || typeof run !== 'object' || Array.isArray(run)) return 0;
    const entries = (run as Record<string, unknown>).entries;
    return Array.isArray(entries) && entries.length > maxHookEntries
      ? entries.length - maxHookEntries
      : 0;
  });

  function statusGlyph(status: string): string {
    if (status === 'completed') return 'x';
    if (status === 'inProgress') return '>';
    return ' ';
  }

  const maxHookEntries = 8;
  const maxHookEntryChars = 300;

  function truncateText(value: string, maxChars: number): string {
    return value.length > maxChars ? `${value.slice(0, maxChars)}...` : value;
  }
</script>

<div
  class="mb-1.5 px-2 py-1 text-[0.6875rem] italic {isWarning ? 'text-warning' : 'text-fg-subtle'}"
  data-testid="notification-row"
  role={isWarning ? 'status' : undefined}
>
  <div class="flex items-center gap-1.5">
    <Icon {icon} size={11} strokeWidth={2} class="opacity-70 shrink-0" />
    <span>{item.summary || 'Provider notification'}</span>
  </div>
  {#if fallbackReason}
    <div class="ml-5 mt-0.5 not-italic text-warning/80">
      Reason: {fallbackReason}
    </div>
  {/if}
  {#if plan.length > 0}
    <div class="ml-5 mt-1 space-y-0.5 not-italic">
      {#each plan as step}
        <div class="truncate font-mono text-[0.6875rem] text-fg-subtle">
          [{statusGlyph(step.status)}] {step.step}
        </div>
      {/each}
    </div>
  {/if}
  {#if hookEntries.length > 0}
    <div class="ml-5 mt-1 space-y-0.5 not-italic">
      {#each hookEntries as entry}
        <div class="truncate font-mono text-[0.6875rem] text-fg-subtle">
          {entry}
        </div>
      {/each}
      {#if hiddenHookEntryCount > 0}
        <div class="truncate font-mono text-[0.6875rem] text-fg-hint">
          +{hiddenHookEntryCount} more
        </div>
      {/if}
    </div>
  {/if}
</div>
