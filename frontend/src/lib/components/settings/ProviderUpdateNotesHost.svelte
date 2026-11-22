<script lang="ts">
  // Renders the one release-notes modal in stores/providerUpdates.svelte.ts.
  // Mounted once at the app root: a toast and both provider pages open it,
  // and it stays tied to the computer that reported the update.
  //
  // Only expanded versions render their Markdown (one release body is tens
  // of KB), so opening the modal costs one body: the newest, expanded by
  // default. Bodies are third-party Markdown: no path links, no thread, and
  // embedded HTML stays escaped text.
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import ProviderUpdateButton from './ProviderUpdateButton.svelte';
  import ProviderUpdateFailure from './ProviderUpdateFailure.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { providerLabel } from '../../providers/catalog';
  import { scopeGrant } from '../../transport/scopes';
  import {
    closeProviderUpdateNotes,
    providerUpdate,
    providerUpdateComputerName,
    providerUpdateNotes,
    retryProviderUpdateNotes,
  } from '../../stores/providerUpdates.svelte';

  let view = $derived(providerUpdateNotes());
  let provider = $derived(view?.provider ?? '');
  let backend = $derived(view?.backend ?? '');
  let label = $derived(providerLabel(provider));
  let loading = $derived(view?.loading ?? false);
  let error = $derived(view?.error ?? '');
  let notes = $derived(view?.notes ?? null);
  let releases = $derived(notes?.releases ?? []);
  let omitted = $derived(notes?.omitted ?? 0);
  let releasesUrl = $derived(notes?.url ?? '');

  let update = $derived(view !== null && scopeGrant('access:admin', backend) === 'granted' ? providerUpdate(provider, backend) : null);
  let computer = $derived(view !== null ? providerUpdateComputerName(backend) : '');
  let fromVersion = $derived(notes?.fromVersion || update?.installedVersion || '');
  let toVersion = $derived(notes?.toVersion || update?.latestVersion || '');

  // The newest version starts expanded; each new answer resets the choice.
  let expanded = $derived<ReadonlySet<string>>(new Set(releases.length > 0 ? [releases[0].version] : []));

  function toggle(version: string): void {
    const next = new Set(expanded);
    if (next.has(version)) next.delete(version);
    else next.add(version);
    expanded = next;
  }

  function publishedDate(value: string): string {
    if (!value) return '';
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? '' : date.toLocaleDateString();
  }
</script>

<Modal
  open={view !== null}
  title="What's new in {label}"
  onClose={closeProviderUpdateNotes}
  width="lg"
  padding="comfortable"
>
  {#snippet children()}
    <div class="flex flex-col gap-3" data-testid="provider-update-notes-modal">
      {#if (fromVersion && toVersion) || computer}
        <p class="text-[0.75rem] text-fg-muted" data-testid="provider-update-notes-range">
          {#if fromVersion && toVersion}
            <span class="font-mono">{fromVersion}</span> to <span class="font-mono">{toVersion}</span>
          {/if}
          {#if computer}on {computer}{/if}
        </p>
      {/if}
      {#if view !== null}
        <ProviderUpdateFailure {provider} {backend} testId="provider-update-notes-update-error" />
      {/if}
      {#if loading}
        <p role="status" class="text-[0.8125rem] text-fg-muted">Loading release notes…</p>
      {:else if error}
        <div class="flex flex-col items-start gap-2" data-testid="provider-update-notes-error">
          <p role="alert" class="text-[0.8125rem] text-error">{error}</p>
          <Button size="sm" variant="secondary" onclick={retryProviderUpdateNotes}>
            {#snippet children()}Retry{/snippet}
          </Button>
        </div>
      {:else if notes !== null && releases.length === 0}
        <p class="text-[0.8125rem] text-fg-muted" data-testid="provider-update-notes-empty">
          No release notes were published for these versions.
        </p>
      {:else}
        <div class="flex flex-col divide-y divide-border-subtle rounded-[var(--radius-field)] border border-border-subtle">
          {#each releases as release (release.version)}
            {@const open = expanded.has(release.version)}
            {@const date = publishedDate(release.publishedAt)}
            <section data-testid="provider-update-notes-version" data-version={release.version}>
              <button
                type="button"
                class="flex w-full cursor-pointer items-center gap-2 px-3 py-2 compact:py-3 compact:select-none text-left text-[0.8125rem] text-fg hover:bg-surface-2/30 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40"
                aria-expanded={open}
                onclick={() => toggle(release.version)}
              >
                <Icon
                  icon={ChevronRight}
                  size={14}
                  strokeWidth={2}
                  class="shrink-0 text-fg-hint transition-transform {open ? 'rotate-90' : ''}"
                />
                <span class="min-w-0 flex-1 truncate font-medium">{release.name || release.version}</span>
                {#if date}
                  <span class="shrink-0 text-[0.6875rem] text-fg-hint">{date}</span>
                {/if}
              </button>
              {#if open}
                <div class="px-3 pb-3 text-[0.8125rem]" data-testid="provider-update-notes-body">
                  {#if release.body}
                    <ChatMarkdown source={release.body} pathRefs={EMPTY_PATH_REFS} class="select-text" />
                  {:else}
                    <p class="text-fg-muted">No notes were published for this version.</p>
                  {/if}
                </div>
              {/if}
            </section>
          {/each}
        </div>
        {#if omitted > 0}
          <p class="text-[0.75rem] text-fg-muted" data-testid="provider-update-notes-omitted">
            {omitted === 1 ? '1 older version' : `${omitted} older versions`} not shown.
            {#if releasesUrl}
              <a href={releasesUrl} target="_blank" rel="noopener noreferrer" class="text-accent hover:underline">See all releases</a>
            {/if}
          </p>
        {/if}
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    {#if releasesUrl}
      <a
        href={releasesUrl}
        target="_blank"
        rel="noopener noreferrer"
        class="mr-auto self-center text-[0.75rem] text-accent hover:underline"
        data-testid="provider-update-notes-releases-link"
      >Releases page</a>
    {/if}
    <Button size="sm" variant="secondary" onclick={closeProviderUpdateNotes}>
      {#snippet children()}Close{/snippet}
    </Button>
    {#if view !== null}
      <ProviderUpdateButton
        {provider}
        {backend}
        variant="primary"
        size="sm"
        testId="provider-update-notes-update-button"
      />
    {/if}
  {/snippet}
</Modal>
