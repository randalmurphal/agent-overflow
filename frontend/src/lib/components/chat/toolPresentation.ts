import type {
  CommandOutputMeta,
  DiffMeta,
  Item,
  ProposedPlanMeta,
  ToolResultMeta,
} from '../../types/models';
import { PROVIDER_DEFINITIONS } from '../../providers/catalog';
import { parseJsonObject } from '../../utils/parseJsonObject';
import { parsePatchFiles, type PatchFile, type PatchLine } from '../../utils/patchFiles';
import { isCodexCollabControlToolName } from './codexCollabControls';
import { commandTextForItem, isCommandToolName } from './commandDisplay';

export type ToolPresentationSurface = 'timeline' | 'tray';

export interface ToolPresentationInput {
  item: Item;
  provider?: string | null;
  surface?: ToolPresentationSurface;
  displayItem?: Item;
  statusItem?: Item;
  outputItem?: Item;
}

export type ToolPresentation =
  | { kind: 'user-input'; item: Item }
  | {
      kind: 'collab';
      item: Item;
      displayItem?: Item;
      statusItem?: Item;
    }
  | {
      kind: 'proposed-plan';
      item: Item;
      payloadId: string;
      meta: ProposedPlanMeta;
    }
  | {
      kind: 'single-file-diff';
      item: Item;
      payloadId?: string;
      file: PatchFile;
    }
  | {
      kind: 'diff-stack';
      item: Item;
      payloadId?: string;
      meta: ToolResultMeta;
    }
  | {
      kind: 'tool-result';
      item: Item;
      payloadId?: string;
      meta: ToolResultMeta;
    }
  | {
      kind: 'command';
      item: Item;
      displayItem?: Item;
      statusItem?: Item;
      payloadId?: string;
      meta: CommandOutputMeta;
      collapsedPreview: string;
    }
  | {
      kind: 'agent';
      item: Item;
      displayItem?: Item;
      statusItem?: Item;
    }
  | {
      kind: 'generic';
      item: Item;
      displayItem?: Item;
      statusItem?: Item;
    };

export type TimelineToolPresentation = Extract<
  ToolPresentation,
  | { kind: 'user-input' }
  | { kind: 'collab' }
  | { kind: 'proposed-plan' }
  | { kind: 'single-file-diff' }
  | { kind: 'diff-stack' }
  | { kind: 'tool-result' }
  | { kind: 'command' }
  | { kind: 'agent' }
  | { kind: 'generic' }
>;

export type TrayToolPresentation = Extract<
  ToolPresentation,
  { kind: 'collab' } | { kind: 'command' } | { kind: 'agent' } | { kind: 'generic' }
>;

export function resolveToolPresentation(
  input: ToolPresentationInput & { surface: 'tray' },
): TrayToolPresentation;
export function resolveToolPresentation(
  input: ToolPresentationInput & { surface?: 'timeline' },
): TimelineToolPresentation;
export function resolveToolPresentation(input: ToolPresentationInput): ToolPresentation {
  const surface = input.surface ?? 'timeline';
  if (surface === 'tray') {
    return resolveTrayToolPresentation(input);
  }
  return resolveTimelineToolPresentation(input);
}

function resolveTimelineToolPresentation(input: ToolPresentationInput): ToolPresentation {
  const item = input.item;
  const payloadId = item.payloadId;
  const payloadKind = item.payloadKind;

  if (isUserInputTool(item)) {
    return { kind: 'user-input', item };
  }

  if (isCollabControlTool(input.provider, item)) {
    return { kind: 'collab', item };
  }

  const planMeta = parsePayloadMeta<ProposedPlanMeta>(item, 'proposed_plan');
  if (planMeta && payloadId) {
    return {
      kind: 'proposed-plan',
      item,
      payloadId,
      meta: planMeta,
    };
  }

  const diffMeta = parsePayloadMeta<DiffMeta>(item, 'diff');
  const diffFile = diffMeta ? patchFileFromDiffMeta(diffMeta) : null;
  if (diffFile) {
    return {
      kind: 'single-file-diff',
      item,
      payloadId,
      file: diffFile,
    };
  }

  const toolResultMeta = parsePayloadMeta<ToolResultMeta>(item, 'tool_result', {
    requirePayloadId: false,
  });
  if (toolResultMeta?.inlineDiff?.files && toolResultMeta.inlineDiff.files.length > 0) {
    return {
      kind: 'diff-stack',
      item,
      payloadId,
      meta: toolResultMeta,
    };
  }
  if (toolResultMeta) {
    return {
      kind: 'tool-result',
      item,
      payloadId,
      meta: toolResultMeta,
    };
  }

  if (isCommandPresentation(item, item)) {
    return commandPresentation(item, item, item);
  }

  if (isClaudeAgentTool(item)) {
    return { kind: 'agent', item };
  }

  return {
    kind: 'generic',
    item,
  };
}

function resolveTrayToolPresentation(input: ToolPresentationInput): ToolPresentation {
  const displayItem = input.displayItem ?? input.item;
  const statusItem = input.statusItem ?? input.item;
  const outputItem = input.outputItem ?? input.item;

  if (isCommandPresentation(displayItem, outputItem)) {
    return commandPresentation(outputItem, displayItem, statusItem);
  }

  if (isCollabControlTool(input.provider, displayItem)) {
    return {
      kind: 'collab',
      item: displayItem,
      displayItem,
      statusItem,
    };
  }

  if (isClaudeAgentTool(displayItem)) {
    return {
      kind: 'agent',
      item: outputItem,
      displayItem,
      statusItem,
    };
  }

  return {
    kind: 'generic',
    item: outputItem,
    displayItem,
    statusItem,
  };
}

function commandPresentation(
  item: Item,
  displayItem: Item,
  statusItem: Item,
): Extract<ToolPresentation, { kind: 'command' }> {
  const parsedMeta = parseCommandOutputMeta(item);
  const meta: CommandOutputMeta = {
    command: commandTextForItem(displayItem, parsedMeta),
    lineCount: parsedMeta?.lineCount ?? 0,
    preview: parsedMeta?.preview,
    errorMessage: parsedMeta?.errorMessage,
  };
  if (parsedMeta?.exitCode !== undefined) {
    meta.exitCode = parsedMeta.exitCode;
  }
  return {
    kind: 'command',
    item,
    displayItem,
    statusItem,
    payloadId: item.payloadId,
    meta,
    collapsedPreview: collapsedCommandPreview(item, meta),
  };
}

function parsePayloadMeta<T>(
  item: Item,
  kind: string,
  opts: { requirePayloadId?: boolean } = {},
): T | null {
  if (item.payloadKind !== kind) {
    return null;
  }
  if (opts.requirePayloadId !== false && !item.payloadId) {
    return null;
  }
  return parseJsonObject(item.payloadMeta) as T | null;
}

function parseCommandOutputMeta(item: Item): CommandOutputMeta | null {
  if (item.payloadKind !== 'command_output') {
    return null;
  }
  const parsed = parseJsonObject(item.payloadMeta) as Partial<CommandOutputMeta> | null;
  if (!parsed) {
    return null;
  }
  return {
    command: typeof parsed.command === 'string' ? parsed.command : '',
    exitCode: typeof parsed.exitCode === 'number' ? parsed.exitCode : undefined,
    lineCount: typeof parsed.lineCount === 'number' ? parsed.lineCount : 0,
    preview: typeof parsed.preview === 'string' ? parsed.preview : undefined,
    errorMessage: typeof parsed.errorMessage === 'string' ? parsed.errorMessage : undefined,
  };
}

function patchFileFromDiffMeta(diffMeta: DiffMeta): PatchFile {
  const parsed = parsePatchFiles(diffMeta.preview);
  if (parsed.length > 0 && parsed[0]) {
    return parsed[0];
  }
  return {
    path: diffMeta.filePath,
    kind: diffMeta.changeKind,
    additions: diffMeta.insertions,
    deletions: diffMeta.deletions,
    lines: [] as PatchLine[],
  };
}

function isCommandPresentation(displayItem: Item, outputItem: Item): boolean {
  const summaryToolName = displayItem.summary?.split(':', 1)[0];
  return (
    outputItem.payloadKind === 'command_output' ||
    isCommandToolName(displayItem.toolName) ||
    isCommandToolName(summaryToolName)
  );
}

function isUserInputTool(item: Item): boolean {
  return item.toolName === 'AskUserQuestion' || item.toolName === 'request_user_input';
}

function isClaudeAgentTool(item: Item): boolean {
  return item.toolName === 'Agent' || item.toolName === 'Task';
}

function isCollabControlTool(provider: string | null | undefined, item: Item): boolean {
  return (
    provider === PROVIDER_DEFINITIONS.codex.id &&
    isCodexCollabControlToolName(item.toolName?.trim())
  );
}

function collapsedCommandPreview(item: Item, meta: CommandOutputMeta): string {
  if (item.kind !== 'tool_completion' || !item.completionOf) {
    return '';
  }
  const parsed = parseJsonObject(item.meta);
  const carrierID = parsed?.wait_carrier_id ?? parsed?.waitCarrierID;
  if (typeof carrierID !== 'string' || !carrierID.trim()) {
    return '';
  }
  return meta.preview ?? '';
}
