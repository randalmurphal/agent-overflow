<script lang="ts">
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
  // In --connect (client) mode the panel is hidden. The remote
  // installation has its own editor catalog (the user's local apps),
  // and persisting a preference here would write into the remote
  // settings file instead of the user's local one.

  import {
    EditorSettings,
    GetEditorSettings,
    ListAvailableEditors,
    SetEditorSettings,
    type EditorInfo,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { isClientMode } from '../../transport/runMode';
  import MicroLabel from '../primitives/MicroLabel.svelte';

  const clientMode = isClientMode();

  let editors = $state<EditorInfo[]>([]);
  let preference = $state<string>('');
  // Track the last persisted value separately from `preference` so the
  // optimistic radio change can revert on RPC failure without re-fetching.
  let savedPreference = $state<string>('');
  let loading = $state(true);
  let saving = $state(false);

  async function load(): Promise<void> {
    if (clientMode) {
      loading = false;
      return;
    }
    loading = true;
    try {
      const [list, current] = await Promise.all([
        ListAvailableEditors(),
        GetEditorSettings(),
      ]);
      editors = (list as EditorInfo[]) ?? [];
      const pref = (current as EditorSettings | null)?.preference ?? '';
      preference = pref;
      savedPreference = pref;
    } catch (err) {
      addToast('error', `Failed to load editor settings: ${errString(err)}`);
    } finally {
      loading = false;
    }
  }

  async function selectEditor(id: string): Promise<void> {
    if (saving || preference === id) return;
    const previous = savedPreference;
    saving = true;
    preference = id;
    try {
      // Server validates against the live catalog at open time, not at
      // save time — invalid IDs persist quietly and silently fall back
      // to the catalog default. That matches Resolve()'s contract and
      // means we don't have to reject "VS Code (not installed)" here.
      const updated = (await SetEditorSettings(
        new EditorSettings({ preference: id }),
      )) as EditorSettings;
      preference = updated.preference;
      savedPreference = updated.preference;
    } catch (err) {
      preference = previous;
      addToast('error', `Failed to update editor preference: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  $effect(() => {
    void load();
  });
</script>

{#if clientMode}
  <section data-testid="editor-section-clientmode">
    <MicroLabel as="p">Open-in-editor</MicroLabel>
    <h3 class="mt-1 text-[15px] font-semibold text-fg">Editor</h3>
    <p class="mt-1 max-w-2xl text-[12px] text-fg-muted">
      Editor preferences are local to your install. This window is attached to a
      remote backend, so changes here would update the remote machine's catalog,
      not yours. Edit your editor preference from your local install.
    </p>
  </section>
{:else}
  <section>
    <MicroLabel as="p">Open-in-editor</MicroLabel>
    <h3 class="mt-1 text-[15px] font-semibold text-fg">Editor</h3>
    <p class="mt-1 max-w-2xl text-[12px] text-fg-muted">
      Choose which editor opens when you click a file path in the chat.
      "Auto" follows the catalog priority order (VS Code, Cursor, Zed, …)
      with <code class="font-mono text-[11px]">$EDITOR</code> / <code class="font-mono text-[11px]">$VISUAL</code>
      as the final fallback. Editors that aren't installed are listed for
      reference but can't be selected.
    </p>

    {#if loading}
      <p class="mt-3 text-[12px] text-fg-subtle" role="status" aria-live="polite">
        Loading editors…
      </p>
    {:else}
      <fieldset
        class="mt-3 space-y-1"
        role="radiogroup"
        aria-label="Preferred editor"
        data-testid="editor-section-radiogroup"
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
            disabled={saving}
            onchange={() => void selectEditor('')}
            class="accent-accent"
          />
          <span class="text-[13px] text-fg font-medium">Auto</span>
          <span class="text-[12px] text-fg-muted">Use the best available editor.</span>
        </label>

        {#each editors as editor (editor.id)}
          {@const disabled = !editor.available || saving}
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
              disabled={disabled}
              onchange={() => void selectEditor(editor.id)}
              class="accent-accent"
            />
            <span class="text-[13px] text-fg font-medium">{editor.name}</span>
            {#if !editor.available}
              <span class="text-[11px] text-fg-subtle italic">(not installed)</span>
            {:else if editor.envFallback}
              <span class="text-[11px] text-fg-subtle">($EDITOR / $VISUAL)</span>
            {/if}
          </label>
        {/each}
      </fieldset>
    {/if}
  </section>
{/if}
