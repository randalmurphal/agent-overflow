type Listener = () => void;

export type InteractionRange = {
	readonly range: Range;
	readonly endpointAncestors: WeakSet<Node>;
};

type TrackerState = {
	document: Document;
	ranges: InteractionRange[];
	focusedAncestors: WeakSet<Node>;
	selectionPending: boolean;
	listeners: Set<Listener>;
	references: number;
	onSelectionStart: () => void;
	onSelectionChange: () => void;
	onFocusIn: (event: FocusEvent) => void;
	onFocusOut: (event: FocusEvent) => void;
	onPointerDown: (event: PointerEvent) => void;
};

export type DocumentInteraction = {
	readonly ranges: readonly InteractionRange[];
	readonly focusedAncestors: WeakSet<Node>;
	readonly selectionPending: boolean;
	release(): void;
};

const trackers = new WeakMap<Document, TrackerState>();

function notify(state: TrackerState): void {
	for (const listener of state.listeners) listener();
}

function nodeAncestors(...nodes: Array<Node | null>): WeakSet<Node> {
	const ancestors = new WeakSet<Node>();
	for (const node of nodes) {
		let current: Node | null = node;
		while (current) {
			ancestors.add(current);
			current = current.parentNode ?? (current as ShadowRoot).host ?? null;
		}
	}
	return ancestors;
}

function eventNode(target: EventTarget | null): Node | null {
	return target && typeof (target as Node).nodeType === 'number' ? target as Node : null;
}

function captureSelection(state: TrackerState): void {
	const selection = state.document.getSelection();
	const ranges: InteractionRange[] = [];
	if (selection && !selection.isCollapsed) {
		for (let index = 0; index < selection.rangeCount; index++) {
			const range = selection.getRangeAt(index).cloneRange();
			ranges.push({
				range,
				endpointAncestors: nodeAncestors(range.startContainer, range.endContainer)
			});
		}
	}
	state.ranges = ranges;
	state.selectionPending = false;
}

function createTracker(document: Document): TrackerState {
	const state: TrackerState = {
		document,
		ranges: [],
		focusedAncestors: nodeAncestors(document.activeElement),
		selectionPending: false,
		listeners: new Set<Listener>(),
		references: 0,
		onSelectionStart: () => {
			state.selectionPending = true;
			notify(state);
		},
		onSelectionChange: () => {
			captureSelection(state);
			notify(state);
		},
		onFocusIn: (event) => {
			state.focusedAncestors = nodeAncestors(eventNode(event.target));
			notify(state);
		},
		onFocusOut: (event) => {
			const next = eventNode(event.relatedTarget) ?? document.activeElement;
			state.focusedAncestors = nodeAncestors(next);
			notify(state);
		},
		onPointerDown: (event) => {
			state.focusedAncestors = nodeAncestors(eventNode(event.target));
			notify(state);
		},
	};
	captureSelection(state);
	document.addEventListener('selectstart', state.onSelectionStart);
	document.addEventListener('selectionchange', state.onSelectionChange);
	document.addEventListener('focusin', state.onFocusIn);
	document.addEventListener('focusout', state.onFocusOut);
	document.addEventListener('pointerdown', state.onPointerDown);
	trackers.set(document, state);
	return state;
}

function destroyTracker(state: TrackerState): void {
	state.document.removeEventListener('selectstart', state.onSelectionStart);
	state.document.removeEventListener('selectionchange', state.onSelectionChange);
	state.document.removeEventListener('focusin', state.onFocusIn);
	state.document.removeEventListener('focusout', state.onFocusOut);
	state.document.removeEventListener('pointerdown', state.onPointerDown);
	trackers.delete(state.document);
}

/**
 * Keeps the live Selection API off append/retirement hot paths. Blink flushes
 * pending style when Selection.isCollapsed is read. Selection changes are
 * sparse document events, so one shared snapshot preserves active ranges
 * without turning every completed code block into a whole-message style pass.
 */
export function acquireDocumentInteraction(
	document: Document,
	listener: Listener,
): DocumentInteraction {
	const state = trackers.get(document) ?? createTracker(document);
	state.references++;
	state.listeners.add(listener);
	let released = false;
	return {
		get ranges() {
			return state.ranges;
		},
		get focusedAncestors() {
			return state.focusedAncestors;
		},
		get selectionPending() {
			return state.selectionPending;
		},
		release() {
			if (released) return;
			released = true;
			state.listeners.delete(listener);
			state.references--;
			if (state.references === 0) destroyTracker(state);
		},
	};
}
