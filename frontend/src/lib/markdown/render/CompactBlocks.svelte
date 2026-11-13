<script lang="ts">
	import { getAllContexts, mount, onDestroy, onMount, unmount } from 'svelte';
	import Block from './Block.svelte';
	import { useStreamdown } from './context.svelte';
	import {
		acquireDocumentInteraction,
		type DocumentInteraction
	} from './documentInteraction';
	import { lex, type Alignment, type Extension } from '../parser/index';
	import { reconcileParagraphGapsInNodes } from './paragraphSpacing';
	import { renderStaticTokenHtml } from './staticHtml';

	type Mounted = ReturnType<typeof mount>;
	type StaticRecord = {
		kind: 'static';
		block: string;
		align: Alignment | 'marker';
		nodes: ChildNode[];
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
	let configuration: Configuration | undefined;
	let destroyed = false;
	let retryScheduled = false;
	let retryHandle: number | undefined;
	let retryScheduler: StaticWorkScheduler | undefined;
	let retryCursor = 0;
	let documentInteraction: DocumentInteraction | undefined;

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

	function removeComponentRecord(record: ComponentRecord): void {
		try {
			void Promise.resolve(unmount(record.instance)).catch(reportUnmountFailure);
		} catch (error) {
			reportUnmountFailure(error);
		}
		let node: ChildNode | null = record.start;
		while (node) {
			const next: ChildNode | null = node.nextSibling;
			node.remove();
			if (node === record.end) break;
			node = next;
		}
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
	}

	function staticNodes(html: string): { fragment: DocumentFragment; nodes: ChildNode[] } {
		const template = document.createElement('template');
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
		html: string
	): StaticRecord {
		const { fragment, nodes } = staticNodes(html);
		try {
			target.insertBefore(fragment, anchor);
			reconcileParagraphGapsInNodes(nodes);
		} catch (error) {
			for (const node of nodes) node.remove();
			throw error;
		}
		return { kind: 'static', block, align, nodes };
	}

	function insertComponentRecord(
		target: HTMLElement,
		anchor: ChildNode | null,
		block: string,
		align: Alignment
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
			return { kind: 'component', block, align, start, end, instance, retryStatic: true };
		} catch (error) {
			if (instance) {
				removeComponentRecord({
					kind: 'component',
					block,
					align,
					start,
					end,
					instance,
					retryStatic: false
				});
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
		if (align === 'marker') return { kind: 'static', block, align, nodes: [] };
		const html = renderStaticTokenHtml(
			lex(block, streamdown.extensions),
			streamdown,
			`${id}-${index}`,
			align
		);
		return html === null
			? insertComponentRecord(target, anchor, block, align)
			: insertStaticRecord(target, anchor, block, align, html);
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
		if (destroyed) return false;
		let attempts = 0;
		while (retryCursor < records.length) {
			const index = retryCursor++;
			const record = records[index];
			if (record.kind !== 'component' || !record.retryStatic) continue;
			if (recordHasActiveInteraction(record)) continue;
			attempts++;
			const html = renderStaticTokenHtml(
				lex(record.block, streamdown.extensions),
				streamdown,
				`${id}-${index}`,
				record.align
			);
			if (html === null) {
				record.retryStatic = false;
				if (attempts >= limit) return retryCursor < records.length;
				continue;
			}
			record.retryStatic = false;

			const { fragment, nodes } = staticNodes(html);
			try {
				record.start.before(fragment);
				reconcileParagraphGapsInNodes(nodes);
			} catch (error) {
				for (const node of nodes) node.remove();
				throw error;
			}
			removeComponentRecord(record);
			records[index] = { kind: 'static', block: record.block, align: record.align, nodes };
			if (attempts >= limit) return retryCursor < records.length;
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
		if (destroyed || retryScheduled) return;
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
		for (const record of records) {
			if (record.kind === 'component') record.retryStatic = true;
		}
		restartStaticRetry();
	}

	function reconcile(target: HTMLElement, nextConfiguration: Configuration): void {
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
		let replaced = false;
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
		for (let index = common; index < blocks.length; index++) {
			records.push(buildRecord(target, null, blocks[index], alignments[index], index));
		}
		// A rebuilt record may be a component record behind the retry cursor.
		if (replaced) restartStaticRetry();
		else scheduleStaticRetry();
	}

	$effect(() => {
		void revision;
		const target = root;
		const nextConfiguration = readConfiguration();
		if (!target || destroyed) return;
		reconcile(target, nextConfiguration);
	});

	$effect(() => {
		if (streamdown.pendingAsyncCount === 0) rearmStaticRetries();
	});

	onMount(() => {
		const ownerDocument = root?.ownerDocument ?? document;
		documentInteraction = acquireDocumentInteraction(ownerDocument, restartStaticRetry);
		const releaseStaticRetry = streamdown.registerStaticRetry(rearmStaticRetries);
		return () => {
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
		renderedRoot = undefined;
		configuration = undefined;
	});
</script>
