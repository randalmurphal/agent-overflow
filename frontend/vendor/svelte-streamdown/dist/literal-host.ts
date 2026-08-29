/**
 * Single-owner text controller for the active trailing literal leaf
 * (divergence 21).
 *
 * The host used to contain a Svelte-owned Text node that app code appended
 * SIBLINGS to. Two writers on one visible text run: on every authoritative
 * fallback the app deleted its siblings in one task and Svelte re-extended its
 * own node in a later one, so the visible string shrank back to an older
 * parser checkpoint before regrowing. Chromium coalesces that before the next
 * frame; WebView2 is allowed to present it.
 *
 * The controller is now the ONLY writer of this element's children. The
 * renderer publishes the parser's authoritative leaf text into it and never
 * renders a Text node of its own; an app-side owner may adopt the element and
 * take over presentation while it streams. Both are held to one rule:
 *
 *   the visible string only ever EXTENDS, until a genuine divergence replaces
 *   it in a single mutation record.
 *
 * `replaceChildren` is the divergence primitive because the DOM `replace all`
 * algorithm queues ONE record carrying both the removals and the addition, so
 * no intermediate state exists between the two halves.
 */

export const STREAMDOWN_LITERAL_HOST: unique symbol = Symbol.for(
	'svelte-streamdown.literal-host'
);

export interface StreamdownLiteralHostOwner {
	/**
	 * Present the parser's authoritative leaf text. The owner knows what it has
	 * already painted, so it must EXTEND when `text` continues that, and replace
	 * in a single mutation when it does not. It must never remove visible bytes
	 * and re-add them across two mutations.
	 */
	present(text: string): void;
	/** The host is unmounting, or another owner adopted it. */
	release(): void;
}

export interface StreamdownLiteralHost {
	readonly element: HTMLElement;
	/** Leaf text of the last authoritative parser update. */
	readonly text: string;
	/** True while an app-side owner holds presentation. */
	readonly owned: boolean;
	/**
	 * Take single ownership of every child node. Adopting mutates nothing: the
	 * owner inherits exactly what is on screen and is responsible for keeping
	 * the extend-only rule from there.
	 */
	adopt(owner: StreamdownLiteralHostOwner): () => void;
}

interface LiteralHostElement extends HTMLElement {
	[STREAMDOWN_LITERAL_HOST]?: LiteralHostController;
}

/** Distinct from every token identity, including `undefined`. */
const UNPUBLISHED: unique symbol = Symbol('streamdown.literal-host.unpublished');

class LiteralHostController implements StreamdownLiteralHost {
	readonly element: HTMLElement;
	text = '';
	private token: unknown = UNPUBLISHED;
	private base: Text | null = null;
	private owner: StreamdownLiteralHostOwner | null = null;

	constructor(element: HTMLElement) {
		this.element = element;
	}

	get owned(): boolean {
		return this.owner !== null;
	}

	/**
	 * Publish an authoritative parser update. Token identity is half the proof:
	 * equal text under a NEW token is a structural change that still has to
	 * reconcile, and the same token with the same text is an unrelated rerender
	 * that must not disturb a live appended suffix.
	 */
	publish(token: unknown, text: string): void {
		if (token === this.token && text === this.text) return;
		this.token = token;
		this.text = text;
		const owner = this.owner;
		if (owner) {
			owner.present(text);
			return;
		}
		this.render(text);
	}

	/** Unowned presentation: one Text node, written in one mutation record. */
	private render(text: string): void {
		const base = this.base;
		if (base && base.parentNode === this.element && this.element.childNodes.length === 1) {
			if (base.data !== text) base.data = text;
			return;
		}
		const next = (this.element.ownerDocument ?? document).createTextNode(text);
		this.base = next;
		this.element.replaceChildren(next);
	}

	adopt(owner: StreamdownLiteralHostOwner): () => void {
		const previous = this.owner;
		this.owner = owner;
		// Every child belongs to the owner now; the default path must not think
		// it still holds a node it can write through.
		this.base = null;
		if (previous && previous !== owner) previous.release();
		let live = true;
		return () => {
			if (!live) return;
			live = false;
			if (this.owner === owner) this.owner = null;
		};
	}

	detach(): void {
		const owner = this.owner;
		this.owner = null;
		this.base = null;
		this.token = UNPUBLISHED;
		const element = this.element as LiteralHostElement;
		if (element[STREAMDOWN_LITERAL_HOST] === this) {
			delete element[STREAMDOWN_LITERAL_HOST];
		}
		owner?.release();
	}
}

export type StreamdownLiteralHostHandle = StreamdownLiteralHost & {
	publish(token: unknown, text: string): void;
	detach(): void;
};

/** Install the controller. Idempotent for one element. */
export function attachStreamdownLiteralHost(
	element: HTMLElement
): StreamdownLiteralHostHandle {
	const host = element as LiteralHostElement;
	const existing = host[STREAMDOWN_LITERAL_HOST];
	if (existing) return existing;
	const controller = new LiteralHostController(element);
	Object.defineProperty(host, STREAMDOWN_LITERAL_HOST, {
		configurable: true,
		value: controller
	});
	return controller;
}

export function streamdownLiteralHostOf(element: Element): StreamdownLiteralHost | null {
	return (element as LiteralHostElement)[STREAMDOWN_LITERAL_HOST] ?? null;
}
