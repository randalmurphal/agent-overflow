// Save-dialog controller for proposed plans. Both the inline
// ProposedPlanCard and the PlanSidebar mount their own save buttons
// (each with its own modal lifetime), so each gets its own controller
// instance via this factory — runes inside the factory back the
// per-instance reactive state.
//
// Copy is handled by <CopyButton>, which manages its own copied/timer
// state. The factory exposes `getCopyableMarkdown` so the button can
// fetch + normalize on click.

import { addToast } from '../stores/toast.svelte';
import { WriteWorkspaceFile } from '../stores/bindings';
import type { WorkspaceRef } from '../types/git';
import { buildProposedPlanMarkdownFilename, normalizePlanMarkdownForExport } from './proposedPlan';

export interface PlanSaveDialog {
  readonly savePath: string;
  readonly saving: boolean;
  readonly saveDialogOpen: boolean;
  getCopyableMarkdown(): Promise<string>;
  openSaveDialog(): Promise<void>;
  handleSave(): Promise<void>;
  closeSaveDialog(): void;
  setSavePath(value: string): void;
}

export function createPlanSaveDialog(
  getMarkdown: () => Promise<string>,
  // The CHECKOUT the plan file lands in (`pane.workspace`). Read at save
  // time, not at construction: a draft placeholder's checkout can change
  // before the user clicks save.
  getWorkspace: () => WorkspaceRef | null,
): PlanSaveDialog {
  let savePath = $state('');
  let saving = $state(false);
  let saveDialogOpen = $state(false);

  async function getCopyableMarkdown(): Promise<string> {
    const markdown = await getMarkdown();
    if (!markdown) return '';
    return normalizePlanMarkdownForExport(markdown);
  }

  async function openSaveDialog(): Promise<void> {
    const markdown = await getMarkdown();
    if (!markdown) return;
    savePath = savePath.trim() || buildProposedPlanMarkdownFilename(markdown);
    saveDialogOpen = true;
  }

  async function handleSave(): Promise<void> {
    if (saving) return;
    const workspace = getWorkspace();
    if (!workspace) {
      // A plan only ever arrives on a project-bound thread, so this is a
      // bug upstream, not a state to absorb silently.
      addToast('error', 'This thread names no workspace to save the plan into');
      return;
    }
    const relativePath = savePath.trim();
    if (!relativePath) {
      addToast('warning', 'Enter a workspace-relative path');
      return;
    }
    const markdown = await getMarkdown();
    if (!markdown) return;

    saving = true;
    try {
      const writtenPath = await WriteWorkspaceFile(
        workspace,
        relativePath,
        normalizePlanMarkdownForExport(markdown),
      );
      addToast('success', `Plan saved to ${writtenPath}`);
      saveDialogOpen = false;
    } catch (err) {
      console.error('Failed to save proposed plan:', err);
      addToast('error', err instanceof Error ? err.message : 'Failed to save plan');
    } finally {
      saving = false;
    }
  }

  function closeSaveDialog(): void {
    // Don't close mid-save — the RPC is still in flight and the modal needs
    // to stay open until the write resolves.
    if (saving) return;
    saveDialogOpen = false;
  }

  function setSavePath(value: string): void {
    savePath = value;
  }

  return {
    get savePath() { return savePath; },
    get saving() { return saving; },
    get saveDialogOpen() { return saveDialogOpen; },
    getCopyableMarkdown,
    openSaveDialog,
    handleSave,
    closeSaveDialog,
    setSavePath,
  };
}
