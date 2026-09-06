import { afterEach, expect, it } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { detachBackend, takePinnedBackend } from '../transport/backends';
import { SCOPES, setCarriedSessionScopes } from '../transport/scopes';
import { ensureClaudeSkills, getClaudeSkills, resetForTest as resetClaude } from './claudeSkills.svelte';
import { ensureCodexSkills, getCodexSkills, resetForTest as resetCodex } from './codexSkills.svelte';
import {
  ensureClaudeProbeCommands, getClaudeProbeCommands, invalidateClaudeProbeCommands,
  resetForTest as resetCommands,
} from './providerCommands.svelte';

afterEach(() => { resetStagedBackends(); resetClaude(); resetCodex(); resetCommands(); });

it('reads command and skill catalogs from the requested computer even when paths are identical', async () => {
  stageBackend();
  setCarriedSessionScopes('laptop', SCOPES);
  setBindingMock('GetClaudeSkills', async () => [{ name: takePinnedBackend() || 'home' }]);
  setBindingMock('GetCodexSkills', async () => ({ skills: [{ name: takePinnedBackend() || 'home' }] }));
  setBindingMock('GetClaudeSlashCommands', async () => ({ probed: true, commands: [{ name: takePinnedBackend() || 'home' }] }));
  await Promise.all(['', 'laptop'].flatMap((backend) => [
    ensureClaudeSkills('/repo', backend), ensureCodexSkills('/repo', false, backend), ensureClaudeProbeCommands(backend),
  ]));
  for (const backend of ['', 'laptop']) {
    const name = backend || 'home';
    expect(getClaudeSkills('/repo', backend).skills[0]?.name).toBe(name);
    expect(getCodexSkills('/repo', backend).skills[0]?.name).toBe(name);
    expect(getClaudeProbeCommands(backend).commands[0]?.name).toBe(name);
  }
});

it('does not resurrect catalogs when a removed computer returns late responses', async () => {
  stageBackend();
  setCarriedSessionScopes('laptop', SCOPES);
  const pending: Array<(answer: unknown) => void> = [];
  const deferred = () => new Promise((resolve) => pending.push(resolve));
  setBindingMock('GetClaudeSkills', deferred);
  setBindingMock('GetCodexSkills', deferred);
  setBindingMock('GetClaudeSlashCommands', deferred);
  const reads = [ensureClaudeSkills('/repo', 'laptop'), ensureCodexSkills('/repo', false, 'laptop'), ensureClaudeProbeCommands('laptop')];
  detachBackend('laptop');
  pending[0]([{ name: 'stale' }]);
  pending[1]({ skills: [{ name: 'stale' }] });
  pending[2]({ probed: true, commands: [{ name: 'stale' }] });
  await Promise.all(reads);
  expect(getClaudeSkills('/repo', 'laptop').status).toBe('unknown');
  expect(getCodexSkills('/repo', 'laptop').status).toBe('unknown');
  expect(getClaudeProbeCommands('laptop').probed).toBe(false);
});

it('keeps a newer explicit Codex refresh when an older scan finishes last', async () => {
  const pending: Array<(answer: unknown) => void> = [];
  setBindingMock('GetCodexSkills', () => new Promise((resolve) => pending.push(resolve)));
  const older = ensureCodexSkills('/repo', false, '');
  const newer = ensureCodexSkills('/repo', true, '');
  pending[1]({ skills: [{ name: 'new' }] });
  await newer;
  pending[0]({ skills: [{ name: 'old' }] });
  await older;
  expect(getCodexSkills('/repo', '').skills[0]?.name).toBe('new');
});

it('rejects a Claude command probe belonging to the previous account', async () => {
  let finish!: (answer: unknown) => void;
  setBindingMock('GetClaudeSlashCommands', () => new Promise((resolve) => { finish = resolve; }));
  const stale = ensureClaudeProbeCommands('');
  invalidateClaudeProbeCommands('');
  setBindingMock('GetClaudeSlashCommands', async () => ({ probed: true, commands: [{ name: 'new-account' }] }));
  await ensureClaudeProbeCommands('');
  finish({ probed: true, commands: [{ name: 'old-account' }] });
  await stale;
  expect(getClaudeProbeCommands('').commands[0]?.name).toBe('new-account');
});
