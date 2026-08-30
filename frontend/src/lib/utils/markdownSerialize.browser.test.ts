// Real-Chromium half of the markdown copy walker. One thing only: GFM
// task-list checkedness survives the walk when it exists solely as a DOM
// property.
//
// `serializeRangeToMarkdown` reads a fragment from `Range.cloneContents`,
// and the renderer renders the checkbox with Svelte's `checked={…}`
// binding — a property assignment that never writes the content attribute.
// Cloning carries it because the HTML cloning steps for <input> propagate
// checkedness; happy-dom does not implement that, so the unit suite can
// only cover the attribute-built shape. Without this file the property path
// is untested and a regression to an attribute-only read would copy every
// checked box as unchecked.
import { describe, expect, it } from 'vitest';
import { serializeRangeToMarkdown } from './markdownSerialize';

describe('serializeRangeToMarkdown — task lists (real DOM)', () => {
  it('reads checkedness set as a property, with no checked attribute', () => {
    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.innerHTML = '<ul><li>done</li><li>todo</li></ul>';
    const [doneItem, todoItem] = Array.from(host.querySelectorAll('li'));
    for (const [li, checked] of [[doneItem, true], [todoItem, false]] as const) {
      const box = document.createElement('input');
      box.type = 'checkbox';
      box.disabled = true;
      box.checked = checked;
      li.prepend(box);
    }
    document.body.appendChild(host);

    expect(host.querySelector('input')!.hasAttribute('checked')).toBe(false);

    const range = document.createRange();
    range.selectNodeContents(host);
    expect(serializeRangeToMarkdown(range)).toBe('- [x] done\n- [ ] todo');

    host.remove();
  });
});
