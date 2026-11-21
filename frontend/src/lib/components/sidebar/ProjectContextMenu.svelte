<script lang="ts">
  import { projectHasScope } from '../../transport/entityScopes';
  // Popover-anchored Rename / Archive / Delete menu for a project row.
  // Extracted from ProjectItem.svelte so that file stays close to the
  // <= 250-line target. The confirm dialogs for Archive / Delete live
  // here too — Rename is the only action the parent still drives.
  //
  // Everything here acts on `target`, the project frozen at the gesture
  // that opened the menu (./sidebarEntries.ts). A row is keyed by its
  // repository and outlives a change of which checkout represents it, so a
  // menu reading a live row would archive or delete whichever checkout
  // represents the entry when its confirmation, preview or reply arrives.

  import type { ProjectTarget } from './sidebarEntries';
  import type { ProjectDeletionPreview } from '../../types/workflow';
  import {
    ArchiveProject,
    DeleteProject,
    ProjectDeletionPreview as fetchDeletionPreview,
  } from '../../stores/bindings';
  import { openInEditor } from '../../stores/openInEditor';
  import { removeProjectLocal } from '../../stores/projects.svelte';
  import { closePanesShowingThreads } from '../../stores/panes.svelte';
  import { removeThread } from '../../stores/threads.svelte';
  import { addErrorToast, addToast } from '../../stores/toast.svelte';
  import { retainedNotice } from '../../utils/projectCleanup';
  import { userFacingError } from '../../utils/userFacingError';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ProjectDeleteDialog from './ProjectDeleteDialog.svelte';
  import Popover from '../primitives/Popover.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import { hasScope } from '../../transport/scopes';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { newThreadGroupInProject } from './threadGroupActions';
  import { assistantsOn } from '../../stores/assistants.svelte';

  interface Props {
    target: ProjectTarget;
    anchor: HTMLElement | undefined;
    open: boolean;
    onClose: (reason?: PopoverCloseReason) => void;
    /** Fires when the user selects Rename from the menu — the parent row
     * owns the inline rename UI so the input can render in place of the
     * project name. */
    onRename: () => void;
    /** Compact only: the header's hover-revealed create controls, which the
     *  phone cannot reveal, so the menu carries them there. */
    onNewThread?: () => void;
    onNewAssistant?: () => void;
    onNewTerminal?: () => void;
  }

  let { target, anchor, open, onClose, onRename, onNewThread, onNewAssistant, onNewTerminal }: Props = $props();
  // The menu outlives a gesture: a second right-click on the row replaces
  // the target. What the menu DISPLAYS follows the current one; an action
  // takes the one in hand when it starts and never reads the prop again.
  let labelText = $derived(target.label);
  let newAssistantUngranted = $derived(!projectHasScope('threads:autonomy', target.id));
  let compact = $derived(isCompactLayout());
  // The same gates the header's own controls use: visible, inert, and saying why.
  // Rename, New Group, Archive and Delete all ride `threads:operate`; Delete
  // first asks ProjectDeletionPreview, which rides `git:operate`.
  let operateUngranted = $derived(!projectHasScope('threads:operate', target.id));
  let deleteUngranted = $derived(operateUngranted || !projectHasScope('git:operate', target.id));
  let ungrantedTitle = $derived(operateUngranted ? 'Not granted to this device' : undefined);
  let newTerminalUngranted = $derived(!projectHasScope('terminal:operate', target.id));
  // The one gated entry here opens an editor on the host desktop.
  let noHost = $derived(!hasScope('host', target.backend));

  let showArchiveConfirm = $state(false);
  let showDeleteConfirm = $state(false);
  // Set only when the project owns workflow work — that is the case with more
  // to say than the one-line confirm can carry.
  let deletionPreview = $state<ProjectDeletionPreview | null>(null);
  let deleting = $state(false);

  // An action's project is taken when the gesture that starts it happens,
  // and the action never reads the prop again: a confirmation, a preview or
  // a reply arrives after the row may have changed hands.
  let archiveTarget: ProjectTarget | null = $state(null);
  let deleteTarget: ProjectTarget | null = $state(null);

  async function doNewGroup(): Promise<void> {
    const project = target;
    await newThreadGroupInProject(project.id);
  }

  async function doArchive(): Promise<void> {
    const project = archiveTarget;
    if (project === null) return;
    try {
      await ArchiveProject(project.id);
      removeProjectLocal(project.id);
      addToast('info', `Archived project "${project.label}".`);
    } catch (err) {
      console.error('Failed to archive project:', err);
      addErrorToast(userFacingError(err), err);
    }
  }

  async function doOpenInEditor(): Promise<void> {
    if (noHost) return;
    try {
      // Project path is already absolute; workspacePath is unused.
      // Empty editorID → the user's default editor.
      const project = target;
      await openInEditor(project.backend, project.path, 0, 0, '', '');
    } catch (err) {
      addErrorToast(userFacingError(err), err);
    }
  }

  // Delete asks the backend what deletion involves before offering anything.
  // A preview that fails stops here rather than falling through to the plain
  // confirm: that dialog would describe a project with runs as if it had none.
  async function startDelete(): Promise<void> {
    const project = target;
    deleteTarget = project;
    try {
      const preview = await fetchDeletionPreview(project.id);
      if (preview.hasWork) {
        deletionPreview = preview;
      } else {
        showDeleteConfirm = true;
      }
    } catch (err) {
      console.error('Failed to inspect project before delete:', err);
      addErrorToast(
        userFacingError(err, 'Could not work out what deleting this project involves.'),
        err,
      );
    }
  }

  async function doDelete(): Promise<void> {
    const project = deleteTarget;
    if (project === null) return;
    deleting = true;
    try {
      const result = await DeleteProject(project.id);
      for (const id of result.threadIds) removeThread(id);
      removeProjectLocal(project.id);
      closePanesShowingThreads(result.threadIds);
      deletionPreview = null;
      addToast('info', `Deleted project "${project.label}".`);
      // A checkout git declined to remove is reported separately and stays on
      // screen as a warning: the deletion succeeded, but there is something on
      // disk the user has to finish themselves, and folding that into the
      // success line is how it goes unread.
      const notice = retainedNotice(result.retainedWorktrees);
      if (notice) addToast('warning', notice, 12000);
    } catch (err) {
      console.error('Failed to delete project:', err);
      addErrorToast(userFacingError(err), err);
    } finally {
      deleting = false;
    }
  }
</script>

<Popover
  {anchor}
  {open}
  {onClose}
  dismissOnAnchorClick
  placement="bottom-start"
  role="none"
>
  {#snippet children()}
    <Menu ariaLabel="Project Actions" {onClose}>
      {#snippet children()}
        {#if compact}
          <!-- Desktop reaches these from the header on hover; the phone has no
               hover, so the menu is where they live there, and only there. -->
          <MenuItem
            label="New Thread"
            disabled={operateUngranted}
            title={ungrantedTitle}
            onSelect={() => {
              onClose();
              onNewThread?.();
            }}
          />
          <MenuItem
            label="New Terminal"
            disabled={newTerminalUngranted}
            title={newTerminalUngranted ? 'Not granted to this device' : undefined}
            onSelect={() => {
              onClose();
              onNewTerminal?.();
            }}
          />
        {/if}
        {#if assistantsOn(target.backend)}
          <MenuItem
            label="New Assistant…"
            disabled={newAssistantUngranted}
            title={newAssistantUngranted ? 'Not granted to this device' : undefined}
            onSelect={() => {
              onClose();
              onNewAssistant?.();
            }}
          />
        {/if}
        <MenuDivider />
        <MenuItem
          label="Rename Project"
          disabled={operateUngranted}
          title={ungrantedTitle}
          onSelect={() => {
            onClose();
            onRename();
          }}
        />
        {#if !noHost}
          <MenuItem
            label="Open in Editor"
            onSelect={() => {
              onClose();
              void doOpenInEditor();
            }}
          />
        {/if}
        <MenuItem
          label="New Group…"
          disabled={operateUngranted}
          title={ungrantedTitle}
          onSelect={() => {
            onClose();
            void doNewGroup();
          }}
        />
        <MenuItem
          label="Archive Project"
          disabled={operateUngranted}
          title={ungrantedTitle}
          onSelect={() => {
            onClose();
            archiveTarget = target;
            showArchiveConfirm = true;
          }}
        />
        <MenuDivider />
        <MenuItem
          label="Delete Project"
          variant="danger"
          disabled={deleteUngranted}
          title={deleteUngranted ? 'Not granted to this device' : undefined}
          onSelect={() => {
            onClose();
            void startDelete();
          }}
        />
      {/snippet}
    </Menu>
  {/snippet}
</Popover>

<ConfirmDialog
  open={showArchiveConfirm}
  title="Archive Project"
  description={`Hide "${labelText}" from the sidebar. Threads remain and the project can be unarchived from Settings.`}
  confirmLabel="Archive"
  onConfirm={() => {
    showArchiveConfirm = false;
    void doArchive();
  }}
  onCancel={() => {
    showArchiveConfirm = false;
  }}
/>

<ConfirmDialog
  open={showDeleteConfirm}
  title="Delete Project"
  description={`Permanently delete "${labelText}" and all ${target.threadCount} thread${target.threadCount === 1 ? '' : 's'} it contains. This cannot be undone.`}
  confirmLabel="Delete"
  destructive={true}
  onConfirm={() => {
    showDeleteConfirm = false;
    void doDelete();
  }}
  onCancel={() => {
    showDeleteConfirm = false;
  }}
/>

{#if deletionPreview}
  <ProjectDeleteDialog
    open={true}
    projectName={labelText}
    threadCount={target.threadCount}
    preview={deletionPreview}
    submitting={deleting}
    onConfirm={() => {
      void doDelete();
    }}
    onCancel={() => {
      deletionPreview = null;
    }}
  />
{/if}
