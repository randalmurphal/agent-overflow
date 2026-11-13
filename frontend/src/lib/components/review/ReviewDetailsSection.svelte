<script lang="ts">
  import Pencil from '@lucide/svelte/icons/pencil';
  import RotateCw from '@lucide/svelte/icons/rotate-cw';
  import Users from '@lucide/svelte/icons/users';
  import Icon from '../primitives/Icon.svelte';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewCollapsibleSection from './ReviewCollapsibleSection.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import ReviewManageError from './ReviewManageError.svelte';
  import ReviewPeoplePicker from './ReviewPeoplePicker.svelte';
  import { detailsSectionOpen, PR_MANAGE_ACTION_LABEL, type PRManageView } from '../../stores/prManage.svelte';
  import {
    canEditRow,
    canRerequest,
    labelColor,
    personKey,
    REVIEWER_STATE_LABEL,
    reviewerStateClass,
    type PRManageRow,
  } from '../../utils/prManage';
  import { authorDisplayName } from '../../utils/reviewIdentity';
  import type { PRDetail, PRPerson, PRReviewer } from '../../types/models';

  // The overview's Details section, between Merge and Description:
  // reviewers with where their review stands, assignees and labels. Each
  // row has one Edit (only where the forge lets the viewer change it)
  // opening a searchable multi-select whose close applies the row's
  // change as one write. A settled or outdated review has a Re-request
  // icon. Collapsed by default while every row is empty; the reader's
  // choice lives in the review state. Nothing here is optimistic: the
  // rows are the forge's answer, and a control is pending until it is on
  // screen.

  interface Props {
    manage: PRManageView;
    detail: PRDetail;
    forge: string;
  }

  let { manage, detail, forge }: Props = $props();

  const data = $derived(detail.manage);
  const reviewers = $derived<readonly PRReviewer[]>(data?.reviewers ?? []);
  const assignees = $derived<readonly PRPerson[]>(data?.assignees ?? []);
  const labels = $derived(data?.labels ?? []);
  // The management read failed: its sentence instead of empty rows.
  const unavailable = $derived(data?.unavailable ?? '');
  const open = $derived(detailsSectionOpen(manage.detailsOpen, detail));
  const pending = $derived(manage.pending);
  const blocked = $derived(manage.blockedReason);
  const picker = $derived(manage.picker);

  const summary = $derived.by(() => {
    const parts: string[] = [];
    if (reviewers.length) parts.push(`${reviewers.length} ${reviewers.length === 1 ? 'reviewer' : 'reviewers'}`);
    if (assignees.length) parts.push(`${assignees.length} ${assignees.length === 1 ? 'assignee' : 'assignees'}`);
    if (labels.length) parts.push(`${labels.length} ${labels.length === 1 ? 'label' : 'labels'}`);
    return parts.join(' · ');
  });

  const anchors: Record<PRManageRow, HTMLButtonElement | undefined> = $state({ reviewers: undefined, assignees: undefined, labels: undefined });
  const ROW_LABEL: Record<PRManageRow, string> = { reviewers: 'Reviewers', assignees: 'Assignees', labels: 'Labels' };
  const NOUN: Record<PRManageRow, string> = { reviewers: 'reviewers', assignees: 'assignees', labels: 'labels' };

  function openPicker(row: PRManageRow): void {
    if (picker.row === row) {
      void picker.close();
      return;
    }
    if (row === 'labels') {
      picker.open(row, { labels });
      return;
    }
    // A GitHub review that is in cannot be withdrawn, only a pending
    // request; GitLab removes any reviewer.
    const locked = row === 'reviewers' && forge === 'github'
      ? reviewers.filter((reviewer) => reviewer.state !== 'PENDING').map(personKey)
      : [];
    picker.open(row, {
      people: row === 'reviewers' ? reviewers : assignees,
      locked,
      teams: row === 'reviewers' && forge === 'github' && !!data?.can.reviewerTeams,
    });
  }

  function rowPending(row: PRManageRow): boolean {
    return pending?.kind === row;
  }

  const revealClass = 'inline-flex shrink-0 transition-opacity';
  const hiddenClass = 'opacity-0 focus-within:opacity-100 group-hover/row:opacity-100 compact:opacity-100';
  const rowClass = 'group/row flex min-h-9 items-start gap-2.5 px-3 py-1.5 text-xs';
  // The values and, under them, a failed change's sentence.
  const valuesClass = 'flex min-w-0 flex-1 flex-col gap-1';
  const nameClass = 'w-20 shrink-0 pt-1 text-fg-muted compact:pt-2';
  const noneClass = 'pt-1 text-fg-subtle compact:pt-2';
</script>

{#snippet editButton(row: PRManageRow)}
  {#if canEditRow(detail, row)}
    <span class="{revealClass} {rowPending(row) || picker.row === row ? 'opacity-100' : hiddenClass}">
      <ReviewIconButton
        bind:element={anchors[row]}
        icon={Pencil}
        label="Edit {NOUN[row]}"
        testid="review-details-edit-{row}"
        haspopup="dialog"
        expanded={picker.row === row}
        pending={rowPending(row)}
        focusableWhilePending
        disabled={blocked !== null && picker.row !== row && !rowPending(row)}
        disabledLabel={rowPending(row) ? `${PR_MANAGE_ACTION_LABEL[row]}…` : (blocked ?? undefined)}
        onclick={() => openPicker(row)}
      />
    </span>
  {/if}
{/snippet}

{#snippet rowError(row: PRManageRow)}
  {@const error = manage.rowError(row)}
  {#if error}
    <div data-testid="review-details-row-error" data-row={row}>
      <ReviewManageError {error} testid="review-details-error-{row}" />
    </div>
  {/if}
{/snippet}

{#snippet person(each: PRPerson)}
  {#if each.team}
    <span class="inline-flex size-4 shrink-0 items-center justify-center text-fg-muted"><Icon icon={Users} size={12} /></span>
  {:else}
    <ReviewAvatar login={each.login} name={each.name} size={16} />
  {/if}
  <span class="max-w-full truncate text-fg" title="@{each.login}">{authorDisplayName({ authorLogin: each.login, authorName: each.name })}</span>
{/snippet}

<ReviewCollapsibleSection
  label="Details"
  {open}
  onToggle={() => manage.setDetailsOpen(!open)}
  testid="review-pr-details"
>
  {#snippet badge()}
    {#if !open && summary}
      <span class="min-w-0 truncate text-[0.6875rem] font-normal text-fg-subtle" data-testid="review-details-summary">{summary}</span>
    {/if}
  {/snippet}
  {#if unavailable}
    <div class="px-3 py-2 text-xs text-fg-muted" data-testid="review-details-unavailable">{unavailable}</div>
  {:else}
  <div class="flex flex-col divide-y divide-border-subtle" data-testid="review-details-rows">
    <div class={rowClass} data-testid="review-details-row" data-row="reviewers">
      <span class={nameClass}>{ROW_LABEL.reviewers}</span>
      <div class={valuesClass}>
        <div class="flex min-w-0 flex-col">
        {#each reviewers as reviewer (personKey(reviewer))}
          {@const rerequesting = pending?.kind === 'rerequest' && pending.target === reviewer.login}
          <div class="group/reviewer flex min-h-6 min-w-0 flex-wrap items-center gap-x-1.5 compact:min-h-8" data-testid="review-details-reviewer" data-login={reviewer.login} data-state={reviewer.state}>
            {@render person(reviewer)}
            <span class="shrink-0 rounded-full px-1.5 text-[0.6875rem] {reviewerStateClass(reviewer.state)}" data-testid="review-details-reviewer-state">{REVIEWER_STATE_LABEL[reviewer.state] ?? reviewer.state.toLowerCase()}</span>
            {#if reviewer.stale}
              <span class="shrink-0 text-[0.6875rem] text-fg-subtle" title="Reviewed an older commit">outdated</span>
            {/if}
            {#if canRerequest(detail, reviewer)}
              <span class="{revealClass} {rerequesting ? 'opacity-100' : 'opacity-0 focus-within:opacity-100 group-hover/reviewer:opacity-100 compact:opacity-100'}">
                <ReviewIconButton
                  icon={RotateCw}
                  label="Re-request review from {authorDisplayName({ authorLogin: reviewer.login, authorName: reviewer.name })}"
                  testid="review-details-rerequest"
                  pending={rerequesting}
                  focusableWhilePending
                  disabled={blocked !== null && !rerequesting}
                  disabledLabel={rerequesting ? `${PR_MANAGE_ACTION_LABEL.rerequest}…` : (blocked ?? undefined)}
                  onclick={() => { void manage.rerequest(reviewer); }}
                />
              </span>
            {/if}
          </div>
        {:else}
          <span class={noneClass}>None</span>
        {/each}
        </div>
        {@render rowError('reviewers')}
      </div>
      {@render editButton('reviewers')}
    </div>
    <div class={rowClass} data-testid="review-details-row" data-row="assignees">
      <span class={nameClass}>{ROW_LABEL.assignees}</span>
      <div class={valuesClass}>
      <div class="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 pt-0.5 compact:pt-1.5">
        {#each assignees as assignee (personKey(assignee))}
          <span class="inline-flex min-h-5 min-w-0 items-center gap-1.5" data-testid="review-details-assignee" data-login={assignee.login}>
            {@render person(assignee)}
          </span>
        {:else}
          <span class="pt-0.5 text-fg-subtle">None</span>
        {/each}
      </div>
      {@render rowError('assignees')}
      </div>
      {@render editButton('assignees')}
    </div>
    <div class={rowClass} data-testid="review-details-row" data-row="labels">
      <span class={nameClass}>{ROW_LABEL.labels}</span>
      <div class={valuesClass}>
      <div class="flex min-w-0 flex-wrap items-center gap-1 pt-0.5 compact:pt-1.5">
        {#each labels as label (label.name)}
          {@const color = labelColor(label)}
          <!-- A neutral chip with the forge's color as a dot: a forge
               color as the chip's fill is not legible in every theme. -->
          <span class="inline-flex h-5 max-w-full min-w-0 items-center gap-1.5 rounded-full border border-border-subtle bg-surface-2/60 px-2 text-[0.6875rem] text-fg" data-testid="review-details-label" data-name={label.name}>
            <span class="size-2 shrink-0 rounded-full {color ? '' : 'bg-fg-subtle'}" style:background-color={color}></span>
            <span class="truncate">{label.name}</span>
          </span>
        {:else}
          <span class="pt-0.5 text-fg-subtle">None</span>
        {/each}
      </div>
      {@render rowError('labels')}
      </div>
      {@render editButton('labels')}
    </div>
  </div>
  {/if}
</ReviewCollapsibleSection>

{#if picker.row !== null}
  <ReviewPeoplePicker {picker} anchor={anchors[picker.row]} noun={NOUN[picker.row]} />
{/if}
