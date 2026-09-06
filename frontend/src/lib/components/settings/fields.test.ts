import { seedSettingsPages } from '../../../test/helpers/settingsPages';
// The search index is checked against the pages it describes: every page is
// mounted with the shipped-default settings, and the anchors it renders are
// compared with the entries `fields.ts` registers for it. Both directions
// fail — an unregistered control is unsearchable, a registered control that
// no page renders is a dead search hit — and a label or hint that differs
// from the index would make the search result read differently from the
// row it lands on.

import { describe, expect, it, beforeEach } from 'vitest';
import { render } from '@testing-library/svelte';
import { PROVIDER_SETTINGS_ORDER } from '../../providers/catalog';
import { SETTINGS_FIELDS, SETTINGS_PROVIDERS } from './fields';
import { SETTINGS_PAGES } from './pages';
import { SETTINGS_SECTIONS } from './sections';


async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
}

describe('settings field index', () => {
  it('has unique ids, each on a page that exists', () => {
    const ids = SETTINGS_FIELDS.map((f) => f.id);
    expect(new Set(ids).size).toBe(ids.length);
    const pages = new Set<string>(SETTINGS_SECTIONS.map((s) => s.id));
    for (const field of SETTINGS_FIELDS) {
      expect(pages.has(field.section), `${field.id} names page ${field.section}`).toBe(true);
    }
  });

  it('gives every provider in settings order its own page', () => {
    expect([...SETTINGS_PROVIDERS]).toEqual(PROVIDER_SETTINGS_ORDER);
  });
});

describe('every page renders exactly its registered fields', () => {
  beforeEach(async () => {
    await seedSettingsPages();
  });

  for (const section of SETTINGS_SECTIONS) {
    it(`${section.id}`, async () => {
      const { container } = render(SETTINGS_PAGES[section.id]);
      await settle();

      const rendered = new Map<string, { label: string | undefined; hint: string | undefined }>();
      for (const el of container.querySelectorAll<HTMLElement>('[data-settings-field]')) {
        const id = el.dataset.settingsField ?? '';
        expect(rendered.has(id), `${section.id} renders ${id} twice`).toBe(false);
        rendered.set(id, { label: el.dataset.settingsLabel, hint: el.dataset.settingsHint });
      }

      const expected = SETTINGS_FIELDS.filter((f) => f.section === section.id);
      const expectedIds = new Set(expected.map((f) => f.id));

      for (const id of rendered.keys()) {
        expect(expectedIds.has(id), `${section.id} renders unregistered field ${id}`).toBe(true);
      }

      for (const field of expected) {
        const got = rendered.get(field.id);
        if (!got) {
          expect(field.conditional ?? false, `${field.id} is registered but not rendered`).toBe(
            true,
          );
          continue;
        }
        expect(got.label, `${field.id} label`).toBe(field.label);
        if (field.hint !== undefined && got.hint !== undefined) {
          expect(got.hint, `${field.id} hint`).toBe(field.hint);
        }
      }
    });
  }
});
