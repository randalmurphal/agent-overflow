import { describe, expect, it } from 'vitest';
import {
  NO_LOADED_SUBAGENT_CHILDREN,
  agentScopeRootId,
  claudeResumeCarrierIdentity,
  claudeResumeTranscriptRootId,
  isPotentialSubagentLaunch,
  subagentLaunchContextFrom,
  subagentLaunchInfo,
} from './subagentLaunch';
import type { Item } from '../types/models';

function mkItem(overrides: Partial<Item> & { id: string }): Item {
  return {
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'running',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

function meta(values: Record<string, unknown>): string {
  return JSON.stringify(values);
}

const NO_CHILDREN = NO_LOADED_SUBAGENT_CHILDREN;

describe('subagentLaunchInfo — Claude Agent / Task', () => {
  it('reads an awaited Agent launch, naming it from subagent_type', () => {
    const info = subagentLaunchInfo(
      mkItem({
        id: 'agent-1',
        toolName: 'Agent',
        meta: meta({ toolName: 'Agent', input: { subagent_type: 'general-purpose' } }),
      }),
      NO_CHILDREN,
    );

    expect(info).toEqual({
      kind: 'agent',
      provider: 'claude',
      background: false,
      name: 'General Purpose',
      agentType: 'general-purpose',
    });
  });

  it('treats the legacy Task tool name as the same launch', () => {
    const info = subagentLaunchInfo(
      mkItem({ id: 'task-1', toolName: 'Task', meta: meta({ toolName: 'Task' }) }),
      NO_CHILDREN,
    );

    expect(info?.kind).toBe('agent');
    // `Task` resolves through the `Agent` label rules so one launch never
    // renders two ways depending on which tool name the build used.
    expect(info?.name).toBe('Agent');
    expect(info?.agentType).toBeUndefined();
  });

  it('marks an async launch background, and treats a missing flag as awaited', () => {
    // claude-wire.md §E5: `is_background` is optional on the wire, so
    // undefined and false both mean awaited.
    const async = subagentLaunchInfo(
      mkItem({ id: 'agent-bg', toolName: 'Agent', isBackground: true }),
      NO_CHILDREN,
    );
    const awaitedExplicit = subagentLaunchInfo(
      mkItem({ id: 'agent-fg', toolName: 'Agent', isBackground: false }),
      NO_CHILDREN,
    );
    const awaitedAbsent = subagentLaunchInfo(
      mkItem({ id: 'agent-none', toolName: 'Agent' }),
      NO_CHILDREN,
    );

    expect(async?.background).toBe(true);
    expect(awaitedExplicit?.background).toBe(false);
    expect(awaitedAbsent?.background).toBe(false);
  });

  it('prefers payloadMeta input over meta input', () => {
    const info = subagentLaunchInfo(
      mkItem({
        id: 'agent-2',
        toolName: 'Agent',
        payloadMeta: meta({ input: { subagent_type: 'Explore' } }),
        meta: meta({ input: { subagent_type: 'stale' } }),
      }),
      NO_CHILDREN,
    );

    expect(info?.name).toBe('Explore');
  });

  it('is not a launch when the row is a completion rather than a call', () => {
    expect(
      subagentLaunchInfo(
        mkItem({ id: 'complete:agent-1', kind: 'tool_completion', toolName: 'Agent' }),
        NO_CHILDREN,
      ),
    ).toBeNull();
  });
});

describe('subagentLaunchInfo — forked Skill (claude-wire.md §E9)', () => {
  const skill = (overrides: Partial<Item> = {}): Item =>
    mkItem({
      id: 'skill-1',
      toolName: 'Skill',
      summary: 'Skill: code-review',
      meta: meta({ toolName: 'Skill', input: { skill: 'code-review', args: 'medium' } }),
      ...overrides,
    });

  it('detects the fork from an attributed child row', () => {
    // Signal 1 — "did this Skill fork" is answered by the first row
    // attributed to its tool_use, never by a skill-name list.
    const items = [skill(), mkItem({ id: 'child', parentId: 'skill-1', kind: 'assistant_text' })];
    const info = subagentLaunchInfo(items[0], subagentLaunchContextFrom(items));

    expect(info).toEqual({
      kind: 'skill',
      provider: 'claude',
      background: false,
      name: 'code-review',
    });
  });

  it('detects the fork from the parser skillFork stamp with no children loaded', () => {
    // Signal 2 — the completion's `tool_use_result {status:"forked",
    // agentId, commandName}`, merged onto the launch row by triage.
    const info = subagentLaunchInfo(
      skill({
        meta: meta({
          toolName: 'Skill',
          input: { skill: 'code-review' },
          skillFork: { agentId: 'a40feadb631d41a04', commandName: 'code-review' },
        }),
      }),
      NO_CHILDREN,
    );

    expect(info?.kind).toBe('skill');
    expect(info?.name).toBe('code-review');
  });

  it('names the fork from skillFork.commandName when the input is unavailable', () => {
    const info = subagentLaunchInfo(
      skill({
        meta: meta({ skillFork: { agentId: 'a1', commandName: 'post-task-review' } }),
      }),
      NO_CHILDREN,
    );

    expect(info?.name).toBe('post-task-review');
  });

  it('detects the fork from the store descendant-count decoration', () => {
    // Signal 3 — the only one available on a history window, which loads
    // no child rows at all.
    const info = subagentLaunchInfo(
      skill({
        meta: meta({
          toolName: 'Skill',
          input: { skill: 'brainstorm' },
          subagentDescendantCount: 12,
        }),
      }),
      NO_CHILDREN,
    );

    expect(info?.kind).toBe('skill');
    expect(info?.name).toBe('brainstorm');
  });

  it('is NOT a launch for an inline skill with none of the three signals', () => {
    // A skill that did not fork gets zero attributed rows and an immediate
    // `Launching skill: <name>` result. It stays an ordinary tool row.
    const items = [skill(), mkItem({ id: 'unrelated', kind: 'assistant_text' })];

    expect(subagentLaunchInfo(items[0], subagentLaunchContextFrom(items))).toBeNull();
    expect(subagentLaunchInfo(skill(), NO_CHILDREN)).toBeNull();
  });

  it('ignores a zero or malformed descendant-count decoration', () => {
    expect(
      subagentLaunchInfo(skill({ meta: meta({ subagentDescendantCount: 0 }) }), NO_CHILDREN),
    ).toBeNull();
    expect(
      subagentLaunchInfo(
        skill({ meta: meta({ subagentDescendantCount: 'lots' }) }),
        NO_CHILDREN,
      ),
    ).toBeNull();
    expect(
      subagentLaunchInfo(skill({ meta: meta({ skillFork: 'forked' }) }), NO_CHILDREN),
    ).toBeNull();
  });
});

describe('subagentLaunchInfo — SendMessage resume carrier (claude-wire.md §E6)', () => {
  it('reads a backgrounded SendMessage bound to a task id as an agent launch', () => {
    const info = subagentLaunchInfo(
      mkItem({
        id: 'toolu_resume',
        toolName: 'SendMessage',
        isBackground: true,
        summary: 'Agent: Frontend transitive suppression fix',
        meta: meta({
          toolName: 'SendMessage',
          task_id: 'a464e54e96a45cd0c',
          resumes_tool_use_id: 'toolu_original',
          description: 'Frontend transitive suppression fix',
        }),
      }),
      NO_CHILDREN,
    );

    expect(info).toEqual({
      kind: 'agent',
      provider: 'claude',
      background: true,
      name: 'Frontend transitive suppression fix',
    });
  });

  it('carries the original agent identity when the triage stamps are present', () => {
    // Parser stamps subagent_type + description off the rebind
    // task_started; triage copies subagent_model from the original
    // launch row in the keep-running flip. With all three present the
    // carrier renders exactly like the original launch: title-cased
    // type as the name, model affix, description beside it.
    const item = mkItem({
      id: 'toolu_resume',
      toolName: 'SendMessage',
      isBackground: true,
      summary: 'Agent: Frontend transitive suppression fix',
      meta: meta({
        task_id: 'a464e54e96a45cd0c',
        resumes_tool_use_id: 'toolu_original',
        description: 'Frontend transitive suppression fix',
        subagent_type: 'general-purpose',
        subagent_model: 'claude-opus-4-7',
      }),
    });

    expect(subagentLaunchInfo(item, NO_CHILDREN)).toEqual({
      kind: 'agent',
      provider: 'claude',
      background: true,
      name: 'General Purpose',
      model: 'claude-opus-4-7',
      agentType: 'general-purpose',
    });

    expect(claudeResumeCarrierIdentity(item)).toEqual({
      name: 'General Purpose',
      agentType: 'general-purpose',
      model: 'claude-opus-4-7',
      description: 'Frontend transitive suppression fix',
    });
  });

  it('suppresses the description line when it already is the name', () => {
    // No subagent_type stamp: the description doubles as the name, so
    // the identity's description is empty and no surface renders the
    // same line twice.
    const identity = claudeResumeCarrierIdentity(
      mkItem({
        id: 'toolu_resume',
        toolName: 'SendMessage',
        isBackground: true,
        meta: meta({ task_id: 'a1', description: 'investigate the leak' }),
      }),
    );
    expect(identity.name).toBe('investigate the leak');
    expect(identity.description).toBe('');
  });

  it('falls back to un-prefixing the rewritten summary when no description was stamped', () => {
    // The reconnect edge: `resumes_tool_use_id` is unknown, so the parser
    // stamps no description, but triage still rewrote the Summary.
    const info = subagentLaunchInfo(
      mkItem({
        id: 'toolu_resume',
        toolName: 'SendMessage',
        isBackground: true,
        summary: 'Agent: investigate the leak',
        meta: meta({ task_id: 'a464e54e96a45cd0c' }),
      }),
      NO_CHILDREN,
    );

    expect(info?.name).toBe('investigate the leak');
  });

  it('is NOT a launch for an ordinary SendMessage, or for one with no task id', () => {
    expect(
      subagentLaunchInfo(
        mkItem({ id: 'send-1', toolName: 'SendMessage', meta: meta({ task_id: 'a1' }) }),
        NO_CHILDREN,
      ),
    ).toBeNull();
    expect(
      subagentLaunchInfo(
        mkItem({ id: 'send-2', toolName: 'SendMessage', isBackground: true }),
        NO_CHILDREN,
      ),
    ).toBeNull();
  });
});

describe('agentScopeRootId', () => {
  // claude-wire.md §E6: only the task LIFECYCLE rebinds onto the carrier.
  // Every round's rows — tool calls, prose, nested launches, background
  // Bash — stay parented to the ORIGINAL launch, so the scope a pane or a
  // hydration call opens must be that launch, never the carrier.
  const carrier = (overrides: Record<string, unknown> = {}): Item =>
    mkItem({
      id: 'toolu_resume_2',
      toolName: 'SendMessage',
      isBackground: true,
      meta: meta({
        task_id: 'a464e54e96a45cd0c',
        resumes_tool_use_id: 'toolu_original',
        transcript_root_id: 'toolu_original',
        subagent_type: 'general-purpose',
        subagent_model: 'claude-opus-4-7',
        ...overrides,
      }),
    });

  it('resolves a resume carrier to the original launch', () => {
    expect(agentScopeRootId(carrier())).toBe('toolu_original');
    expect(claudeResumeTranscriptRootId(carrier())).toBe('toolu_original');
  });

  it('names the ORIGINAL launch for round three, never the previous carrier', () => {
    // The stamp is one hop deep by construction: the backend always
    // writes the original's id, so no surface ever walks a chain.
    const roundThree = carrier({ resumes_tool_use_id: 'toolu_resume_2' });
    expect(agentScopeRootId(roundThree)).toBe('toolu_original');
  });

  it('is the row itself for every other launch, and for an unstamped carrier', () => {
    const launch = mkItem({
      id: 'agent-1',
      toolName: 'Agent',
      meta: meta({ toolName: 'Agent', input: { subagent_type: 'Explore' } }),
    });
    expect(agentScopeRootId(launch)).toBe('agent-1');
    expect(claudeResumeTranscriptRootId(launch)).toBe('');

    // A carrier whose ack has not landed yet carries no stamp: it scopes
    // to itself rather than to nothing.
    const unstamped = mkItem({
      id: 'toolu_resume_2',
      toolName: 'SendMessage',
      isBackground: true,
      meta: meta({ task_id: 'a1' }),
    });
    expect(agentScopeRootId(unstamped)).toBe('toolu_resume_2');

    // A self-referential stamp is refused rather than trusted.
    expect(agentScopeRootId(carrier({ transcript_root_id: 'toolu_resume_2' }))).toBe(
      'toolu_resume_2',
    );

    // An ordinary SendMessage is not a carrier at all, whatever it carries.
    const ordinary = mkItem({
      id: 'send-1',
      toolName: 'SendMessage',
      meta: meta({ transcript_root_id: 'toolu_original' }),
    });
    expect(agentScopeRootId(ordinary)).toBe('send-1');
  });
});

describe('subagentLaunchInfo — Codex spawn_agent', () => {
  it('reads a spawn row as an always-background agent launch', () => {
    const info = subagentLaunchInfo(
      mkItem({
        id: 'spawn-1',
        toolName: 'collab_agent',
        meta: meta({
          toolName: 'collab_agent',
          input: {
            tool: 'spawn_agent',
            newAgentNickname: 'Chandrasekhar',
            newAgentRole: 'reviewer',
          },
        }),
      }),
      NO_CHILDREN,
    );

    expect(info).toEqual({
      kind: 'agent',
      provider: 'codex',
      background: true,
      name: 'Chandrasekhar [reviewer]',
      agentType: 'reviewer',
    });
  });

  it('is NOT a launch for a non-spawn collab tool', () => {
    expect(
      subagentLaunchInfo(
        mkItem({
          id: 'collab-1',
          toolName: 'collab_agent',
          meta: meta({ toolName: 'collab_agent', input: { tool: 'send_input' } }),
        }),
        NO_CHILDREN,
      ),
    ).toBeNull();
  });
});

describe('subagentLaunchInfo — Codex built-in review', () => {
  it('reads the synthetic review launch as an awaited agent with its effective model', () => {
    const info = subagentLaunchInfo(
      mkItem({
        id: 'review-1',
        toolName: 'codex_review',
        meta: meta({
          toolName: 'codex_review',
          input: {
            tool: 'review',
            model: 'gpt-5.6-codex',
            newAgentNickname: 'Code review',
            newAgentRole: 'review',
          },
        }),
      }),
      NO_CHILDREN,
    );

    expect(info).toEqual({
      kind: 'agent',
      provider: 'codex',
      background: false,
      name: 'Code review',
      model: 'gpt-5.6-codex',
      agentType: 'review',
    });
  });
});

describe('subagentLaunchContextFrom', () => {
  it('answers hasChildren from any row that names the id as parent', () => {
    const ctx = subagentLaunchContextFrom([
      mkItem({ id: 'a' }),
      mkItem({ id: 'b', parentId: 'a' }),
    ]);

    expect(ctx.hasChildren('a')).toBe(true);
    expect(ctx.hasChildren('b')).toBe(false);
  });

  it('is consulted only for Skill rows, so an Agent-only window builds no index', () => {
    // The index build is what costs; the predicate must not trigger it for
    // any row whose kind it can settle from the tool name alone.
    let reads = 0;
    const ctx = {
      hasChildren(): boolean {
        reads += 1;
        return false;
      },
    };

    subagentLaunchInfo(mkItem({ id: 'agent', toolName: 'Agent' }), ctx);
    subagentLaunchInfo(mkItem({ id: 'bash', toolName: 'Bash' }), ctx);
    subagentLaunchInfo(mkItem({ id: 'send', toolName: 'SendMessage' }), ctx);
    expect(reads).toBe(0);

    subagentLaunchInfo(mkItem({ id: 'skill', toolName: 'Skill' }), ctx);
    expect(reads).toBe(1);
  });
});

describe('isPotentialSubagentLaunch', () => {
  it('is a superset of the real predicate — every launch tool name passes', () => {
    for (const toolName of ['Agent', 'Task', 'Skill', 'SendMessage', 'collab_agent', 'codex_review']) {
      expect(isPotentialSubagentLaunch(mkItem({ id: toolName, toolName }))).toBe(true);
    }
    // Superset: an inline skill is not a launch, but must still trip the
    // grouping pass's fast path so the real predicate gets to run.
    expect(
      subagentLaunchInfo(mkItem({ id: 'skill', toolName: 'Skill' }), NO_CHILDREN),
    ).toBeNull();
  });

  it('rejects ordinary tools and non-tool rows', () => {
    expect(isPotentialSubagentLaunch(mkItem({ id: 'bash', toolName: 'Bash' }))).toBe(false);
    expect(
      isPotentialSubagentLaunch(
        mkItem({ id: 'text', kind: 'assistant_text', toolName: 'Agent' }),
      ),
    ).toBe(false);
  });
});
