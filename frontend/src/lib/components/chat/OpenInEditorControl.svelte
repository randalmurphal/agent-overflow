<script lang="ts">
  import type { BackendKey } from '../../transport/backendKey';
  // The chat header's "Open" affordance. The primary segment opens the
  // project in the user's default editor and shows that editor's icon so
  // the button says *where* it will open. When more than one editor is
  // available a caret opens a dropdown to launch the project in a
  // different one — just for that click; the saved default (set in
  // Settings) is unchanged, and the check marks which one that is.
  //
  // Split-button chrome is shared with GitActionsControl (the Commit
  // button beside it) via SPLIT_BTN_BASE so the two read as one cluster.
  //
  // The button works before the editor catalog loads: an empty editorID
  // lets the backend resolve the default independently, so only the icon
  // and the dropdown depend on the shared store having loaded.
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import { openInEditor } from '../../stores/openInEditor';
  import { errString } from '../../utils/errors';
  import { addToast } from '../../stores/toast.svelte';
  import {
    ensureEditorsLoaded,
    getAvailableEditors,
    getResolvedEditor,
  } from '../../stores/editors.svelte';
  import { SPLIT_BTN_BASE } from '../primitives/splitButton';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Icon from '../primitives/Icon.svelte';
  import EditorIcon from '../shared/EditorIcon.svelte';
  import { hasScope } from '../../transport/scopes';

  let { backend, path, name }: { backend: BackendKey; path: string; name: string } = $props();

  // The editor catalog and the open itself both act on the host desktop.
  let noHost = $derived(!hasScope('host', backend));
  let available = $derived(getAvailableEditors(backend));
  let resolved = $derived(getResolvedEditor(backend));
  let hasChoice = $derived(available.length > 1);

  let showDropdown = $state(false);
  let menuTriggerEl: HTMLButtonElement | undefined = $state(undefined);

  $effect(() => {
    if (noHost) return;
    void ensureEditorsLoaded(backend);
  });

  // path is the project root (already absolute); workspacePath is unused.
  // Empty editorID → backend resolves the default; a concrete id → open
  // in exactly that editor, this once, without touching the default.
  async function openIn(editorID: string): Promise<void> {
    if (noHost) return;
    try {
      await openInEditor(backend, path, 0, 0, '', editorID);
    } catch (err) {
      addToast('error', errString(err));
    } finally {
      // A primary open does not need the frontend catalog, but it is a useful
      // revalidation edge after the shared snapshot reaches its 60s TTL.
      void ensureEditorsLoaded(backend);
    }
  }

  function closeMenu(reason?: PopoverCloseReason): void {
    showDropdown = false;
    restorePickerFocus(reason, { triggerEl: menuTriggerEl });
  }

  function selectEditor(editorID: string): void {
    showDropdown = false;
    void openIn(editorID);
  }

  let primaryTitle = $derived(
    resolved
      ? `Open ${name} in ${resolved.name}`
      : `Open ${name} in editor`,
  );
</script>

{#if !noHost}
  <div class="flex shrink-0">
    <button
      onclick={() => void openIn('')}
      title={primaryTitle}
      aria-label={primaryTitle}
      data-testid="chat-header-open-editor"
      class="{SPLIT_BTN_BASE} gap-1 px-2 {hasChoice ? 'rounded-l' : 'rounded'}"
    >
      <EditorIcon editorId={resolved?.id ?? null} size={12} class="opacity-90" />
      Open
    </button>
    {#if hasChoice}
      <button
        bind:this={menuTriggerEl}
        onclick={() => (showDropdown = !showDropdown)}
        aria-label="Open in a different editor"
        aria-expanded={showDropdown}
        aria-haspopup="menu"
        data-testid="chat-header-open-editor-caret"
        class="{SPLIT_BTN_BASE} rounded-r border-l-0 px-1"
      >
        <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-80" />
      </button>
    {/if}
  </div>

  {#if hasChoice}
    <Popover
      anchor={menuTriggerEl}
      open={showDropdown}
      onClose={closeMenu}
      placement="bottom-end"
      role="none"
    >
      {#snippet children()}
        <Menu ariaLabel="Open in editor" onClose={closeMenu} minWidthClass="min-w-[180px]">
          {#each available as editor (editor.id)}
            <MenuItem
              label={editor.name}
              checked={editor.id === resolved?.id}
              title={`Open ${name} in ${editor.name}`}
              onSelect={() => selectEditor(editor.id)}
            >
              {#snippet icon()}
                <EditorIcon editorId={editor.id} size={14} />
              {/snippet}
            </MenuItem>
          {/each}
        </Menu>
      {/snippet}
    </Popover>
  {/if}
{/if}
