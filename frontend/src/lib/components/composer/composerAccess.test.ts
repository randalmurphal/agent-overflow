import { describe, expect, it } from 'vitest';
import { composerAccess, type ComposerCapability } from './composerAccess';
import type { EntityScopeGrant } from '../../transport/scopes';

const grants = (answers: Partial<Record<ComposerCapability, EntityScopeGrant>>) =>
  (scope: ComposerCapability): EntityScopeGrant => answers[scope] ?? 'granted';

describe('composerAccess', () => {
  it('passes each capability its own grant while the computer is reachable', () => {
    expect(composerAccess(true, grants({ 'approvals:respond': 'pending', 'attachments:write': 'unrouted', 'terminal:operate': 'refused' })))
      .toEqual({ send: 'granted', respond: 'pending', attach: 'unrouted', shell: 'refused' });
  });

  it('says offline for every capability the outage blocks, a full grant included', () => {
    expect(composerAccess(false, grants({}))).toEqual({ send: 'offline', respond: 'offline', attach: 'offline', shell: 'offline' });
    expect(composerAccess(false, grants({ 'threads:operate': 'pending', 'approvals:respond': 'unrouted' })))
      .toEqual({ send: 'offline', respond: 'offline', attach: 'offline', shell: 'offline' });
  });

  it('keeps a known refusal over the outage, since reconnecting would not let it through', () => {
    expect(composerAccess(false, grants({ 'threads:operate': 'refused' })))
      .toEqual({ send: 'refused', respond: 'offline', attach: 'offline', shell: 'offline' });
  });
});
