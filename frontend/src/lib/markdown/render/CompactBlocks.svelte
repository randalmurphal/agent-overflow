<script lang="ts">
	import { getAllContexts, mount, onDestroy, onMount, unmount } from 'svelte';
	import Block from './Block.svelte';
	import { useStreamdown } from './context.svelte';
	import {
		acquireDocumentInteraction,
		type DocumentInteraction
	} from './documentInteraction';
	import { lex } from '../parser/index';
	import { reconcileParagraphGapsInNodes } from './paragraphSpacing';
	import { renderStaticTokenHtml } from './staticHtml';

	type Mounted = ReturnType<typeof mount>;
	type StaticRecord = {
		kind: 'static';
		block: string;
		nodes: ChildNode[];
	};
	type ComponentRecord = {
		kind: 'component';
		block: string;
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
		defaultOrigin: unknown;
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
		revision,
		id
	}: {
		root: HTMLElement | undefined;
		blocks: string[];
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
			defaultOrigin: streamdown.defaultOrigin,
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
			left.defaultOrigin === right.defaultOrigin &&
			left.renderHtml === right.renderHtml &&
			left.staticRenderers === right.staticRenderers;
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

	function appendStaticRecord(target: HTMLElement, block: string, html: string): void {
		const { fragment, nodes } = staticNodes(html);
		try {
			target.append(fragment);
			reconcileParagraphGapsInNodes(nodes);
		} catch (error) {
			for (const node of nodes) node.remove();
			throw error;
		}
		records.push({ kind: 'static', block, nodes });
	}

	function appendComponentRecord(target: HTMLElement, block: string): void {
		const start = document.createComment('');
		const end = document.createComment('');
		target.append(start, end);
		let instance: Mounted | undefined;
		try {
			instance = mount(Block, {
				target,
				anchor: end,
				context: contexts,
				props: {
					block,
					static: false,
					compactStaticHtml: false
				}
			});
			const nodes: ChildNode[] = [];
			for (let node = start.nextSibling; node && node !== end; node = node.nextSibling) {
				nodes.push(node);
			}
			reconcileParagraphGapsInNodes(nodes);
			records.push({ kind: 'component', block, start, end, instance, retryStatic: true });
		} catch (error) {
			if (instance) {
				removeComponentRecord({
					kind: 'component',
					block,
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

	function appendRecord(target: HTMLElement, block: string, index: number): void {
		const html = renderStaticTokenHtml(
			lex(block, streamdown.extensions),
			streamdown,
			`${id}-${index}`
		);
		if (html === null) appendComponentRecord(target, block);
		else appendStaticRecord(target, block, html);
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
				`${id}-${index}`
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
			records[index] = { kind: 'static', block: record.block, nodes };
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
		const reset = target !== renderedRoot ||
			configuration === undefined ||
			!sameConfiguration(configuration, nextConfiguration);
		let common = reset ? 0 : Math.min(records.length, blocks.length);
		if (!reset) {
			let index = 0;
			while (index < common && records[index].block === blocks[index]) index++;
			common = index;
		}
		truncateRecords(common);
		if (target !== renderedRoot) renderedRoot = target;
		configuration = nextConfiguration;
		for (let index = common; index < blocks.length; index++) {
			appendRecord(target, blocks[index], index);
		}
		scheduleStaticRetry();
	}

	$effect(() => {
		revision;
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
