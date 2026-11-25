<script lang="ts">
  // The Issues pane's board picker: every board some project lists, on
  // every attached computer. Grouped by computer when more than one lists
  // boards, then by project with its tracker; the board shown is checked.
  // Opening it reads the boards again (`onOpen`). A board that cannot be
  // opened (`isBoardChoiceOpenable`: its connection is turned off) is
  // listed, not offered.
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Kanban from '@lucide/svelte/icons/kanban';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuSectionHeader from '../primitives/MenuSectionHeader.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { choiceRef, sameBoard } from '../../stores/issuesPane.svelte';
  import type { TrackerBoardRef } from '../../stores/trackerBoard.svelte';
  import { isBoardChoiceOpenable, type ListedBoardChoice } from '../../stores/trackerBoardChoices.svelte';

  interface Props {
    choices: readonly ListedBoardChoice[];
    current: TrackerBoardRef | null;
    /** The name a computer is shown by. */
    computerName: (backend: string) => string;
    /** The name a project is shown by. */
    projectName: (backend: string, projectId: string) => string;
    /** The shown board's name as its subscription read it, for a board
     * shown before the boards are listed. */
    currentName: string | null;
    /** What the trigger says with no board shown and none listed. */
    emptyLabel: string;
    /** What the open picker says with none listed. */
    emptyText: string;
    onPick: (ref: TrackerBoardRef) => void;
    onOpen: () => void;
  }

  let { choices, current, computerName, projectName, currentName, emptyLabel, emptyText, onPick, onOpen }: Props = $props();

  const KIND_NAMES: Readonly<Record<string, string>> = { jira: 'Jira', linear: 'Linear' };

  let open = $state(false);
  let triggerEl: HTMLButtonElement | undefined = $state();

  const currentChoice = $derived(choices.find((c) => sameBoard(choiceRef(c), current)) ?? null);
  const label = $derived.by(() => {
    if (currentChoice) return boardName(currentChoice);
    if (current) return currentName ?? 'Board';
    return choices.length === 0 ? emptyLabel : 'Pick a board';
  });
  const severalComputers = $derived(new Set(choices.map((c) => c.backend)).size > 1);

  interface Group {
    key: string;
    computer: string | null;
    heading: string;
    boards: ListedBoardChoice[];
  }

  const groups = $derived.by(() => {
    const out: Group[] = [];
    const byKey = new Map<string, Group>();
    for (const c of choices) {
      const key = `${c.backend}\n${c.choice.project}\n${c.choice.connection}`;
      let group = byKey.get(key);
      if (!group) {
        const kind = KIND_NAMES[c.choice.connection_kind] ?? c.choice.connection_kind;
        const tracker = c.choice.connection_label ? `${kind}, ${c.choice.connection_label}` : kind;
        group = { key, computer: c.backend, heading: `${projectName(c.backend, c.choice.project)} · ${tracker}`, boards: [] };
        byKey.set(key, group);
        out.push(group);
      }
      group.boards.push(c);
    }
    return out;
  });

  function boardName(c: ListedBoardChoice): string {
    return c.choice.name || c.choice.board;
  }

  function toggle(): void {
    open = !open;
    if (open) onOpen();
  }

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl });
  }

  function pick(c: ListedBoardChoice): void {
    onPick(choiceRef(c));
    close();
  }
</script>

<button
  bind:this={triggerEl}
  type="button"
  class="inline-flex h-6 min-w-0 cursor-pointer items-center gap-1.5 rounded-[var(--radius-control)] px-2 text-sm text-fg transition-colors hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:h-9 compact:select-none"
  aria-haspopup="menu"
  aria-expanded={open}
  aria-label="Board: {label}"
  data-testid="issues-board-picker"
  onclick={toggle}
>
  <Icon icon={Kanban} size={13} strokeWidth={2} />
  <span class="truncate">{label}</span>
  <Icon icon={ChevronDown} size={11} strokeWidth={2} />
</button>
<Popover anchor={triggerEl} {open} onClose={close} placement="bottom-start" ariaLabel="Boards">
  <Menu ariaLabel="Boards" onClose={close} minWidthClass="min-w-[260px]">
    {#each groups as group, i (group.key)}
      {#if severalComputers && (i === 0 || groups[i - 1].computer !== group.computer)}
        <MenuSectionHeader label={computerName(group.computer!)} />
      {/if}
      <MenuSectionHeader label={group.heading} />
      {#each group.boards as c (c.choice.board)}
        <MenuItem
          label={boardName(c)}
          description={isBoardChoiceOpenable(c.choice) ? c.choice.tracker_project || undefined : 'Its connection is turned off'}
          checked={c === currentChoice}
          disabled={!isBoardChoiceOpenable(c.choice)}
          onSelect={() => pick(c)}
          testId="issues-board-choice"
        />
      {/each}
    {:else}
      <p class="px-3 py-2 text-sm text-fg-muted">{emptyText}</p>
    {/each}
  </Menu>
</Popover>
