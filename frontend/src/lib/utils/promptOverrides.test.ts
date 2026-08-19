import { describe, expect, it } from 'vitest';
import {
  CLAUDE_TODO_TOOL_GROUP,
  CLAUDE_TOOL_SUGGESTIONS,
  CODEX_TOOL_TOGGLES,
  disabledToolNameError,
  disabledToolsFor,
  disabledToolsSettingsKey,
  entryInertness,
  exposedTodoTools,
  isTodoGroupTool,
  insertAtSelection,
  MAX_DISABLED_TOOL_BYTES,
  newPromptOverride,
  placeholdersFor,
  PROMPT_PLACEHOLDERS,
  promptOverridesFor,
  promptOverridesSettingsKey,
  shadowedModels,
  toggleModelSelection,
  unknownCodexToggleIds,
  withEntryAdded,
  withEntryPatch,
  withEntryRemoved,
  withToolAdded,
  withToolRemoved,
  withToolsAdded,
  withToolsRemoved,
} from './promptOverrides';
import { makeSettings } from '../../test/helpers/settings';
import type { PromptOverride } from '../types/settings';

// The default prompt is deliberately NON-blank: Match skips a blank one, so
// an entry with an empty prompt is inert and a fixture built on it would be
// asking a different question than the one under test.
const ENTRY = (over: Partial<PromptOverride> = {}): PromptOverride => ({
  enabled: true,
  models: [],
  prompt: 'p',
  ...over,
});

describe('promptOverrides settings routing', () => {
  it('shares the claude keys with claude-tui and routes codex separately', () => {
    expect(promptOverridesSettingsKey('claude')).toBe('claudePromptOverrides');
    expect(promptOverridesSettingsKey('codex')).toBe('codexPromptOverrides');
    // Mirrors internal/settings: one binary, and the interactive TUI honors
    // --system-prompt-file and --disallowedTools the same way headless does,
    // so a claude-tui pane edits the Claude lists.
    expect(promptOverridesSettingsKey('claude-tui')).toBe('claudePromptOverrides');
    expect(promptOverridesSettingsKey('nope')).toBeNull();

    expect(disabledToolsSettingsKey('claude')).toBe('claudeDisabledTools');
    expect(disabledToolsSettingsKey('codex')).toBe('codexDisabledTools');
    expect(disabledToolsSettingsKey('claude-tui')).toBe('claudeDisabledTools');
    expect(disabledToolsSettingsKey('nope')).toBeNull();
  });

  it('reads an unset list as empty', () => {
    const settings = makeSettings();
    expect(promptOverridesFor(settings, 'claude')).toEqual([]);
    expect(disabledToolsFor(settings, 'codex')).toEqual([]);
    expect(promptOverridesFor(settings, 'nope')).toEqual([]);
  });

  it('reads the configured lists', () => {
    const entry = ENTRY({ models: ['claude-fable-5'], prompt: 'hi' });
    const settings = makeSettings({
      claudePromptOverrides: [entry],
      codexDisabledTools: ['web_search'],
    });
    expect(promptOverridesFor(settings, 'claude')).toEqual([entry]);
    expect(promptOverridesFor(settings, 'claude-tui')).toEqual([entry]);
    expect(promptOverridesFor(settings, 'nope')).toEqual([]);
    expect(disabledToolsFor(settings, 'codex')).toEqual(['web_search']);
  });
});

describe('prompt override list edits', () => {
  it('adds a disabled empty entry without touching the others', () => {
    const list = [ENTRY({ prompt: 'first' })];
    const next = withEntryAdded(list);
    expect(next).toHaveLength(2);
    expect(next[0]).toEqual(list[0]);
    expect(next[0]).not.toBe(list[0]);
    expect(next[1]).toEqual(newPromptOverride());
  });

  it('patches one entry and leaves the array immutable', () => {
    const list = [ENTRY({ prompt: 'a' }), ENTRY({ prompt: 'b' })];
    const next = withEntryPatch(list, 1, { enabled: false });
    expect(next[1]).toEqual({ enabled: false, models: [], prompt: 'b' });
    expect(list[1].enabled).toBe(true);
  });

  it('ignores a patch for an index that does not exist', () => {
    const list = [ENTRY({ prompt: 'a' })];
    expect(withEntryPatch(list, 4, { prompt: 'x' })).toEqual(list);
    expect(withEntryPatch(list, -1, { prompt: 'x' })).toEqual(list);
  });

  it('removes by index', () => {
    const list = [ENTRY({ prompt: 'a' }), ENTRY({ prompt: 'b' })];
    expect(withEntryRemoved(list, 0)).toEqual([list[1]]);
  });

  it('toggles a model in selection order and back out', () => {
    const once = toggleModelSelection([], 'a');
    expect(once).toEqual(['a']);
    expect(toggleModelSelection(once, 'b')).toEqual(['a', 'b']);
    expect(toggleModelSelection(['a', 'b'], 'a')).toEqual(['b']);
  });
});

describe('shadowedModels', () => {
  it('reports models an earlier enabled entry already claims', () => {
    const list = [
      ENTRY({ models: ['m1', 'm2'] }),
      ENTRY({ models: ['m2', 'm3'] }),
    ];
    expect(shadowedModels(list, 0)).toEqual([]);
    expect(shadowedModels(list, 1)).toEqual(['m2']);
  });

  it('ignores disabled entries on both sides', () => {
    const list = [
      ENTRY({ enabled: false, models: ['m1'] }),
      ENTRY({ models: ['m1'] }),
      ENTRY({ enabled: false, models: ['m1'] }),
    ];
    expect(shadowedModels(list, 1)).toEqual([]);
    expect(shadowedModels(list, 2)).toEqual([]);
  });

  it('ignores an earlier entry whose prompt is blank, as Match does', () => {
    const list = [
      ENTRY({ models: ['m'], prompt: '' }),
      ENTRY({ models: ['m'], prompt: 'x' }),
    ];
    expect(shadowedModels(list, 1)).toEqual([]);
  });

  it('treats a whitespace-only earlier prompt as blank', () => {
    const list = [
      ENTRY({ models: ['m'], prompt: '  \n ' }),
      ENTRY({ models: ['m'], prompt: 'x' }),
    ];
    expect(shadowedModels(list, 1)).toEqual([]);
  });
});

describe('entryInertness', () => {
  it('reports an enabled entry with no models', () => {
    expect(entryInertness(ENTRY({ models: [] }))).toEqual({
      noModels: true,
      noPrompt: false,
    });
  });

  it('reports an enabled entry whose prompt is blank after trimming', () => {
    expect(entryInertness(ENTRY({ models: ['m'], prompt: '   ' }))).toEqual({
      noModels: false,
      noPrompt: true,
    });
  });

  it('reports both causes independently', () => {
    expect(entryInertness(ENTRY({ models: [], prompt: '' }))).toEqual({
      noModels: true,
      noPrompt: true,
    });
  });

  it('stays silent about a disabled entry, however empty', () => {
    expect(entryInertness(ENTRY({ enabled: false, models: [], prompt: '' }))).toEqual({
      noModels: false,
      noPrompt: false,
    });
  });

  it('says nothing about a usable entry', () => {
    expect(entryInertness(ENTRY({ models: ['m'], prompt: 'x' }))).toEqual({
      noModels: false,
      noPrompt: false,
    });
  });
});

describe('disabled tool list edits', () => {
  it('adds a trimmed name once and removes it', () => {
    expect(withToolAdded([], '  Workflow ')).toEqual(['Workflow']);
    expect(withToolAdded(['Workflow'], 'Workflow')).toEqual(['Workflow']);
    expect(withToolAdded(['Workflow'], '   ')).toEqual(['Workflow']);
    expect(withToolRemoved(['Workflow', 'Agent'], 'Workflow')).toEqual(['Agent']);
  });

  it('keeps case, because CLI tool names are case-sensitive', () => {
    expect(withToolAdded(['Workflow'], 'workflow')).toEqual(['Workflow', 'workflow']);
  });

  it('adds and removes a whole group without duplicating existing entries', () => {
    expect(withToolsAdded(['Workflow', 'TaskGet'], CLAUDE_TODO_TOOL_GROUP)).toEqual([
      'Workflow',
      'TaskGet',
      'TodoWrite',
      'TaskCreate',
      'TaskUpdate',
      'TaskList',
    ]);
    expect(
      withToolsRemoved(
        ['Workflow', 'TodoWrite', 'TaskCreate', 'TaskUpdate', 'TaskGet', 'TaskList'],
        CLAUDE_TODO_TOOL_GROUP,
      ),
    ).toEqual(['Workflow']);
  });
});

describe('todo tool group', () => {
  it('projects group membership over the flat list', () => {
    expect(exposedTodoTools([])).toEqual([...CLAUDE_TODO_TOOL_GROUP]);
    expect(exposedTodoTools(['Workflow', 'TaskGet'])).toEqual([
      'TodoWrite',
      'TaskCreate',
      'TaskUpdate',
      'TaskList',
    ]);
    expect(exposedTodoTools([...CLAUDE_TODO_TOOL_GROUP])).toEqual([]);
    expect(isTodoGroupTool('TaskCreate')).toBe(true);
    expect(isTodoGroupTool('Workflow')).toBe(false);
  });

  it('keeps the group out of the flat suggestion row', () => {
    for (const name of CLAUDE_TODO_TOOL_GROUP) {
      expect(CLAUDE_TOOL_SUGGESTIONS).not.toContain(name);
    }
  });
});

describe('disabledToolNameError', () => {
  it('accepts an ordinary name, and says nothing about a blank draft', () => {
    expect(disabledToolNameError('MyCustomTool')).toBeNull();
    expect(disabledToolNameError('  Workflow  ')).toBeNull();
    expect(disabledToolNameError('   ')).toBeNull();
  });

  it('refuses a leading dash the CLI would read as a flag', () => {
    expect(disabledToolNameError('-Workflow')).toContain('start with');
    // Trimmed first, so a padded one is refused for the same reason.
    expect(disabledToolNameError('  -Workflow')).toContain('start with');
  });

  it('refuses inner whitespace, including what JS `\\s` alone would miss', () => {
    expect(disabledToolNameError('Web Search')).toContain('whitespace');
    expect(disabledToolNameError('Web\tSearch')).toContain('whitespace');
    // U+0085 (NEL) is whitespace to Go's unicode.IsSpace but not to `\s`;
    // letting it through would fail the save with a generic toast.
    expect(disabledToolNameError('Web\u0085Search')).toContain('whitespace');
  });

  it('refuses a name past the byte cap the backend enforces', () => {
    expect(disabledToolNameError('a'.repeat(MAX_DISABLED_TOOL_BYTES))).toBeNull();
    expect(disabledToolNameError('a'.repeat(MAX_DISABLED_TOOL_BYTES + 1))).toContain(
      'limited to',
    );
    // Counted in BYTES, as Go does: 64 four-byte runes is over the cap.
    expect(disabledToolNameError('𝔘'.repeat(43))).toContain('limited to');
  });
});

describe('unknownCodexToggleIds', () => {
  it('returns nothing when every stored id is curated', () => {
    expect(unknownCodexToggleIds(['web_search', 'update_plan'])).toEqual([]);
    expect(unknownCodexToggleIds([])).toEqual([]);
  });

  it('returns the uncurated ids in stored order', () => {
    expect(
      unknownCodexToggleIds(['zzz_future', 'web_search', 'aaa_typo']),
    ).toEqual(['zzz_future', 'aaa_typo']);
  });
});

describe('placeholders', () => {
  it('offers the memory dir to Claude only', () => {
    const claude = placeholdersFor('claude').map((p) => p.token);
    const codex = placeholdersFor('codex').map((p) => p.token);
    expect(claude).toContain('{{MEMORY_DIR}}');
    expect(codex).not.toContain('{{MEMORY_DIR}}');
    expect(codex).toContain('{{WORKDIR}}');
    expect(claude).toHaveLength(PROMPT_PLACEHOLDERS.length);
  });

  it('inserts at the caret and reports where the caret lands', () => {
    expect(insertAtSelection('AB', 1, 1, '{{X}}')).toEqual({
      text: 'A{{X}}B',
      caret: 6,
    });
  });

  it('replaces a selection', () => {
    expect(insertAtSelection('hello world', 0, 5, '{{X}}')).toEqual({
      text: '{{X}} world',
      caret: 5,
    });
  });

  it('clamps offsets outside the text', () => {
    expect(insertAtSelection('ab', 9, 9, 'X')).toEqual({ text: 'abX', caret: 3 });
    expect(insertAtSelection('ab', -3, 1, 'X')).toEqual({ text: 'Xb', caret: 1 });
  });
});

describe('codex toggle ids', () => {
  // The ids are a locked contract with the backend's config mapping, not
  // display strings — a rename here silently stops disabling the tool.
  it('is exactly the curated set', () => {
    expect(CODEX_TOOL_TOGGLES.map((t) => t.id)).toEqual([
      'web_search',
      'update_plan',
      'view_image',
      'request_user_input',
      'collab_agents',
      'image_generation',
      'tool_suggest',
    ]);
  });
});
