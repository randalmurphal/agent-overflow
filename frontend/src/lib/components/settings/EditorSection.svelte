<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  const { hasScope, backend } = settingsComputer();
  import { onMount } from 'svelte';
  // EditorSection: settings panel for the open-in-editor preference.
  // The Go-side catalog returns every editor it knows about
  // (available + unavailable), so the UI can offer a stable list and
  // mark the missing ones rather than silently hiding them — keeps the
  // picker predictable across machines.
  //
  // "Auto" is the empty-preference state: the catalog default applies
  // at open time, with $EDITOR / $VISUAL as the final fallback. Any
  // explicit pick is persisted as the editor ID so a vendor binary
  // upgrade doesn't quietly switch selection.
  //
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';

  import {
    ensureEditorsLoaded,
    getEditorPreference,
    getEditors,
    getEditorsError,
    getEditorsLoadStatus,
    hasEditorsSnapshot,
    refreshEditors,
    setEditorPreference,
  } from '../../stores/editors.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import { SECTION_PROSE_CLASS } from './styles';
  import Button from '../primitives/Button.svelte';

  let localOnly = $derived(!hasScope('settings:read'));
  let canWrite = $derived(hasScope('settings:write'));

  let editors = $derived(getEditors(backend));
  let preference = $derived(getEditorPreference(backend));
  let loadStatus = $derived(getEditorsLoadStatus(backend));
  let loadError = $derived(getEditorsError(backend));
  let hasSnapshot = $derived(hasEditorsSnapshot(backend));
  let saving = $state(false);

  function retryLoad(): void {
    if (localOnly || loadStatus === 'loading') return;
    void refreshEditors(backend);
  }

  async function selectEditor(id: string): Promise<void> {
    if (localOnly || !canWrite || saving || preference === id) return;
    saving = true;
    try {
      // Server validates against the live catalog at open time, not at
      // save time — invalid IDs persist quietly and silently fall back
      // to the catalog default. That matches Resolve()'s contract and
      // means we don't have to reject "VS Code (not installed)" here.
      await setEditorPreference(id, backend);
    } catch (err) {
      addToast('error', `Failed to update editor preference: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  onMount(() => {
    if (localOnly) return;
    void ensureEditorsLoaded(backend);
  });
</script>

{#if localOnly}
  <section data-testid="editor-section-clientmode">
    <SettingsCallout>
      This connection does not have permission to read this computer’s editor settings.
    </SettingsCallout>
  </section>
{:else}
  <section>
    <p class="{SECTION_PROSE_CLASS} mb-3">
      Choose the editor this computer uses when opening files. Auto picks an
      installed editor, with your environment’s editor as a fallback.
    </p>

    {#if loadStatus === 'loading' && !hasSnapshot}
      <p
        class="text-[0.75rem] text-fg-subtle"
        role="status"
        aria-live="polite"
      >
        Loading editors…
      </p>
    {:else if loadStatus === 'loading'}
      <p class="text-[0.75rem] text-fg-subtle" role="status" aria-live="polite">
        Refreshing editor choices…
      </p>
    {/if}

    {#if loadStatus === 'error' && loadError}
      <SettingsCallout tone="error">
        <div class="flex items-center gap-3">
          <div class="min-w-0 flex-1">
            <p class="font-semibold">Editor choices could not be loaded</p>
            <p class="mt-1 break-words font-mono text-[0.71875rem]">{loadError}</p>
            {#if hasSnapshot}
              <p class="mt-1 text-fg-muted">Showing the last editor list loaded successfully.</p>
            {/if}
          </div>
          <Button
            size="xs"
            onclick={retryLoad}
            testId="editor-section-retry"
            ariaLabel="Retry loading editor choices"
          >
            {#snippet children()}Retry{/snippet}
          </Button>
        </div>
      </SettingsCallout>
    {/if}

    {#if hasSnapshot}
      <fieldset
        class="flex flex-col gap-0.5"
        role="radiogroup"
        aria-label="Preferred editor"
        data-testid="editor-section-radiogroup"
        data-settings-field="editor.preferred"
        data-settings-label="Preferred editor"
      >
        <label
          class="flex items-center gap-2.5 rounded-[var(--radius-field)] px-2 py-1.5 cursor-pointer hover:bg-surface-2/30 transition-colors"
          data-testid="editor-option-auto"
        >
          <input
            type="radio"
            name="editor-preference"
            value=""
            checked={preference === ''}
            disabled={saving || !canWrite}
            onchange={() => void selectEditor('')}
            class="accent-accent"
          />
          <span class="text-[0.8125rem] font-medium text-fg">Auto</span>
          <span class="text-[0.75rem] text-fg-muted">Use the best available editor.</span>
        </label>

        {#each editors as editor (editor.id)}
          {@const disabled = !editor.available || saving || !canWrite}
          <label
            class="flex items-center gap-2.5 rounded-[var(--radius-field)] px-2 py-1.5 transition-colors
              {!editor.available ? 'opacity-60' : 'hover:bg-surface-2/30 cursor-pointer'}"
            data-testid="editor-option-{editor.id}"
            data-available={editor.available}
          >
            <input
              type="radio"
              name="editor-preference"
              value={editor.id}
              checked={preference === editor.id}
              {disabled}
              onchange={() => void selectEditor(editor.id)}
              class="accent-accent"
            />
            <span class="text-[0.8125rem] font-medium text-fg">{editor.name}</span>
            {#if !editor.available}
              <span class="text-[0.6875rem] italic text-fg-hint">(not installed)</span>
            {:else if editor.envFallback}
              <span class="text-[0.6875rem] text-fg-hint">($EDITOR / $VISUAL)</span>
            {/if}
          </label>
        {/each}
      </fieldset>
    {/if}
  </section>
{/if}
