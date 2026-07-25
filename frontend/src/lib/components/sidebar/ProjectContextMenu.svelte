<script lang="ts">
  // Popover-anchored Rename / Archive / Delete menu for a project row.
  // Extracted from ProjectItem.svelte so that file stays close to the
  // <= 250-line target. The confirm dialogs for Archive / Delete live
  // here too — Rename is the only action the parent still drives.

  import type { ProjectWithCounts } from '../../types/models';
  import type { ProjectDeletionPreview } from '../../types/workflow';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    ArchiveProject,
    DeleteProject,
    OpenInEditor,
    ProjectDeletionPreview as fetchDeletionPreview,
  } from '../../stores/bindings';
  import { removeProjectLocal } from '../../stores/projects.svelte';
  import { closePanesShowingThreads } from '../../stores/panes.svelte';
  import { removeThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { retainedNotice } from '../../utils/projectCleanup';
  import { userFacingError } from '../../utils/userFacingError';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ProjectDeleteDialog from './ProjectDeleteDialog.svelte';
  import Popover from '../primitives/Popover.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';

  interface Props {
    project: ProjectWithCounts;
    pane?: ThreadPane;
    anchor: HTMLElement | undefined;
    open: boolean;
    onClose: () => void;
    /** Fires when the user selects Rename from the menu — the parent row
     * owns the inline rename UI so the input can render in place of the
     * project name. */
    onRename: () => void;
  }

  let { project, pane, anchor, open, onClose, onRename }: Props = $props();

  let showArchiveConfirm = $state(false);
  let showDeleteConfirm = $state(false);
  // Set only when the project owns workflow work — that is the case with more
  // to say than the one-line confirm can carry.
  let deletionPreview = $state<ProjectDeletionPreview | null>(null);
  let deleting = $state(false);

  async function doArchive(): Promise<void> {
    try {
      await ArchiveProject(project.project.id);
      removeProjectLocal(project.project.id);
      addToast('info', `Archived project "${project.project.name}".`);
    } catch (err) {
      console.error('Failed to archive project:', err);
      addToast('error', userFacingError(err));
    }
  }

  async function doOpenInEditor(): Promise<void> {
    try {
      // Project path is already absolute; workspacePath is unused.
      // Empty editorID → the user's default editor.
      await OpenInEditor(project.project.path, 0, 0, '', '');
    } catch (err) {
      addToast('error', userFacingError(err));
    }
  }

  // Delete asks the backend what deletion involves before offering anything.
  // A preview that fails stops here rather than falling through to the plain
  // confirm: that dialog would describe a project with runs as if it had none.
  async function startDelete(): Promise<void> {
    try {
      const preview = await fetchDeletionPreview(project.project.id);
      if (preview.hasWork) {
        deletionPreview = preview;
      } else {
        showDeleteConfirm = true;
      }
    } catch (err) {
      console.error('Failed to inspect project before delete:', err);
      addToast(
        'error',
        userFacingError(err, 'Could not work out what deleting this project involves.'),
      );
    }
  }

  async function doDelete(): Promise<void> {
    deleting = true;
    try {
      const result = await DeleteProject(project.project.id);
      for (const id of result.threadIds) removeThread(id);
      removeProjectLocal(project.project.id);
      closePanesShowingThreads(result.threadIds);
      deletionPreview = null;
      addToast('info', `Deleted project "${project.project.name}".`);
      // A checkout git declined to remove is reported separately and stays on
      // screen as a warning: the deletion succeeded, but there is something on
      // disk the user has to finish themselves, and folding that into the
      // success line is how it goes unread.
      const notice = retainedNotice(result.retainedWorktrees);
      if (notice) addToast('warning', notice, 12000);
    } catch (err) {
      console.error('Failed to delete project:', err);
      addToast('error', userFacingError(err));
    } finally {
      deleting = false;
    }
  }
</script>

<Popover
  {anchor}
  {open}
  {onClose}
  placement="bottom-start"
  role="menu"
  ariaLabel="Project Actions"
>
  {#snippet children()}
    <Menu ariaLabel="Project Actions" {onClose}>
      {#snippet children()}
        <MenuItem
          label="Rename Project"
          onSelect={() => {
            onClose();
            onRename();
          }}
        />
        <MenuItem
          label="Open in Editor"
          onSelect={() => {
            onClose();
            void doOpenInEditor();
          }}
        />
        <MenuItem
          label="Archive Project"
          onSelect={() => {
            onClose();
            showArchiveConfirm = true;
          }}
        />
        <MenuDivider />
        <MenuItem
          label="Delete Project"
          variant="danger"
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
  description={`Hide "${project.project.name}" from the sidebar. Threads remain and the project can be unarchived from Settings.`}
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
  description={`Permanently delete "${project.project.name}" and all ${project.threadCount} thread${project.threadCount === 1 ? '' : 's'} it contains. This cannot be undone.`}
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
    projectName={project.project.name}
    threadCount={project.threadCount}
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
