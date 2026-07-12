import { describe, expect, it } from 'vitest';
import { isHiddenThreadMode } from './threadModes';

describe('isHiddenThreadMode', () => {
  it.each(['workflow', 'workflow-studio', 'workflow-triage'])('hides %s', (mode) => {
    expect(isHiddenThreadMode(mode)).toBe(true);
  });

  it.each(['chat', 'plan', 'design', 'discussion', 'terminal', '', undefined])('keeps %s visible', (mode) => {
    expect(isHiddenThreadMode(mode)).toBe(false);
  });
});
