<script lang="ts">
  // Everything the modal renders that is not a row: the scan running, the
  // scan failing, every provider home being unreadable, an empty catalogue,
  // filters that match nothing — plus the two inline notices that coexist
  // with a healthy list (a refused run, a provider that read partially).
  //
  // One component because it is one slot. The modal renders this in exactly
  // one place, between the progress strip and the list, and what appears
  // there is entirely a function of `surface`. Splitting the seven blocks
  // apart would put the same alert markup in several files and the choice
  // between them in one more.
  //
  // Reads the catalogue's own state from the store, like the toolbar and the
  // progress strip do. The two props are the modal's business: which state
  // applies (the filtered projection lives up there) and the filter reset.

  import Button from '../primitives/Button.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { providerLabel } from '../../providers/catalog';
  import { getImportProviders, getSessionImportError } from '../../stores/sessionImport.svelte';
  import { surfaceHasCatalog, type ImportSurface } from '../../stores/sessionImportFilter';

  interface Props {
    surface: ImportSurface;
    onClearFilters: () => void;
  }

  let { surface, onClearFilters }: Props = $props();

  let catalogError = $derived(getSessionImportError());
  let providers = $derived(getImportProviders());
  // Notices ride alongside a catalogue. On the `error` surface the same
  // string is already the headline alert below, and on the others there is
  // no catalogue for them to annotate.
  let hasCatalog = $derived(surfaceHasCatalog(surface));
  let providerWarnings = $derived(
    hasCatalog ? providers.filter((p) => p.available && p.error !== '') : [],
  );
</script>

{#if hasCatalog && catalogError}
  <!-- The store funnels a refused/failed run start into the same field as a
       failed scan, and a run start does NOT clear the catalogue — so this has
       to render over a healthy list too. -->
  <p
    role="alert"
    class="mx-3 mt-2 rounded-md border border-error/40 bg-error/10 px-3 py-1.5 text-xs text-error"
    data-testid="session-import-run-error"
  >
    {catalogError}
  </p>
{/if}

{#each providerWarnings as provider (provider.provider)}
  <p
    role="status"
    class="mx-3 mt-2 rounded-md border border-warning/40 bg-warning/10 px-3 py-1.5 text-xs text-warning"
    data-testid={`session-import-provider-warning-${provider.provider}`}
  >
    {providerLabel(provider.provider)}: {provider.error}
  </p>
{/each}

{#if surface === 'loading'}
  <div
    class="flex items-center gap-2 px-5 py-8 text-[0.8125rem] text-fg-muted"
    data-testid="session-import-loading"
  >
    <SteppedSpinner size={12} />
    Scanning Claude Code and Codex session files…
  </div>
{:else if surface === 'error'}
  <p
    role="alert"
    class="m-4 rounded-md border border-error/40 bg-error/10 px-3 py-2 text-xs text-error"
    data-testid="session-import-error"
  >
    {catalogError}
  </p>
{:else if surface === 'unavailable'}
  <div
    role="alert"
    class="m-4 flex flex-col gap-1 rounded-md border border-error/40 bg-error/10 px-3 py-2 text-xs text-error"
    data-testid="session-import-providers-unavailable"
  >
    <p class="font-medium">Agent Overflow can't read any provider session files.</p>
    {#each providers as provider (provider.provider)}
      <p>
        {providerLabel(provider.provider)}: {provider.error || 'session files unavailable'}
      </p>
    {/each}
  </div>
{:else if surface === 'empty'}
  <p class="px-5 py-8 text-[0.8125rem] text-fg-muted" data-testid="session-import-empty">
    No sessions to import — everything Agent Overflow can see is already here.
  </p>
{:else if surface === 'no-matches'}
  <div class="flex flex-col items-start gap-2 px-5 py-8" data-testid="session-import-no-matches">
    <p class="text-[0.8125rem] text-fg-muted">No sessions match these filters.</p>
    <Button
      variant="ghost"
      size="sm"
      testId="session-import-clear-filters"
      onclick={onClearFilters}
    >
      {#snippet children()}Clear filters{/snippet}
    </Button>
  </div>
{/if}
