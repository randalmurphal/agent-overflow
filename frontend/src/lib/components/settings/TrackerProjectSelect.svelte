<script lang="ts" module>
  import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
  import { projectBackend } from '../../transport/entityIndex';
  import { getProjects } from '../../stores/projects.svelte';

  /** The projects an Issues settings page of `backend` offers: that
   * computer's, since a project's tracker settings live on its computer.
   * Reactive. */
  export function computerProjects(backend: BackendKey) {
    return getProjects().filter((row) => (projectBackend(row.project.id) ?? HOME_BACKEND) === backend);
  }
</script>

<script lang="ts">
  // The project a per-project Issues settings page is about: the page's
  // computer's projects, the pick kept while it is listed, else the first.
  // A requested project (a deep link) wins once it is listed.
  import { untrack } from 'svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { disambiguatedPathLabels, formatPathLabel } from '../../utils/pathDisplay';
  import SettingsField from './SettingsField.svelte';
  import type { SettingsFieldId } from './fields';
  import { SELECT_CLASS } from './styles';

  interface Props {
    backend: BackendKey;
    /** The field's entry in the search index and the select's test ID. */
    field: SettingsFieldId;
    testId: string;
    hint: string;
    selected: string;
    /** A project asked for, taken once it is listed; null for none. */
    requested?: string | null;
    onRequestTaken?: () => void;
  }

  let { backend, field, testId, hint, selected = $bindable(), requested = null, onRequestTaken }: Props = $props();

  let projects = $derived(computerProjects(backend));
  let labels = $derived(disambiguatedPathLabels(projects.map((row) => row.project)));

  $effect(() => {
    if (requested !== null && projects.some((p) => p.project.id === requested)) {
      selected = requested;
      untrack(() => onRequestTaken?.());
      return;
    }
    if (projects.length === 0) {
      selected = '';
      return;
    }
    if (!projects.some((p) => p.project.id === untrack(() => selected))) selected = projects[0].project.id;
  });
</script>

<SettingsField id={field} label="Project" {hint} htmlFor={testId} stacked={isCompactLayout()}>
  <select
    id={testId}
    data-testid={testId}
    class={SELECT_CLASS}
    value={selected}
    onchange={(e) => (selected = (e.target as HTMLSelectElement).value)}
  >
    {#each projects as entry (entry.project.id)}
      <option value={entry.project.id}>{formatPathLabel(labels.get(entry.project.id)!)}</option>
    {/each}
  </select>
</SettingsField>
