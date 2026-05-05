<script lang="ts">
  // Empty pane shown when the active mode tab doesn't match the loaded
  // thread's mode AND no thread of the target mode exists in the same
  // project. The pane stays "in" the original thread (so flipping back
  // to the matching tab is instant); this overlay just covers the main
  // surface with a quiet "no threads of this type yet" message and
  // points the user at the project's + button.
  //
  // Tab navigation is pure navigation by spec — it never auto-creates a
  // thread. + New on the project is the only path to a fresh thread,
  // and it now creates a thread of whichever mode tab is active.

  import Frame from 'lucide-svelte/icons/frame';
  import MessagesSquare from 'lucide-svelte/icons/messages-square';
  import Plus from 'lucide-svelte/icons/plus';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    /** The mode whose empty state is being shown ('chat' | 'design'). */
    mode: 'chat' | 'design';
    /** Display name of the project the empty state is scoped to. */
    projectName: string;
  }

  let { mode, projectName }: Props = $props();

  let icon = $derived(mode === 'design' ? Frame : MessagesSquare);
  let modeLabel = $derived(mode === 'design' ? 'design' : 'chat');
</script>

<div
  data-testid="mode-empty-for-project"
  data-mode={mode}
  class="chat-surface-ground flex h-full w-full items-center justify-center px-8"
>
  <div class="flex flex-col items-center text-center max-w-sm">
    <div class="mb-4 rounded-[var(--radius-card)] border border-dashed border-border-subtle bg-surface-1/40 p-6 text-fg-hint">
      <Icon {icon} size={28} strokeWidth={1.3} />
    </div>
    <p class="text-[13px] text-fg">
      No {modeLabel} threads in
      <span class="font-medium">{projectName}</span> yet.
    </p>
    <p class="mt-1 text-[12px] text-fg-muted leading-relaxed">
      Hover the project in the sidebar and click
      <span class="inline-flex items-center align-middle gap-1 rounded-[var(--radius-field)] border border-border-subtle px-1 py-0.5 mx-0.5 text-fg-muted">
        <Icon icon={Plus} size={10} strokeWidth={2} />
        <span class="text-[10px]">New</span>
      </span>
      to start one.
    </p>
  </div>
</div>
