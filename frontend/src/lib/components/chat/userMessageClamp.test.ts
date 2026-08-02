import { describe, expect, it } from 'vitest';
import { USER_MESSAGE_CLAMP_LINES, userMessageMayClamp } from './userMessageClamp';

// The pre-gate is what keeps short messages free of the clamp's DOM, its
// observer and its effect, so its ONE contract is that it is one-sided:
// `false` must mean "cannot reach the clamp at any width the pane layout can
// take". A false negative would clip a message with no way to open it.
describe('userMessageMayClamp', () => {
  const line = (chars: number) => 'x'.repeat(chars);

  it('lets ordinary short messages through untouched', () => {
    expect(userMessageMayClamp('')).toBe(false);
    expect(userMessageMayClamp('ship it')).toBe(false);
    expect(userMessageMayClamp('Please rerun the failing test and paste the output.')).toBe(false);
  });

  it('counts hard newlines, which no width can merge', () => {
    const atCap = Array.from({ length: USER_MESSAGE_CLAMP_LINES }, (_, i) => `line ${i}`);
    // Exactly the cap fits; one more line does not.
    expect(userMessageMayClamp(atCap.join('\n'))).toBe(false);
    expect(userMessageMayClamp([...atCap, 'one more'].join('\n'))).toBe(true);
  });

  it('counts the wrapping a long single paragraph must do', () => {
    expect(userMessageMayClamp(line(200))).toBe(false);
    expect(userMessageMayClamp(line(5_000))).toBe(true);
  });

  it('adds wrapping to hard lines instead of taking the larger of the two', () => {
    // Eight hard lines, each wide enough to wrap: neither count alone passes
    // the cap, and a gate that looked at only one would let this through.
    const wide = Array.from({ length: 8 }, () => line(120)).join('\n');
    expect(wide.split('\n')).toHaveLength(8);
    expect(userMessageMayClamp(wide)).toBe(true);
  });

  it('treats a blank line as the rendered line it occupies', () => {
    const paragraphs = Array.from({ length: 7 }, (_, i) => `p${i}`).join('\n\n');
    expect(paragraphs.split('\n')).toHaveLength(13);
    expect(userMessageMayClamp(paragraphs)).toBe(true);
  });
});
