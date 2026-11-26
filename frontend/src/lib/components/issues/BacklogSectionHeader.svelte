<script lang="ts">
  // A Backlog section's header (docs/specs/issues.md, "Backlog and sprint
  // planning"): the fold, the name, the sprint's dates, and its numbers
  // (`trackerBacklogModel.ts#sectionNumbers`). An active sprint says how
  // much of it is done; a future sprint says what it holds against the
  // board's pace, each person's load and how far it is over, with a bar; the
  // backlog says its tickets, points and tickets with no estimate. While a
  // row is carried over it or out of it, or waits there, the numbers say
  // what they become after an arrow. `controls` draws the sprint's actions.
  import type { Snippet } from 'svelte';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import ReviewAvatar from '../review/ReviewAvatar.svelte';
  import type { BacklogSection, SectionNumbers } from '../../stores/trackerBacklogModel';
  import { measureOf } from '../../stores/trackerBacklogModel';
  import { formatEstimateSum, sectionLine } from './boardFormat';

  interface Props {
    sec: BacklogSection;
    numbers: SectionNumbers;
    unit: string;
    open: boolean;
    /** Now, to the minute. */
    now: number;
    /** A carried row is over this section, from another. */
    target: boolean;
    onFold: (open: boolean) => void;
    controls?: Snippet;
  }

  let { sec, numbers, unit, open, now, target, onFold, controls }: Props = $props();

  const DAY_MS = 86_400_000;
  const estimated = $derived(unit === 'points' || unit === 'time');
  const name = $derived(sec.sprint?.name || (sec.kind === 'backlog' ? 'Backlog' : 'Sprint'));

  function day(ms: number): string {
    return new Date(ms).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  }

  const note = $derived.by(() => {
    const sp = sec.sprint;
    if (!sp) return '';
    if (sec.kind === 'active') {
      if (!sp.end) return 'Active';
      const left = Math.ceil((sp.end - now) / DAY_MS);
      return left > 0 ? `Active · ${left} ${left === 1 ? 'day' : 'days'} left` : 'Active · past its end';
    }
    return sp.start && sp.end ? `${day(sp.start)} to ${day(sp.end)}` : 'Not started';
  });

  /** A measure as the board words it: points or time, else tickets. */
  function amount(n: number): string {
    if (!estimated) return `${n} ${n === 1 ? 'ticket' : 'tickets'}`;
    const v = formatEstimateSum(n, unit) ?? '0';
    return unit === 'points' ? `${v} ${n === 1 ? 'point' : 'points'}` : v;
  }
  function bare(n: number): string {
    return estimated ? (formatEstimateSum(n, unit) ?? '0') : String(n);
  }

  const line = $derived(sectionLine(sec.kind, numbers, unit));
  const pace = $derived(numbers.pace ? measureOf(numbers.pace, unit) : null);
  const fill = $derived(pace ? Math.min(100, (measureOf(numbers.total, unit) / pace) * 100) : 0);
</script>

<header class="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 px-3 pb-1.5 pt-3 text-sm {target ? 'text-accent' : ''}" data-testid="issues-backlog-section" data-list={sec.list} data-kind={sec.kind} data-open={open ? '' : undefined}>
  <button
    type="button"
    class="inline-flex min-w-0 shrink-0 items-center gap-1.5 rounded-[var(--radius-control)] px-1 font-medium text-fg hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:select-none"
    aria-expanded={open}
    data-testid="issues-backlog-fold"
    onclick={() => onFold(!open)}
  >
    <Icon icon={ChevronDown} size={14} strokeWidth={2} class={open ? '' : '-rotate-90'} />
    <span class="truncate">{name}</span>
  </button>
  {#if note}<span class="min-w-0 truncate text-[0.75rem] text-fg-muted" data-testid="issues-backlog-note">{note}</span>{/if}
  <span class="flex-1"></span>
  {#if numbers.load && numbers.load.length > 0}
    <span class="flex flex-wrap gap-1" data-testid="issues-backlog-load">
      {#each numbers.load as load (load.person?.id ?? '')}
        {@const who = load.person ? load.person.name || 'Unknown user' : 'Unassigned'}
        <span class="inline-flex h-6 items-center gap-1 rounded-full border border-border-subtle bg-surface-1 pl-0.5 pr-2 text-[0.75rem] tabular-nums text-fg-muted" title="{who}: {amount(measureOf(load, unit))} in this sprint" data-testid="issues-backlog-load-person">
          {#if load.person}
            <ReviewAvatar login={load.person.id} name={load.person.name} title={who} size={18} />
          {:else}
            <span class="h-[18px] w-[18px] rounded-full border border-dashed border-border"></span>
          {/if}
          {bare(measureOf(load, unit))}
        </span>
      {/each}
    </span>
  {/if}
  <span class="shrink-0 text-[0.75rem] tabular-nums text-fg-muted" data-testid="issues-backlog-numbers">
    {#if sec.unread}
      Not read yet
    {:else}
      {#each line as part, i (i)}{#if part.tone === 'to'}<span class="text-accent" data-testid="issues-backlog-to">{part.text}</span>{:else if part.tone === 'warn'}<span class="text-warning" data-testid="issues-backlog-warn">{part.text}</span>{:else}{part.text}{/if}{/each}
    {/if}
  </span>
  {#if controls}{@render controls()}{/if}
  {#if sec.kind === 'future' && pace}
    <div class="h-[3px] w-full overflow-hidden rounded-full bg-surface-2" data-testid="issues-backlog-cap" data-over={numbers.over > 0 ? '' : undefined}>
      <div class="h-full rounded-full {numbers.over > 0 ? 'bg-warning' : 'bg-accent'}" style:width="{fill}%"></div>
    </div>
  {/if}
</header>
