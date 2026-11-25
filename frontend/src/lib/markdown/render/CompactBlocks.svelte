<script lang="ts" module>
	const templatesByDocument = new WeakMap<Document, HTMLTemplateElement>();
	let batchSepSeq = 0;

	function reusableTemplate(ownerDocument: Document): HTMLTemplateElement {
		let template = templatesByDocument.get(ownerDocument);
		if (!template) {
			template = ownerDocument.createElement('template');
			templatesByDocument.set(ownerDocument, template);
		}
		return template;
	}
</script>

<script lang="ts">
	import { getAllContexts, mount, onDestroy, onMount, unmount } from 'svelte';
	import Block from './Block.svelte';
	import { useStreamdown } from './context.svelte';
	import {
		acquireDocumentInteraction,
		type DocumentInteraction
	} from './documentInteraction';
	import {
		lexCapture,
		sameScopeReads,
		type Alignment,
		type DocumentContext,
		type DocumentScope,
		type Extension
	} from '../parser/index';
	import { reconcileParagraphGapsInNodes } from './paragraphSpacing';
	import { renderStaticTokenHtml } from './staticHtml';

	type Mounted = ReturnType<typeof mount>;
	type StaticRecord = {
		kind: 'static';
		block: string;
		align: Alignment | 'marker';
		nodes: ChildNode[];
		/** The document scope it was lexed under and the keys it read. */
		scope: DocumentScope | null;
		reads: Set<string> | null;
	};
	type ComponentRecord = {
		kind: 'component';
		block: string;
		align: Alignment;
		start: Comment;
		end: Comment;
		instance: Mounted;
		retryStatic: boolean;
	};
	type Record = StaticRecord | ComponentRecord;

	type Configuration = {
		extensions: unknown;
		theme: unknown;
		snippets: unknown;
		children: unknown;
		allowedLinkPrefixes: unknown;
		renderHtml: unknown;
		staticRenderers: unknown;
	};
	type StaticWorkScheduler = {
		request(callback: FrameRequestCallback): number;
		cancel(handle: number): void;
	};

	// A single completed code island can materialize syntax spans and controls.
	// Retiring one per owner per frame keeps four simultaneous long-turn settles
	// inside the display budget while still releasing every component promptly.
	// Readiness is per record: one live highlighter must not retain every older
	// island in this Streamdown until the global async settle count reaches zero.
	const SCHEDULED_RETRIES_PER_FRAME = 1;

	let {
		root,
		blocks,
		alignments,
		revision,
		id
	}: {
		root: HTMLElement | undefined;
		blocks: string[];
		/** Parallel to blocks: the alignment in force before each block, or
		 * `marker` for a marker block, which renders nothing. */
		alignments: (Alignment | 'marker')[];
		/** Fresh wrapper published after parseBlocks mutates its stable array. */
		revision: object;
		id: string;
	} = $props();

	const streamdown = useStreamdown();
	const contexts = getAllContexts();
	const records: Record[] = [];
	let renderedRoot: HTMLElement | undefined;
	let renderedDocument: DocumentContext | null | undefined;
	let configuration: Configuration | undefined;
	let destroyed = false;
	let retryScheduled = false;
	let retryHandle: number | undefined;
	let retryScheduler: StaticWorkScheduler | undefined;
	let retryCursor = 0;
	let documentInteraction: DocumentInteraction | undefined;
	let mounted = false;
	let componentCount = 0;
	let retryableComponentCount = 0;

	function syncDocumentInteraction(target?: HTMLElement): void {
		if (!mounted || destroyed || retryableComponentCount === 0) {
			if (documentInteraction) {
				documentInteraction.release();
				documentInteraction = undefined;
			}
			return;
		}
		if (!documentInteraction) {
			const ownerDocument = target?.ownerDocument ?? renderedRoot?.ownerDocument ?? root?.ownerDocument ?? document;
			documentInteraction = acquireDocumentInteraction(ownerDocument, restartStaticRetry);
		}
	}

	function readConfiguration(): Configuration {
		return {
			extensions: streamdown.extensions,
			theme: streamdown.theme,
			snippets: streamdown.snippets,
			children: streamdown.children,
			allowedLinkPrefixes: streamdown.allowedLinkPrefixes,
			renderHtml: streamdown.renderHtml,
			staticRenderers: streamdown.staticRenderers
		};
	}

	function sameConfiguration(left: Configuration, right: Configuration): boolean {
		return left.extensions === right.extensions &&
			left.theme === right.theme &&
			left.snippets === right.snippets &&
			left.children === right.children &&
			left.allowedLinkPrefixes === right.allowedLinkPrefixes &&
			left.renderHtml === right.renderHtml &&
			left.staticRenderers === right.staticRenderers;
	}

	function sameExceptExtensions(left: Configuration, right: Configuration): boolean {
		return sameConfiguration({ ...left, extensions: right.extensions }, right);
	}

	/**
	 * For a change to the extension list alone, returns which block sources
	 * still render as they did. Null when any changed extension cannot say.
	 */
	function extensionChangeKeeps(
		previous: unknown,
		next: unknown
	): ((source: string) => boolean) | null {
		if (!Array.isArray(previous) || !Array.isArray(next) || previous.length !== next.length) {
			return null;
		}
		const changed: { before: Extension; after: Extension }[] = [];
		for (let index = 0; index < next.length; index++) {
			const before = previous[index] as Extension;
			const after = next[index] as Extension;
			if (before === after) continue;
			if (typeof after?.tokenizesAlike !== 'function') return null;
			changed.push({ before, after });
		}
		return (source) => changed.every(({ before, after }) =>
			after.tokenizesAlike?.(before, source) === true
		);
	}

	function reportUnmountFailure(error: unknown): void {
		console.error('[streamdown-compact-blocks] component unmount failed', error);
	}

	function unmountComponentNodes(start: Comment, end: Comment, instance: Mounted): void {
		try {
			void Promise.resolve(unmount(instance)).catch(reportUnmountFailure);
		} catch (error) {
			reportUnmountFailure(error);
		}
		let node: ChildNode | null = start;
		while (node) {
			const next: ChildNode | null = node.nextSibling;
			node.remove();
			if (node === end) break;
			node = next;
		}
	}

	function removeComponentRecord(record: ComponentRecord): void {
		componentCount--;
		if (record.retryStatic) retryableComponentCount--;
		unmountComponentNodes(record.start, record.end, record.instance);
	}

	function removeRecord(record: Record): void {
		if (record.kind === 'component') {
			removeComponentRecord(record);
			return;
		}
		for (const node of record.nodes) node.remove();
	}

	function truncateRecords(length: number): void {
		for (let index = records.length - 1; index >= length; index--) {
			removeRecord(records[index]);
		}
		records.length = length;
		retryCursor = Math.min(retryCursor, length);
		syncDocumentInteraction();
	}

	function staticNodes(ownerDocument: Document, html: string): { fragment: DocumentFragment; nodes: ChildNode[] } {
		const template = reusableTemplate(ownerDocument);
		template.innerHTML = html;
		return {
			fragment: template.content,
			nodes: Array.from(template.content.childNodes)
		};
	}

	// Records are inserted before `anchor`, or appended when it is null.
	function insertStaticRecord(
		target: HTMLElement,
		anchor: ChildNode | null,
		block: string,
		align: Alignment,
		html: string,
		lexed: { scope: DocumentScope | null; reads: Set<string> | null }
	): StaticRecord {
		const { fragment, nodes } = staticNodes(target.ownerDocument, html);
		try {
			target.insertBefore(fragment, anchor);
			reconcileParagraphGapsInNodes(nodes);
		} catch (error) {
			for (const node of nodes) node.remove();
			throw error;
		}
		return { kind: 'static', block, align, nodes, scope: lexed.scope, reads: lexed.reads };
	}

	type PendingStaticEntry =
		| { marker: true; block: string }
		| {
			marker: false;
			block: string;
			align: Alignment;
			html: string;
			scope: DocumentScope | null;
			reads: Set<string> | null;
		};

	// Appends a contiguous run of static blocks at `anchor === null` using one
	// HTML parse and one DocumentFragment insertion instead of one <template>
	// parse + DOM insert per block.
	function flushPendingStaticAppend(target: HTMLElement, pending: PendingStaticEntry[]): void {
		if (pending.length === 0) return;
		let nonMarkerCount = 0;
		for (let i = 0; i < pending.length; i++) {
			if (!pending[i].marker) nonMarkerCount++;
		}
		if (nonMarkerCount <= 1) {
			for (let i = 0; i < pending.length; i++) {
				const entry = pending[i];
				if (entry.marker) {
					records.push({ kind: 'static', block: entry.block, align: 'marker', nodes: [], scope: null, reads: null });
				} else {
					records.push(insertStaticRecord(target, null, entry.block, entry.align, entry.html, entry));
				}
			}
			pending.length = 0;
			return;
		}

		let sep = `ao-sd-sep-${++batchSepSeq}`;
		let collision = true;
		while (collision) {
			collision = false;
			for (let i = 0; i < pending.length; i++) {
				const entry = pending[i];
				if (!entry.marker && entry.html.includes(sep)) {
					sep = `ao-sd-sep-${++batchSepSeq}`;
					collision = true;
					break;
				}
			}
		}

		const parts: string[] = [];
		for (let i = 0; i < pending.length; i++) {
			const entry = pending[i];
			if (!entry.marker) parts.push(entry.html);
		}
		const template = reusableTemplate(target.ownerDocument);
		template.innerHTML = parts.join(`<!--${sep}-->`);
		const fragment = template.content;

		const buckets: ChildNode[][] = [];
		let currentNodes: ChildNode[] = [];
		buckets.push(currentNodes);
		const separators: Comment[] = [];
		for (let node = fragment.firstChild; node !== null; node = node.nextSibling) {
			if (node.nodeType === 8 && (node as Comment).data === sep) {
				separators.push(node as Comment);
				currentNodes = [];
				buckets.push(currentNodes);
			} else {
				currentNodes.push(node);
			}
		}

		if (buckets.length !== nonMarkerCount) {
			template.innerHTML = '';
			for (let i = 0; i < pending.length; i++) {
				const entry = pending[i];
				if (entry.marker) {
					records.push({ kind: 'static', block: entry.block, align: 'marker', nodes: [], scope: null, reads: null });
				} else {
					records.push(insertStaticRecord(target, null, entry.block, entry.align, entry.html, entry));
				}
			}
			pending.length = 0;
			return;
		}

		for (let i = 0; i < separators.length; i++) separators[i].remove();
		try {
			target.insertBefore(fragment, null);
			for (let i = 0; i < buckets.length; i++) {
				reconcileParagraphGapsInNodes(buckets[i]);
			}
		} catch (error) {
			for (let i = 0; i < buckets.length; i++) {
				for (const node of buckets[i]) node.remove();
			}
			throw error;
		}

		let bucketIndex = 0;
		for (let i = 0; i < pending.length; i++) {
			const entry = pending[i];
			if (entry.marker) {
				records.push({ kind: 'static', block: entry.block, align: 'marker', nodes: [], scope: null, reads: null });
			} else {
				records.push({
					kind: 'static',
					block: entry.block,
					align: entry.align,
					nodes: buckets[bucketIndex++],
					scope: entry.scope,
					reads: entry.reads
				});
			}
		}
		pending.length = 0;
	}

	function insertComponentRecord(
		target: HTMLElement,
		anchor: ChildNode | null,
		block: string,
		align: Alignment,
		index: number
	): ComponentRecord {
		const start = document.createComment('');
		const end = document.createComment('');
		target.insertBefore(start, anchor);
		target.insertBefore(end, anchor);
		let instance: Mounted | undefined;
		try {
			instance = mount(Block, {
				target,
				anchor: end,
				context: contexts,
				props: {
					block,
					index,
					align,
					static: false,
					compactStaticHtml: false
				}
			});
			const nodes: ChildNode[] = [];
			for (let node = start.nextSibling; node && node !== end; node = node.nextSibling) {
				nodes.push(node);
			}
			reconcileParagraphGapsInNodes(nodes);
			componentCount++;
			retryableComponentCount++;
			syncDocumentInteraction(target);
			return { kind: 'component', block, align, start, end, instance, retryStatic: true };
		} catch (error) {
			if (instance) {
				unmountComponentNodes(start, end, instance);
			} else {
				start.remove();
				end.remove();
			}
			throw error;
		}
	}

	function buildRecord(
		target: HTMLElement,
		anchor: ChildNode | null,
		block: string,
		align: Alignment | 'marker',
		index: number
	): Record {
		if (align === 'marker') return { kind: 'static', block, align, nodes: [], scope: null, reads: null };
		const scope = scopeAt(index);
		const lexed = lexCapture(block, streamdown.extensions ?? [], null, null, scope);
		const html = renderStaticTokenHtml(
			lexed.tokens,
			streamdown,
			`${id}-${index}`,
			align
		);
		return html === null
			? insertComponentRecord(target, anchor, block, align, index)
			: insertStaticRecord(target, anchor, block, align, html, { scope, reads: lexed.reads });
	}

	function scopeAt(index: number): DocumentScope | null {
		return streamdown.document?.scope(index) ?? null;
	}

	function firstNodeFrom(index: number): ChildNode | null {
		for (let at = index; at < records.length; at++) {
			const record = records[at];
			const node = record.kind === 'component' ? record.start : record.nodes[0];
			if (node) return node;
		}
		return null;
	}

	/** Rebuilds one static record where it stands. The new nodes enter before
	 * the old ones leave, so a failed build leaves the record intact. */
	function replaceStaticRecord(target: HTMLElement, index: number): void {
		const old = records[index];
		const next = buildRecord(target, firstNodeFrom(index), old.block, old.align, index);
		removeRecord(old);
		records[index] = next;
		syncDocumentInteraction(target);
	}

	function recordHasActiveInteraction(record: ComponentRecord): boolean {
		// onMount creates the document snapshot before any async island can
		// retire. If an unusual renderer settles earlier, keeping the island is
		// safer than replacing DOM without knowing whether it owns interaction.
		if (!documentInteraction || documentInteraction.selectionPending) return true;
		const active = record.start.ownerDocument.activeElement;
		const ranges = documentInteraction.ranges;
		const focusedAncestors = documentInteraction.focusedAncestors;
		let node: Node | null = record.start.nextSibling;
		while (node && node !== record.end) {
			if (active && (node === active || (node instanceof Element && node.contains(active)))) {
				return true;
			}
			if (focusedAncestors.has(node)) return true;
			for (const interactionRange of ranges) {
				if (
					interactionRange.endpointAncestors.has(node) ||
					interactionRange.range.intersectsNode(node)
				) return true;
			}
			node = node.nextSibling;
		}
		return false;
	}

	function retryStaticRecords(limit: number): boolean {
		if (destroyed || retryableComponentCount === 0) return false;
		let attempts = 0;
		while (retryCursor < records.length) {
			const index = retryCursor++;
			const record = records[index];
			if (record.kind !== 'component' || !record.retryStatic) continue;
			if (recordHasActiveInteraction(record)) continue;
			attempts++;
			const scope = scopeAt(index);
			const lexed = lexCapture(record.block, streamdown.extensions ?? [], null, null, scope);
			const html = renderStaticTokenHtml(
				lexed.tokens,
				streamdown,
				`${id}-${index}`,
				record.align
			);
			if (html === null) {
				record.retryStatic = false;
				retryableComponentCount--;
				syncDocumentInteraction();
				if (attempts >= limit) return retryableComponentCount > 0 && retryCursor < records.length;
				continue;
			}
			record.retryStatic = false;
			retryableComponentCount--;

			const { fragment, nodes } = staticNodes(record.start.ownerDocument, html);
			try {
				record.start.before(fragment);
				reconcileParagraphGapsInNodes(nodes);
			} catch (error) {
				for (const node of nodes) node.remove();
				record.retryStatic = true;
				retryableComponentCount++;
				throw error;
			}
			removeComponentRecord(record);
			records[index] = { kind: 'static', block: record.block, align: record.align, nodes, scope, reads: lexed.reads };
			syncDocumentInteraction();
			if (attempts >= limit) return retryableComponentCount > 0 && retryCursor < records.length;
		}
		return false;
	}

	function runStaticRetry(): void {
		const scheduler = retryScheduler;
		retryScheduled = false;
		retryHandle = undefined;
		retryScheduler = undefined;
		if (destroyed) return;
		try {
			const hasMore = retryStaticRecords(
				scheduler ? SCHEDULED_RETRIES_PER_FRAME : Number.POSITIVE_INFINITY
			);
			if (hasMore) scheduleStaticRetry();
		} catch (error) {
			console.error('[streamdown-compact-blocks] static retry failed', error);
		}
	}

	function scheduleStaticRetry(): void {
		if (destroyed || retryScheduled || retryableComponentCount === 0) return;
		retryScheduled = true;
		const scheduler = streamdown.staticWorkScheduler;
		if (!scheduler) {
			queueMicrotask(runStaticRetry);
			return;
		}
		retryScheduler = scheduler;
		try {
			retryHandle = scheduler.request(runStaticRetry);
		} catch (error) {
			retryScheduled = false;
			retryScheduler = undefined;
			throw error;
		}
	}

	function restartStaticRetry(): void {
		retryCursor = 0;
		scheduleStaticRetry();
	}

	function rearmStaticRetries(): void {
		if (componentCount === 0) return;
		for (const record of records) {
			if (record.kind === 'component' && !record.retryStatic) {
				record.retryStatic = true;
				retryableComponentCount++;
			}
		}
		syncDocumentInteraction();
		restartStaticRetry();
	}

	function reconcile(
		target: HTMLElement,
		nextConfiguration: Configuration,
		nextDocument: DocumentContext | null
	): void {
		const previous = configuration;
		let reset = target !== renderedRoot || previous === undefined;
		// A changed extension list normally invalidates every record. When
		// each changed extension can name the sources it tokenizes as before,
		// only the other records are rebuilt.
		let keeps: ((source: string) => boolean) | null = null;
		if (!reset && previous && !sameConfiguration(previous, nextConfiguration)) {
			keeps = sameExceptExtensions(previous, nextConfiguration)
				? extensionChangeKeeps(previous.extensions, nextConfiguration.extensions)
				: null;
			reset = keeps === null;
		}
		let common = reset ? 0 : Math.min(records.length, blocks.length);
		if (!reset) {
			let index = 0;
			while (
				index < common &&
				records[index].block === blocks[index] &&
				records[index].align === alignments[index]
			) index++;
			common = index;
		}
		truncateRecords(common);
		if (target !== renderedRoot) renderedRoot = target;
		configuration = nextConfiguration;
		const documentChanged = nextDocument !== renderedDocument;
		renderedDocument = nextDocument;
		let replaced = false;
		// A new document context rebuilds only the kept records whose lex
		// read a key that now resolves differently. A component record's
		// Block reads the context reactively.
		if (documentChanged) {
			for (let index = 0; index < common; index++) {
				const record = records[index];
				if (record.kind !== 'static' || record.align === 'marker') continue;
				const scope = scopeAt(index);
				if (sameScopeReads(record.scope, scope, record.reads)) {
					record.scope = scope;
					continue;
				}
				replaceStaticRecord(target, index);
				replaced = true;
			}
		}
		if (keeps) {
			for (let index = 0; index < common; index++) {
				const record = records[index];
				// A component record's Block reads the extensions reactively.
				if (record.kind !== 'static' || record.align === 'marker') continue;
				if (keeps(record.block)) continue;
				replaceStaticRecord(target, index);
				replaced = true;
			}
		}
		const pendingStatic: PendingStaticEntry[] = [];
		const extensions = streamdown.extensions ?? [];
		for (let index = common; index < blocks.length; index++) {
			const block = blocks[index];
			const align = alignments[index];
			if (align === 'marker') {
				pendingStatic.push({ marker: true, block });
				continue;
			}
			const scope = scopeAt(index);
			const lexed = lexCapture(block, extensions, null, null, scope);
			const html = renderStaticTokenHtml(
				lexed.tokens,
				streamdown,
				`${id}-${index}`,
				align
			);
			if (html === null) {
				flushPendingStaticAppend(target, pendingStatic);
				records.push(insertComponentRecord(target, null, block, align, index));
			} else {
				pendingStatic.push({ marker: false, block, align, html, scope, reads: lexed.reads });
			}
		}
		flushPendingStaticAppend(target, pendingStatic);
		// A rebuilt record may be a component record behind the retry cursor.
		if (replaced) restartStaticRetry();
		else scheduleStaticRetry();
	}

	$effect(() => {
		void revision;
		const target = root;
		const nextConfiguration = readConfiguration();
		const nextDocument = streamdown.document ?? null;
		if (!target || destroyed) return;
		reconcile(target, nextConfiguration, nextDocument);
	});

	$effect(() => {
		if (streamdown.pendingAsyncCount === 0) rearmStaticRetries();
	});

	onMount(() => {
		mounted = true;
		syncDocumentInteraction(root);
		const releaseStaticRetry = streamdown.registerStaticRetry(rearmStaticRetries);
		return () => {
			mounted = false;
			releaseStaticRetry();
			documentInteraction?.release();
			documentInteraction = undefined;
		};
	});

	onDestroy(() => {
		destroyed = true;
		if (retryHandle !== undefined) retryScheduler?.cancel(retryHandle);
		retryScheduled = false;
		retryHandle = undefined;
		retryScheduler = undefined;
		truncateRecords(0);
		documentInteraction?.release();
		documentInteraction = undefined;
		renderedRoot = undefined;
		renderedDocument = undefined;
		configuration = undefined;
	});
</script>
