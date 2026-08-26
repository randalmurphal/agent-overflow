// Codex MultiAgentV2 ("collab") child cards, end to end against the real
// backend and the real mock binary.
//
// The contract these cover is one the unit tests can only prove a slice of:
// a child's answers reach the parent as MAILBOX ENVELOPES on the raw response
// stream, minutes after the typed spawn item, and their identity has to come
// from the envelope's own content. `internal_chat_message_metadata_passthrough
// .turn_id` — the field AO used to key on — is the RECEIVING PARENT turn and
// is byte-identical across every delivery drained into one parent turn, so a
// child that answers twice in a turn used to lose its first answer. Only a run
// where the wire really carries two envelopes under one turn id proves the fix
// holds through the provider adapter, triage, and SQLite together.
import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';

interface Item {
  id: string;
  kind: string;
  status: string;
  summary: string;
  toolName?: string;
  completionOf?: string;
  isBackground?: boolean;
  meta?: string;
  payloadMeta?: string;
}

async function startCollabThread(harness: HarnessApp, scenario: string): Promise<string> {
  await harness.rpc('HarnessSetScenario', { name: scenario });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'codex-collab',
        repo: {},
        threads: [{ title: 'Collab', provider: 'codex' }],
      },
    ],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  await harness.waitForEvent('harness:mock', (ev: any) => ev.report.kind === 'registered');
  return threadId;
}

function parseMeta(item: Item | undefined): Record<string, unknown> {
  if (!item?.meta) return {};
  try {
    return JSON.parse(item.meta) as Record<string, unknown>;
  } catch {
    return {};
  }
}

/** Immutable spawn events. One per spawned child. */
function spawnCards(items: Item[]): Item[] {
  return items.filter((i) => i.kind === 'tool_call' && i.toolName === 'collab_agent');
}

function completionsFor(items: Item[], launchId: string): Item[] {
  return items.filter((i) => i.completionOf === launchId);
}

test('two FINAL_ANSWERs in one parent turn become two rows, not one overwritten row', async ({
  harness,
}) => {
  const threadId = await startCollabThread(harness, 'codex-collab-two-deliveries');
  await harness.rpc('SendMessage', threadId, 'review this', null);
  await harness.waitForEvent('provider:turn_completed');

  // Both answers must survive. Keyed on the passthrough turn id they collapse
  // onto one row and only "Second review pass." remains — the G1 bug.
  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId);
      const cards = spawnCards(items);
      if (cards.length !== 1) return [];
      return completionsFor(items, cards[0].id)
        .map((row) => {
          try {
            return String((JSON.parse(row.payloadMeta ?? '{}') as any).preview ?? '').trim();
          } catch {
            return '';
          }
        })
        .sort();
    })
    .toEqual(['First review pass.', 'Second review pass.']);

  // One card, and nothing dangling.
  const items = await harness.rpc<Item[]>('ListItems', threadId);
  expect(spawnCards(items)).toHaveLength(1);
  expect(items.filter((i) => i.status === 'running' || i.status === 'streaming')).toEqual([]);
});

test('a queue-only send_message becomes its own chronological activity row', async ({
  harness,
}) => {
  const threadId = await startCollabThread(harness, 'codex-collab-send-message-queueonly');
  await harness.rpc('SendMessage', threadId, 'ping the reviewer', null);
  await harness.waitForEvent('provider:turn_completed');

  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId);
      return items
        .filter((item) => item.toolName === 'send_input')
        .map((item) => String((parseMeta(item).input as any)?.activityTool ?? ''));
    })
    .toEqual(['send_message']);

  const items = await harness.rpc<Item[]>('ListItems', threadId);
  const activity = items.find((item) => item.toolName === 'send_input');
  expect(activity?.completionOf ?? '').toBe('');
  expect(parseMeta(spawnCards(items)[0]).codex_collab_interactions).toBeUndefined();
});

test('an encrypted MESSAGE delivery is a progress beat, not a completion', async ({ harness }) => {
  const threadId = await startCollabThread(harness, 'codex-collab-progress-message');
  await harness.rpc('SendMessage', threadId, 'review with progress', null);
  await harness.waitForEvent('provider:turn_completed');

  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId);
      const card = spawnCards(items)[0];
      const progress = items.filter(
        (item) => item.toolName === 'send_input' && (parseMeta(item).input as any)?.activityKind === 'progress',
      );
      if (!card) return null;
      return {
        progress: progress.length,
        completions: completionsFor(items, card.id).length,
      };
    })
    .toEqual({ progress: 1, completions: 1 });

  // The ciphertext body never becomes timeline text.
  const items = await harness.rpc<Item[]>('ListItems', threadId);
  const card = spawnCards(items)[0];
  const progress = items.find(
    (item) => item.toolName === 'send_input' && (parseMeta(item).input as any)?.activityKind === 'progress',
  );
  expect((parseMeta(progress).input as any)?.message ?? '').toBe('');
  const completion = completionsFor(items, card.id)[0];
  expect(JSON.parse(completion.payloadMeta ?? '{}').preview).toContain('Review complete.');
});

test('parallel children each keep their own answer', async ({ harness }) => {
  const threadId = await startCollabThread(harness, 'codex-collab-parallel-children');
  await harness.rpc('SendMessage', threadId, 'run both lenses', null);
  await harness.waitForEvent('provider:turn_completed');

  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId);
      const cards = spawnCards(items);
      if (cards.length !== 2) return null;
      return cards
        .map((card) =>
          completionsFor(items, card.id)
            .map((row) => String(JSON.parse(row.payloadMeta ?? '{}').preview ?? '').trim())
            .join('|'),
        )
        .sort();
    })
    .toEqual(['Reviewer verdict.', 'Tester verdict.']);
});

test('a reloaded child resumes on the same card and both answers survive', async ({ harness }) => {
  const threadId = await startCollabThread(harness, 'codex-collab-reload-after-unload');
  await harness.rpc('SendMessage', threadId, 'review then follow up', null);
  await harness.waitForEvent('provider:turn_completed');

  await expect
    .poll(async () => {
      const items = await harness.rpc<Item[]>('ListItems', threadId);
      const cards = spawnCards(items);
      if (cards.length !== 1) return null;
      return {
        cards: cards.length,
        answers: completionsFor(items, cards[0].id)
          .map((row) => String(JSON.parse(row.payloadMeta ?? '{}').preview ?? '').trim())
          .sort(),
        followups: items.filter(
          (item) => item.toolName === 'send_input' && (parseMeta(item).input as any)?.activityKind === 'interacted',
        ).length,
      };
    })
    .toEqual({
      cards: 1,
      answers: ['First answer.', 'Second answer.'],
      followups: 1,
    });
});
