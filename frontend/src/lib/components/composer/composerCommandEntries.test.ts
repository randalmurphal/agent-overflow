import { describe, expect, it } from 'vitest';
import {
  buildCommandSections,
  filterCommandSections,
  flattenSections,
  interceptedCommandNames,
  interceptedCommandsFor,
  mergeStaticClaudeCommands,
  unionProviderCommands,
} from './composerCommandEntries';
import type { SlashCommand } from '../../stores/bindings';

function cmd(name: string, description?: string, argumentHint?: string): SlashCommand {
  return { name, description, argumentHint } as SlashCommand;
}

describe('unionProviderCommands', () => {
  it('is the probe list alone until a session frame arrives', () => {
    const probe = [cmd('usage', 'Show usage')];
    expect(unionProviderCommands(null, probe)).toEqual(probe);
  });

  it('is empty when neither source has answered', () => {
    expect(unionProviderCommands(null, null)).toEqual([]);
  });

  it('makes the session frame’s NAME SET authoritative once it arrives', () => {
    const session = [cmd('usage'), cmd('mcp__linear__issue')];
    const probe = [cmd('usage', 'Show usage'), cmd('compact', 'Compact now')];
    const union = unionProviderCommands(session, probe);
    // `compact` is in the probe list but not this session's — dropped, because
    // the session is the answer for THIS thread.
    expect(union.map((c) => c.name)).toEqual(['usage', 'mcp__linear__issue']);
  });

  it('enriches matching names from the probe list without overwriting the frame', () => {
    const session = [cmd('usage'), cmd('context', 'From the wire')];
    const probe = [
      cmd('usage', 'Show usage', '[period]'),
      cmd('context', 'From the probe', '[hint]'),
    ];
    const union = unionProviderCommands(session, probe);
    // `system/init` reports names only, so an entry with no description takes
    // the probe's...
    expect(union[0]).toMatchObject({ description: 'Show usage', argumentHint: '[period]' });
    // ...while one the wire described keeps its own text.
    expect(union[1].description).toBe('From the wire');
    expect(union[1].argumentHint).toBe('[hint]');
  });

  it('treats an empty session frame as a real answer, not as unknown', () => {
    expect(unionProviderCommands([], [cmd('usage')])).toEqual([]);
  });
});

describe('mergeStaticClaudeCommands', () => {
  it('stays null when nothing has answered', () => {
    expect(mergeStaticClaudeCommands(null, [])).toBeNull();
  });

  it('forms a real list from skills alone so a cold menu can show them', () => {
    expect(mergeStaticClaudeCommands(null, [{ name: 'commit-helper', description: 'Commit' }]))
      .toEqual([{ name: 'commit-helper', description: 'Commit', argumentHint: '' }]);
  });

  it('appends skills after the probe list, defaulting a missing description', () => {
    const merged = mergeStaticClaudeCommands([cmd('usage', 'Show usage')], [{ name: 'deploy' }]);
    expect(merged).toEqual([
      cmd('usage', 'Show usage'),
      { name: 'deploy', description: '', argumentHint: '' },
    ]);
  });

  it('lets a probe entry win a name collision — it is the CLI’s own answer', () => {
    const merged = mergeStaticClaudeCommands(
      [cmd('deploy', 'From the CLI')],
      [{ name: 'deploy', description: 'From the filesystem' }],
    );
    expect(merged).toEqual([cmd('deploy', 'From the CLI')]);
  });
});

describe('interceptedCommandsFor', () => {
  it('exposes the provider-agnostic reroutes everywhere', () => {
    const names = interceptedCommandNames('claude');
    expect([...names].sort()).toEqual(['clear', 'config', 'effort', 'fast', 'model', 'rename']);
  });

  it('adds Codex’s own two only on a Codex thread', () => {
    const names = interceptedCommandNames('codex');
    expect(names.has('compact')).toBe(true);
    expect(names.has('review')).toBe(true);
  });

  it('offers nothing provider-scoped when the thread has no provider yet', () => {
    expect(interceptedCommandsFor('').map((c) => c.name)).not.toContain('review');
  });
});

describe('buildCommandSections', () => {
  const base = {
    provider: 'claude',
    atStart: true,
    sessionCommands: null,
    probeCommands: null,
    skills: [],
    claudeSkills: [],
  };

  it('offers only AO commands away from the start of the draft', () => {
    const sections = buildCommandSections({
      ...base,
      atStart: false,
      probeCommands: [cmd('usage')],
    });
    expect(sections.map((s) => s.id)).toEqual(['ao']);
    expect(flattenSections(sections).map((e) => e.name)).toEqual(['workflow']);
  });

  it('adds intercepted and provider sections at the start of the draft', () => {
    const sections = buildCommandSections({ ...base, probeCommands: [cmd('usage', 'Show usage')] });
    expect(sections.map((s) => s.id)).toEqual(['ao', 'provider']);
    const names = flattenSections(sections).map((e) => e.name);
    expect(names).toContain('workflow');
    expect(names).toContain('model');
    expect(names).toContain('usage');
  });

  // A Claude session frame enumerates commands from several scopes at once
  // (user, project, plugin, MCP prompt) and reports one row per scope, so one
  // name can arrive twice. Two rows sharing a `label` is what the popover's
  // keyed `{#each}` throws `each_key_duplicate` on, and a throw inside an
  // update flush aborts the batch and freezes the pane (incident 2026-08-29).
  it('emits one row when a session frame reports the same command name twice', () => {
    const sections = buildCommandSections({
      ...base,
      sessionCommands: [cmd('review', 'Project scope'), cmd('review', 'User scope'), cmd('usage')],
    });
    const labels = flattenSections(sections).map((e) => e.label);
    expect(labels.filter((label) => label === '/review')).toEqual(['/review']);
    expect(new Set(labels).size).toBe(labels.length);
    // First occurrence wins, so source order and the priority it encodes hold.
    const review = flattenSections(sections).find((e) => e.label === '/review');
    expect(review?.description).toBe('Project scope');
  });

  it('emits one row when the probe list alone repeats a name', () => {
    const sections = buildCommandSections({
      ...base,
      probeCommands: [cmd('context'), cmd('context')],
    });
    const labels = flattenSections(sections).map((e) => e.label);
    expect(new Set(labels).size).toBe(labels.length);
  });

  it('emits one row when two Codex skills share a name', () => {
    const sections = buildCommandSections({
      ...base,
      provider: 'codex',
      skills: [
        { name: 'review-code', description: 'workspace' },
        { name: 'review-code', description: 'global' },
      ],
    });
    const labels = flattenSections(sections).map((e) => e.label);
    expect(labels.filter((label) => label === '$review-code')).toEqual(['$review-code']);
    expect(new Set(labels).size).toBe(labels.length);
  });

  // Static-data tripwire: nothing stops an AO command and an intercepted one
  // from being given the same word, and the 'ao' section renders both.
  it('mints unique labels within every section it can build', () => {
    for (const provider of ['claude', 'codex']) {
      const sections = buildCommandSections({ ...base, provider });
      for (const section of sections) {
        const labels = section.entries.map((e) => e.label);
        expect(new Set(labels).size, `${provider}/${section.id}`).toBe(labels.length);
      }
    }
  });

  it('shadows a provider command whose name AO intercepts', () => {
    const sections = buildCommandSections({
      ...base,
      probeCommands: [cmd('model', 'Claude’s own model picker'), cmd('usage')],
    });
    const rows = flattenSections(sections).filter((e) => e.name === 'model');
    // Exactly one `/model` row, and it is the app-side reroute.
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe('intercepted');
  });

  it('lists Codex skills with their $ token and marks disabled ones', () => {
    const sections = buildCommandSections({
      ...base,
      provider: 'codex',
      skills: [
        { name: 'review-code', shortDescription: 'Careful review', enabled: true },
        { name: 'legacy', description: 'Old one', enabled: false },
      ],
    });
    expect(sections.map((s) => s.id)).toEqual(['ao', 'skills']);
    const skills = sections.find((s) => s.id === 'skills')!.entries;
    expect(skills[0]).toMatchObject({
      label: '$review-code',
      insertText: '$review-code ',
      description: 'Careful review',
      disabled: false,
    });
    expect(skills[1]).toMatchObject({ label: '$legacy', disabled: true });
    expect(skills[1].disabledReason).toBeTruthy();
  });

  it('hides CLI-terminal-UI commands and dunder internals from the menu only', () => {
    const sections = buildCommandSections({
      ...base,
      probeCommands: [
        cmd('usage', 'Show usage'),
        cmd('color', 'Change the theme'),
        cmd('agents', 'Manage agent configurations'),
        cmd('extra-usage', 'Toggle extra usage'),
        cmd('__internal', 'Reserved'),
      ],
    });
    const names = flattenSections(sections).map((e) => e.name);
    expect(names).toContain('usage');
    for (const hidden of ['color', 'agents', 'extra-usage', '__internal']) {
      expect(names).not.toContain(hidden);
    }
  });

  it('shows filesystem-enumerated Claude skills as provider rows pre-session', () => {
    const sections = buildCommandSections({
      ...base,
      claudeSkills: [{ name: 'commit-helper', description: 'Commit like a pro' }],
    });
    const provider = sections.find((s) => s.id === 'provider')!;
    expect(provider.entries).toHaveLength(1);
    expect(provider.entries[0]).toMatchObject({
      kind: 'provider',
      label: '/commit-helper',
      insertText: '/commit-helper ',
      description: 'Commit like a pro',
    });
  });

  it('drops a skill the live session frame does not list — the frame is authoritative', () => {
    const sections = buildCommandSections({
      ...base,
      sessionCommands: [cmd('usage')],
      claudeSkills: [{ name: 'commit-helper', description: 'Commit like a pro' }],
    });
    const names = flattenSections(sections).map((e) => e.name);
    expect(names).toContain('usage');
    expect(names).not.toContain('commit-helper');
  });

  it('shadows a skill whose name AO intercepts', () => {
    const sections = buildCommandSections({
      ...base,
      claudeSkills: [{ name: 'model', description: 'A skill unluckily named model' }],
    });
    const rows = flattenSections(sections).filter((e) => e.name === 'model');
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe('intercepted');
  });

  it('never shows provider commands on a Codex thread', () => {
    const sections = buildCommandSections({
      ...base,
      provider: 'codex',
      probeCommands: [cmd('usage')],
    });
    expect(sections.map((s) => s.id)).not.toContain('provider');
  });
});

describe('filterCommandSections', () => {
  const sections = [
    {
      id: 'provider',
      header: 'Provider commands',
      entries: [
        { kind: 'provider' as const, name: 'context', label: '/context', insertText: '/context ' },
        { kind: 'provider' as const, name: 'usage', label: '/usage', insertText: '/usage ' },
        {
          kind: 'provider' as const,
          name: 'mcp__linear__create_issue',
          label: '/mcp__linear__create_issue',
          insertText: '/mcp__linear__create_issue ',
        },
      ],
    },
  ];

  it('keeps source order for an empty query', () => {
    expect(flattenSections(filterCommandSections(sections, '')).map((e) => e.name)).toEqual([
      'context',
      'usage',
      'mcp__linear__create_issue',
    ]);
  });

  it('matches substrings so a long provider name is reachable from its middle', () => {
    expect(flattenSections(filterCommandSections(sections, 'linear')).map((e) => e.name)).toEqual([
      'mcp__linear__create_issue',
    ]);
  });

  it('ranks prefix matches ahead of interior ones', () => {
    const names = flattenSections(filterCommandSections(sections, 'c')).map((e) => e.name);
    expect(names[0]).toBe('context');
    expect(names).toContain('mcp__linear__create_issue');
  });

  it('drops a section that matched nothing', () => {
    expect(filterCommandSections(sections, 'zzz')).toEqual([]);
  });

  it('matches a row’s searchText when it has one', () => {
    const withSearch = [
      {
        id: 'review-commits',
        header: 'Commit',
        entries: [
          {
            kind: 'intercepted' as const,
            name: 'commit abc1234',
            label: 'abc1234',
            insertText: 'commit abc1234 ',
            description: 'Fix the parser',
            searchText: 'commit abc1234 Fix the parser',
          },
        ],
      },
    ];
    expect(flattenSections(filterCommandSections(withSearch, 'parser'))).toHaveLength(1);
  });
});
