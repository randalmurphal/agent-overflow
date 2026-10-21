<script lang="ts">
  // The projects an assistant serves (docs/specs/standing-agents.md ruling
  // 37): All projects, which takes projects added later too, or a checklist
  // of the computer's unarchived projects by name. New Assistant and the
  // assistant's Projects dialog both choose with it; the backend refuses an
  // empty choice. An archived project the choice already holds is not
  // listed and stays in it.
  import type { BackendKey } from '../../transport/backendKey';
  import { projectBackend } from '../../transport/entityIndex';
  import { getProjects } from '../../stores/projects.svelte';

  interface Props {
    /** The computer the assistant is on; nothing is listed while it is unknown. */
    backend: BackendKey | undefined;
    all: boolean;
    selected: readonly string[];
    onChange: (all: boolean, selected: string[]) => void;
    disabled?: boolean;
  }

  let { backend, all, selected, onChange, disabled = false }: Props = $props();

  let projects = $derived(
    backend === undefined
      ? []
      : getProjects()
        .filter((row) => !row.project.archived && projectBackend(row.project.id) === backend)
        .map((row) => row.project)
        .sort((a, b) => a.name.localeCompare(b.name)),
  );

  function toggle(id: string, checked: boolean): void {
    const next = selected.filter((other) => other !== id);
    if (checked) next.push(id);
    onChange(false, next);
  }
</script>

<div class="flex min-w-0 flex-col gap-1.5" role="group" aria-label="Projects" data-testid="assistant-projects-picker">
  <label class="flex items-center gap-2 text-xs text-fg">
    <input
      type="checkbox"
      class="accent-accent disabled:cursor-not-allowed"
      checked={all}
      {disabled}
      onchange={(e) => onChange(e.currentTarget.checked, selected.slice())}
      data-testid="assistant-projects-all"
    />
    <span>All projects</span>
    <span class="text-fg-hint">including ones added later</span>
  </label>
  <div
    class="flex max-h-44 flex-col overflow-y-auto rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 py-1"
    data-testid="assistant-projects-list"
  >
    {#each projects as project (project.id)}
      <label
        class="flex min-w-0 items-center gap-2 px-2 py-1 compact:py-2 text-xs {all ? 'text-fg-hint' : 'text-fg'}"
        title={project.path}
      >
        <input
          type="checkbox"
          class="accent-accent disabled:cursor-not-allowed"
          checked={all || selected.includes(project.id)}
          disabled={disabled || all}
          onchange={(e) => toggle(project.id, e.currentTarget.checked)}
          data-testid="assistant-projects-item"
          data-project-id={project.id}
        />
        <span class="min-w-0 truncate">{project.name}</span>
      </label>
    {:else}
      <p class="px-2 py-1 text-xs text-fg-hint">This computer has no projects yet.</p>
    {/each}
  </div>
</div>
