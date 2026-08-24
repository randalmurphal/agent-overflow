<script lang="ts">
  import Bell from '@lucide/svelte/icons/bell';
  import AlertTriangle from '@lucide/svelte/icons/alert-triangle';
  import ClipboardList from '@lucide/svelte/icons/clipboard-list';
  import SearchCheck from '@lucide/svelte/icons/search-check';
  import Inbox from '@lucide/svelte/icons/inbox';
  import ShieldBan from '@lucide/svelte/icons/shield-ban';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
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
      kind === 'model_refusal_fallback' ||
      kind === 'transcript_mirror_degraded' ||
      // Messages parked in Codex's own queue that the running Codex has no
      // API for. Same family as a deprecation notice: nothing is lost, but
      // it stays true until the user upgrades.
      kind === 'codex_queue_unsupported' ||
      // Codex HAS the queue API but could not be asked, so a queued message
      // can be neither confirmed nor returned to the composer until a later
      // session start reads the queue.
      kind === 'codex_queue_unreconciled'
    ) {
      return AlertTriangle;
    }
    if (kind === 'plan_update') return ClipboardList;
    if (kind === 'review_status') return SearchCheck;
    // A message reached this thread's queue from outside Agent Overflow
    // (`codex queue --thread ...`). Not a warning — nothing is wrong — but it
    // is the one notification that reports someone ELSE acting on this thread,
    // so it gets its own glyph rather than the generic bell.
    if (kind === 'external_queue') return Inbox;
    // A tool call the provider refused before it could ask. Its own glyph
    // because it is the one notification that reports something the model
    // TRIED to do and could not.
    if (kind === 'permission_denied') return ShieldBan;
    if (kind === 'permission_retry') return RotateCcw;
    return Bell;
  });
  let isWarning = $derived(
    kind === 'warning' ||
    kind === 'deprecation_notice' ||
    kind === 'model_verification' ||
    kind === 'model_refusal_fallback' ||
    kind === 'transcript_mirror_degraded' ||
    // The messages run only after a Codex upgrade, so the row is a standing
    // request for action, not chatter.
    kind === 'codex_queue_unsupported' ||
    // Unresolved until the thread is reopened: the user needs to know a
    // message they sent is in limbo, not read it as a passing note.
    kind === 'codex_queue_unreconciled' ||
    // A denial is user-facing state, not chatter: the tool did not run and
    // nothing else on the timeline says why.
    kind === 'permission_denied',
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

  // The deciding component's own words ("Denied by alwaysDenyRules:
  // Bash(rm:*)", "Path is outside allowed working directories"). Absent when
  // the CLI sent neither decision_reason nor message.
  let denialReason = $derived.by(() => {
    if (kind !== 'permission_denied') return '';
    return typeof meta?.decisionReason === 'string' ? meta.decisionReason.trim() : '';
  });

  // A workspace-boundary refusal has a different remedy from a rule refusal,
  // and the wrong one fixes nothing: the CLI answers a boundary denial with
  // addDirectories suggestions, never a tool rule.
  let isWorkspaceBoundaryDenial = $derived(
    kind === 'permission_denied' && meta?.workspaceBoundary === true,
  );

  // permission_retry's display names of the commands a permission-mode change
  // just allowed. Per command NAME — the subtype carries no tool id.
  let retriedCommands = $derived.by<Array<string>>(() => {
    if (kind !== 'permission_retry') return [];
    const raw = meta?.commands;
    if (!Array.isArray(raw)) return [];
    return raw.flatMap((entry) =>
      typeof entry === 'string' && entry.trim() ? [entry.trim()] : [],
    );
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
  {#if denialReason}
    <div class="ml-5 mt-0.5 not-italic text-warning/80" data-testid="permission-denied-reason">
      Reason: {denialReason}
    </div>
  {/if}
  {#if isWorkspaceBoundaryDenial}
    <div class="ml-5 mt-0.5 not-italic text-warning/80" data-testid="permission-denied-remedy">
      Allowing this needs the directory added to the session, not a tool permission rule.
    </div>
  {/if}
  {#if retriedCommands.length > 0}
    <div class="ml-5 mt-1 space-y-0.5 not-italic" data-testid="permission-retry-commands">
      {#each retriedCommands as command}
        <div class="truncate font-mono text-[0.6875rem] text-fg-subtle">
          {command}
        </div>
      {/each}
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
