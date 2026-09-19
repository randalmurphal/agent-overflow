import { describe, expect, it } from 'vitest';
import { isHiddenThreadMode, isScratchThreadMode, renderedThreadMode } from './threadModes';

describe('isHiddenThreadMode', () => {
  it.each(['workflow', 'workflow-studio', 'workflow-triage', 'scratch'])('hides %s', (mode) => {
    expect(isHiddenThreadMode(mode)).toBe(true);
  });

  it.each(['chat', 'plan', 'discussion', 'terminal', '', undefined])('keeps %s visible', (mode) => {
    expect(isHiddenThreadMode(mode)).toBe(false);
  });
});

describe('isScratchThreadMode', () => {
  it('recognizes the side chat mode', () => {
    expect(isScratchThreadMode('scratch')).toBe(true);
  });

  it.each(['chat', 'plan', 'workflow', '', undefined])('rejects %s', (mode) => {
    expect(isScratchThreadMode(mode)).toBe(false);
  });
});

describe('renderedThreadMode', () => {
  it('renders a scratch thread as a chat while the row keeps its own mode', () => {
    expect(renderedThreadMode('scratch')).toBe('chat');
  });

  it.each(['chat', 'plan', 'discussion', 'terminal'])('leaves %s alone', (mode) => {
    expect(renderedThreadMode(mode)).toBe(mode);
  });

  it('answers empty for a thread with no mode yet', () => {
    expect(renderedThreadMode(undefined)).toBe('');
    expect(renderedThreadMode('')).toBe('');
  });
});
