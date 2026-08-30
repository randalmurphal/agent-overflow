import type { Locator, Page } from '@playwright/test';
import type { SemanticObservation, SemanticTarget } from './flow-model.ts';

const ownedPageObjects = new WeakMap<Page, OwnedPage>();
const ownedPages = new WeakSet<OwnedPage>();
const semanticLocatorToken = Symbol('semantic-locator');

export class OwnedPage {
  readonly #page: Page;
  readonly #origin: string;
  #activeRun = false;

  private constructor(page: Page) {
    this.#page = page;
    this.#origin = new URL(page.url()).origin;
  }

  static from(page: Page): OwnedPage {
    const existing = ownedPageObjects.get(page);
    if (existing) return existing;
    const owned = new OwnedPage(page);
    ownedPageObjects.set(page, owned);
    ownedPages.add(owned);
    return owned;
  }

  locator(target: SemanticTarget): SemanticLocator {
    this.assertCurrent();
    return SemanticLocator.create(this, this.#page, locatorFor(this.#page, target), semanticLocatorToken);
  }
  viewport(width: number, height: number): Promise<void> {
    this.assertCurrent();
    return this.#page.setViewportSize({ width, height });
  }
  async wheel(target: SemanticTarget, deltaX: number, deltaY: number): Promise<void> { const locator = this.locator(target); await locator.requireActionable('wheel'); await locator.hover(); await this.#page.mouse.wheel(deltaX, deltaY); }
  assertCurrent(): void {
    const origin = new URL(this.#page.url()).origin;
    if (origin !== this.#origin) throw new Error(`owned flow page navigated to ${origin}, expected ${this.#origin}`);
  }
  claimRun(): () => void {
    if (this.#activeRun) throw new Error('owned flow page already has an active run');
    this.#activeRun = true;
    return () => { this.#activeRun = false; };
  }
}

export function ownPage(page: Page): OwnedPage { return OwnedPage.from(page); }

export function isOwnedPage(page: OwnedPage): boolean { return ownedPages.has(page); }

function locatorFor(page: Page, target: SemanticTarget): Locator {
  if ('testId' in target) return page.getByTestId(target.testId);
  if ('role' in target) return page.getByRole(target.role as never, target.name === undefined ? {} : { name: target.name, exact: target.exact });
  if ('label' in target) return page.getByLabel(target.label, { exact: target.exact });
  if ('text' in target) return page.getByText(target.text, { exact: target.exact });
  return page.getByPlaceholder(target.placeholder, { exact: target.exact });
}

export class SemanticLocator {
  readonly #owner: OwnedPage;
  readonly #page: Page;
  readonly #raw: Locator;
  private constructor(owner: OwnedPage, page: Page, locator: Locator) { this.#owner = owner; this.#page = page; this.#raw = locator; }
  static create(owner: OwnedPage, page: Page, locator: Locator, token: typeof semanticLocatorToken): SemanticLocator {
    if (token !== semanticLocatorToken) throw new Error('semantic locators can only be created by an owned page');
    return new SemanticLocator(owner, page, locator);
  }
  async count(): Promise<number> { this.#owner.assertCurrent(); return this.#raw.count(); }
  click(): Promise<void> { this.#owner.assertCurrent(); return this.#raw.click(); }
  focus(): Promise<void> { this.#owner.assertCurrent(); return this.#raw.focus(); }
  fill(value: string): Promise<void> { this.#owner.assertCurrent(); return this.#raw.fill(value); }
  type(text: string, delayMs?: number): Promise<void> { this.#owner.assertCurrent(); return this.#raw.pressSequentially(text, { delay: delayMs }); }
  key(key: string): Promise<void> { this.#owner.assertCurrent(); return this.#raw.press(key); }
  hover(): Promise<void> { this.#owner.assertCurrent(); return this.#raw.hover(); }
  dragTo(target: SemanticLocator): Promise<void> { this.#owner.assertCurrent(); target.#owner.assertCurrent(); return this.#raw.dragTo(target.#raw); }
  async observe(attributeNames: string[] = []): Promise<SemanticObservation> {
    this.#owner.assertCurrent();
    const count = await this.#raw.count();
    if (count === 0) return { count, visible: false, text: '', value: null, checked: null, disabled: false, focused: false, selected: null, attributes: {}, timestamp: Date.now() };
    const first = this.#raw.first();
    const [visible, text, valueAttr, disabled, selectedAttr] = await Promise.all([
      first.isVisible(), first.textContent(), first.getAttribute('value'), first.isDisabled(), first.getAttribute('aria-selected'),
    ]);
    const focused = (await first.and(this.#page.locator(':focus')).count()) > 0;
    const checked = await first.isChecked().catch((error: unknown) => { if (/not a checkbox/i.test(String(error))) return null; throw error; });
    const value = await first.inputValue().catch((error: unknown) => { if (/Node is not an <input>/i.test(String(error))) return valueAttr; throw error; });
    const selected = selectedAttr === null ? (await first.getAttribute('selected')) !== null : selectedAttr === 'true';
    const attributes: Record<string, string | null> = {};
    for (const name of attributeNames) attributes[name] = await first.getAttribute(name);
    return { count, visible, text: text ?? '', value, checked, disabled, focused, selected, attributes, timestamp: Date.now() };
  }
  async requireActionable(operation: string): Promise<void> {
    const observation = await this.observe();
    if (observation.count !== 1) throw new Error(`${operation} requires exactly one semantic match, got ${observation.count}`);
    if (!observation.visible) throw new Error(`${operation} target is not visible`);
    if (observation.disabled) throw new Error(`${operation} target is disabled`);
  }
}
