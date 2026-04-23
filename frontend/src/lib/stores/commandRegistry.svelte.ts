// Command registry: the single source of truth for palette-visible and
// keybinding-addressable actions.
//
// A Command encapsulates the metadata the palette needs (id, label, icon,
// optional `when` predicate) plus the mutation the app performs when the
// command runs. Commands consume a CommandContext that the host (App.svelte)
// assembles on each invocation so the callback sees the current pane / thread.
//
// The registry is intentionally dumb: it's a plain Svelte-5 $state map whose
// accessors are pure. Context evaluation, keybinding resolution, and
// when-expression gating all live in keybindings.svelte.ts; palette UI lives
// in CommandPalette.svelte.

import type { WhenNode } from './keybindingParser';
import { evaluateWhen, tryParseWhen } from './keybindingParser';

export interface CommandContext {
  /** True while the command palette is open. */
  paletteOpen: boolean;
  /** True while the active pane has the terminal drawer open. */
  terminalOpen: boolean;
  /** True when keyboard focus is inside the terminal. */
  terminalFocus: boolean;
  /** True while there is at least one pending approval prompt. */
  approvalPending: boolean;
  /** True when any modal / confirm dialog is currently visible. */
  anyModalOpen: boolean;
  /** Whether there is an active thread. */
  hasActiveThread: boolean;
  /** True while the active thread has a live provider turn. */
  turnActive: boolean;
  /** Thread metadata fields used by fork / discussion commands. */
  canForkActiveThread: boolean;
  canStartDiscussion: boolean;
  /** Extra identifiers callers want to expose to `when` expressions. */
  [key: string]: boolean;
}

export interface Command {
  id: string;
  label: string;
  description?: string;
  icon?: string;
  /** When expression (parsed when the command is registered; see below). */
  when?: string;
  /** Runs the command. Receives the live context at invocation time. */
  run: (ctx: CommandContext) => void | Promise<void>;
}

interface RegisteredCommand extends Command {
  whenAst: WhenNode | null;
}

const commands: Map<string, RegisteredCommand> = $state(new Map());

export function registerCommand(cmd: Command): void {
  const whenAst = cmd.when ? tryParseWhen(cmd.when) : null;
  commands.set(cmd.id, { ...cmd, whenAst });
}

export function unregisterCommand(id: string): void {
  commands.delete(id);
}

export function listCommands(): Command[] {
  return Array.from(commands.values());
}

export function getCommand(id: string): Command | undefined {
  return commands.get(id);
}

/**
 * Returns the commands whose `when` expression evaluates true in the current
 * context. Commands without a `when` are always enabled.
 */
export function enabledCommands(ctx: CommandContext): Command[] {
  const out: Command[] = [];
  for (const cmd of commands.values()) {
    if (!cmd.whenAst) {
      out.push(cmd);
      continue;
    }
    if (evaluateWhen(cmd.whenAst, ctx)) out.push(cmd);
  }
  return out;
}

export function isCommandEnabled(id: string, ctx: CommandContext): boolean {
  const cmd = commands.get(id);
  if (!cmd) return false;
  if (!cmd.whenAst) return true;
  return evaluateWhen(cmd.whenAst, ctx);
}

/**
 * Execute a command by id. Returns false when the id is not registered or
 * the command is disabled by its when-expression; true when the run callback
 * was invoked (errors from within the callback propagate to the caller).
 */
export function runCommand(id: string, ctx: CommandContext): boolean {
  const cmd = commands.get(id);
  if (!cmd) return false;
  if (cmd.whenAst && !evaluateWhen(cmd.whenAst, ctx)) return false;
  void cmd.run(ctx);
  return true;
}

export function clearCommandRegistry(): void {
  commands.clear();
}
