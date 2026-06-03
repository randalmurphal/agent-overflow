<script lang="ts">
  // Stub ThreadRow used by TerminalsGroup.test.ts. The real ThreadRow mounts
  // status pills, context menus, drag handles, and pulls in the panes /
  // keybindings / settings stores. The Terminals group only needs to verify
  // that it renders one row per terminal (and which), so this placeholder
  // exposes the thread id + mode and swallows the rest of the props.
  import type { Thread } from '../../lib/types/models';

  let { thread }: { thread: Thread } = $props();
</script>

<div
  data-testid="stub-thread-row"
  data-thread-id={thread.id}
  data-thread-mode={thread.mode}
>
  {thread.title}
</div>
