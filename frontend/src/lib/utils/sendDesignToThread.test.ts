import { describe, expect, it } from 'vitest';
import { buildSendToThreadDraftBody } from './sendDesignToThread';

describe('buildSendToThreadDraftBody', () => {
  it('formats path + manifest + attachment hint when a screenshot is attached', () => {
    const body = buildSendToThreadDraftBody('/tmp/main', ['index.html', 'style.css'], true);
    expect(body).toContain('Files at: `/tmp/main`');
    expect(body).toContain('  - index.html');
    expect(body).toContain('  - style.css');
    expect(body).toMatch(/A screenshot of the current state is attached/);
    // Two trailing newlines so the composer cursor lands a blank
    // line below the seeded context, not on the last manifest entry.
    expect(body.endsWith('\n\n')).toBe(true);
  });

  it('omits the screenshot-attached line when capture failed and no PNG is uploaded', () => {
    // Pins the contract: the body must NOT claim an attachment the
    // thread doesn't have. The user reported a draft that said "A
    // screenshot of the current state is attached" alongside a toast
    // confirming capture failed — confusing for both the user and
    // any agent reading the seed message.
    const body = buildSendToThreadDraftBody('/tmp/main', ['index.html'], false);
    expect(body).not.toMatch(/screenshot/i);
    expect(body).toContain('Files at: `/tmp/main`');
    expect(body).toContain('  - index.html');
    // Body still ends with a blank line so the composer cursor still
    // lands in a useful spot.
    expect(body.endsWith('\n\n')).toBe(true);
  });

  it('falls back to a placeholder when the manifest is empty', () => {
    const body = buildSendToThreadDraftBody('/tmp/main', [], true);
    expect(body).toContain('(no files yet)');
    expect(body).toContain('Files at: `/tmp/main`');
  });

  it('caps the manifest at 50 entries and notes how many were elided', () => {
    const files = Array.from({ length: 73 }, (_, i) => `file-${i}.html`);
    const body = buildSendToThreadDraftBody('/tmp/main', files, true);
    expect(body).toContain('  - file-0.html');
    expect(body).toContain('  - file-49.html');
    // 50th index (file-50.html) onward should be elided.
    expect(body).not.toContain('file-72.html');
    expect(body).toContain('… and 23 more');
  });

  it('strips backticks from path + filenames so they cannot break out of the markdown code span', () => {
    // A future config option might let the user choose a design base
    // dir; today the path is host-controlled but defense in depth
    // applies — a backtick would close the code span and leak the
    // remainder as free-form prompt text into the LLM.
    const body = buildSendToThreadDraftBody('/tmp/main`evil', ['file`with`ticks.html'], true);
    expect(body).not.toContain('`evil');
    expect(body).not.toContain('`with`ticks');
    expect(body).toContain('/tmp/mainevil');
    expect(body).toContain('  - filewithticks.html');
  });
});
