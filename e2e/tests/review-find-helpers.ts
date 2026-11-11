// Shared reads for the review find specs (review-find, compact-review-find,
// pulls-page-find): what a find key paints and whether its current match
// is in view.
import type { Locator, Page } from '@playwright/test';

/** The highlight name of a find key (utils/findHighlights.ts findHighlightName). */
export function findHighlightName(key: string): string {
  return `ao-find-${key.replace(/[^A-Za-z0-9_-]/g, '_')}`;
}

/** The ranges painted under a highlight name, as text. */
export function painted(page: Page, name: string): Promise<string[]> {
  return page.evaluate((n) => {
    const highlight = CSS.highlights.get(n);
    return highlight ? [...highlight].map((range) => range.toString()) : [];
  }, name);
}

/**
 * Whether the current match's range lies inside `scroller`, below the
 * sticky file header when one shows: never under the header or the bar.
 */
export function currentInView(scroller: Locator, name: string): Promise<boolean> {
  return scroller.evaluate((el, n) => {
    const highlight = CSS.highlights.get(`${n}-current`);
    const range = highlight ? [...highlight][0] : undefined;
    if (!(range instanceof Range)) return false;
    const rect = range.getBoundingClientRect();
    const view = el.getBoundingClientRect();
    const sticky = el.parentElement?.querySelector('[data-testid="review-sticky-header"]');
    const top = Math.max(view.top, sticky ? sticky.getBoundingClientRect().bottom : view.top);
    return rect.height > 0 && rect.top >= top && rect.bottom <= view.bottom;
  }, name);
}

/** A GitLab trace: a prepare section, then a script section of `lines`
 * lines and one "step N done" line per step. */
export function gitlabTrace(lines: number, steps: number): string {
  const t0 = 1_760_000_000;
  let text = 'Running with gitlab-runner 17.0.0\n';
  text += `section_start:${t0}:prepare_executor\r\x1b[0K\x1b[36;1mPreparing the executor\x1b[0;m\n`;
  text += 'Using docker image alpine:3.20\n';
  text += `\x1b[0Ksection_end:${t0 + 4}:prepare_executor\r\x1b[0K\n`;
  text += `section_start:${t0 + 4}:step_script\r\x1b[0K\x1b[36;1mExecuting the job script\x1b[0;m\n`;
  for (let line = 1; line <= lines; line += 1) text += `script line ${line}\n`;
  for (let step = 1; step <= steps; step += 1) text += `step ${step} done\n`;
  return text;
}
