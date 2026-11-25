<script lang="ts">
  // An issue type's mark, picked by its name or level (`boardFormat.ts`):
  // the board answer carries no tracker icon, and the app fetches none.
  import Bookmark from '@lucide/svelte/icons/bookmark';
  import Bug from '@lucide/svelte/icons/bug';
  import CornerDownRight from '@lucide/svelte/icons/corner-down-right';
  import Square from '@lucide/svelte/icons/square';
  import SquareCheck from '@lucide/svelte/icons/square-check';
  import Zap from '@lucide/svelte/icons/zap';
  import Icon from '../primitives/Icon.svelte';
  import type { IssueTypeView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { typeMark, type TypeMark } from './boardFormat';

  let { type, testId }: { type: IssueTypeView | null | undefined; testId: string } = $props();

  const TYPE_ICONS = {
    epic: { icon: Zap, tone: 'text-accent' },
    subtask: { icon: CornerDownRight, tone: 'text-fg-muted' },
    bug: { icon: Bug, tone: 'text-error' },
    story: { icon: Bookmark, tone: 'text-success' },
    task: { icon: SquareCheck, tone: 'text-info' },
    other: { icon: Square, tone: 'text-fg-muted' },
  } satisfies Record<TypeMark, unknown>;

  const mark = $derived(TYPE_ICONS[typeMark(type)]);
</script>

<span class="inline-flex shrink-0 {mark.tone}" title={type?.name ?? 'No type'} data-testid={testId}>
  <Icon icon={mark.icon} size={13} strokeWidth={2} />
</span>
