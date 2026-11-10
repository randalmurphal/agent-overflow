// Run on a shell code block: runs the whole block as one composer shell
// command in the thread whose chat shows it, and sends the output to that
// thread's agent (stores/shellCommands.svelte.ts). Shared by the live code
// host and the settled block's static overlay so both decide the same way.
import { isShellRunActive, runShellCommand } from '../../../stores/shellCommands.svelte';
import { addErrorToast, addToast } from '../../../stores/toast.svelte';
import { threadHasScope } from '../../../transport/entityScopes';

export const CODE_RUN_LABEL = 'Run and send the output to the agent';

/**
 * Class of the Run control. It is hidden by default and shown inside a chat
 * surface (app.css), the one place a block has a thread to run in.
 */
export const CODE_RUN_CLASS = 'code-run-action';

const SHELL_LANGS = new Set(['sh', 'bash', 'zsh', 'shell']);

/** True for a fence whose info word names a shell this app can run. */
export function isRunnableShellLang(infoWord: string): boolean {
  return SHELL_LANGS.has(infoWord.toLowerCase());
}

/** The thread whose chat surface contains `origin`, or empty. */
function threadOf(origin: Element): string {
  return origin.closest<HTMLElement>('[data-ui-surface="chat"][data-thread-id]')?.dataset.threadId ?? '';
}

/** Runs a code block's text for the thread that shows it. */
export async function runCodeBlock(origin: Element, text: string): Promise<void> {
  const threadId = threadOf(origin);
  const command = text.trim();
  if (!threadId || !command) return;
  if (!threadHasScope('terminal:operate', threadId)) {
    addToast('warning', 'This session cannot run commands');
    return;
  }
  if (isShellRunActive(threadId)) {
    addToast('warning', 'A command is already running in this thread');
    return;
  }
  try {
    await runShellCommand(threadId, command);
  } catch (err) {
    addErrorToast('Failed to run the command', err);
  }
}
