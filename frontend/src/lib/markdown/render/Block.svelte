<script module lang="ts">
	// Shared by every mounted block. Keeping this at module scope avoids one
	// Set allocation for each committed and volatile markdown block.
	const DIRECT_TEXT_CONTAINERS = new Set([
		'paragraph',
		'heading',
		'blockquote',
		'text',
		'escape',
		'strong',
		'em',
		'del',
		'codespan',
		'list',
		'list_item',
		'table',
		'thead',
		'tbody',
		'tfoot',
		'tr',
		'td',
		'th',
		'sub',
		'sup',
		'alert',
		'descriptionList',
		'description',
		'descriptionTerm',
		'descriptionDetail'
	]);
</script>

<script lang="ts">
	import { parseIncompleteMarkdown } from '../parser/incompleteMarkdown';
	import Element from './elements/Element.svelte';
	import {
		createIncrementalLexCache,
		incrementalLex,
		type IncrementalLexObserver,
		type ProvenAppend,
		type StreamdownToken
	} from '../parser/index';
	import LiteralHost from './LiteralHost.svelte';
	import { useStreamdown } from './context.svelte';
	import { renderStaticTokenHtml } from './staticHtml';

	let {
		block,
		append,
		static: isStatic = false,
		directAppendTail = false,
		compactStaticHtml = false
	}: {
		block: string;
		append?: ProvenAppend;
		static?: boolean;
		directAppendTail?: boolean;
		compactStaticHtml?: boolean;
	} = $props();

	const streamdown = useStreamdown();
	// Per-instance incremental state: a streaming list block re-lexes only
	// from its last item per update, and sealed items keep their token
	// references so their subtrees below never re-evaluate. Non-reactive by
	// design — incrementalLex is idempotent for a given (block, extensions),
	// so mutation inside the $derived is safe under re-evaluation.
	const diagnosticContext = streamdown as typeof streamdown & {
		__observeIncrementalLex?: IncrementalLexObserver;
	};
	const lexCache = createIncrementalLexCache(diagnosticContext.__observeIncrementalLex);
	const tokens = $derived(
		incrementalLex(
			block,
			streamdown.extensions,
			lexCache,
			isStatic || streamdown.parseIncompleteMarkdown === false
				? null
				: parseIncompleteMarkdown,
			append
		)
	);
	const id = $props.id();
	const staticHtml = $derived(
		compactStaticHtml && streamdown.parseIncompleteMarkdown === false
			? renderStaticTokenHtml(tokens, streamdown, id)
			: null
	);

</script>

{#snippet renderChildren(tokens: StreamdownToken[], trailingPath: boolean, safePath: boolean)}
	{#each tokens as token, index}
		{#if token}
			{@const children = (token as any)?.tokens || []}
			{@const isTextOnlyNode = children.length === 0}
			{@const isTrailingToken = trailingPath && index === tokens.length - 1}
			{@const isSafeTokenPath = safePath && DIRECT_TEXT_CONTAINERS.has(token.type)}
			{#if token.type === 'text'}
				{#if isTextOnlyNode}
					{#if isTrailingToken && isSafeTokenPath}
						<LiteralHost text={token.text || ''} {token} />
					{:else}
						{token.text}
					{/if}
				{:else}
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				{/if}
			{:else if token.type === 'heading' && !streamdown.snippets.heading}
				{#if token.depth === 1}
					<h1 data-streamdown-heading-1={id} class={streamdown.theme.h1.base}>
						{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
					</h1>
				{:else if token.depth === 2}
					<h2 data-streamdown-heading-2={id} class={streamdown.theme.h2.base}>
						{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
					</h2>
				{:else if token.depth === 3}
					<h3 data-streamdown-heading-3={id} class={streamdown.theme.h3.base}>
						{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
					</h3>
				{:else if token.depth === 4}
					<h4 data-streamdown-heading-4={id} class={streamdown.theme.h4.base}>
						{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
					</h4>
				{:else if token.depth === 5}
					<h5 data-streamdown-heading-5={id} class={streamdown.theme.h5.base}>
						{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
					</h5>
				{:else if token.depth === 6}
					<h6 data-streamdown-heading-6={id} class={streamdown.theme.h6.base}>
						{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
					</h6>
				{/if}
			{:else if token.type === 'paragraph' && !streamdown.snippets.paragraph}
				<p
					data-streamdown-paragraph={id}
					class={`${streamdown.theme.paragraph.base}${streamdown.parseIncompleteMarkdown === true ? ' sd-volatile-paragraph' : ''}`}
				>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</p>
			{:else if token.type === 'blockquote' && !streamdown.snippets.blockquote}
				<blockquote data-streamdown-blockquote={id} class={streamdown.theme.blockquote.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</blockquote>
			{:else if token.type === 'codespan' && !streamdown.snippets.codespan}
				<code data-streamdown-codespan={id} class={streamdown.theme.codespan.base}>
					{token.text}
				</code>
			{:else if token.type === 'list' && (
				(token.ordered && !streamdown.snippets.ol) ||
				(!token.ordered && !streamdown.snippets.ul)
			)}
				{#if token.ordered}
					<ol
						data-streamdown-ol={id}
						style:list-style-type={token.listType}
						class={streamdown.theme.ol.base}
					>
						{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
					</ol>
				{:else}
					<ul data-streamdown-ul={id} class={streamdown.theme.ul.base}>
						{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
					</ul>
				{/if}
			{:else if token.type === 'list_item' && !streamdown.snippets.li}
				<li
					data-streamdown-li={id}
					style:list-style-type={token.task ? 'none' : undefined}
					{...token.value && !token.task ? { value: token.value } : {}}
					class={`${streamdown.theme.li.base}${token.task ? ' md-task-list-item' : ''}`}
				>
					{#if token.task}
						<input
							disabled
							type="checkbox"
							checked={token.checked}
							class={streamdown.theme.li.checkbox}
						/>
					{/if}
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</li>
			{:else if token.type === 'table' && !streamdown.snippets.table}
				<div
					data-streamdown-table={id}
					class={`${streamdown.theme.table.base} group`}
					style:overscroll-behavior-x="none"
				>
					<table class={streamdown.theme.table.table}>
						{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
					</table>
				</div>
			{:else if token.type === 'thead' && !streamdown.snippets.thead}
				<thead data-streamdown-thead={id} class={streamdown.theme.thead.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</thead>
			{:else if token.type === 'tbody' && !streamdown.snippets.tbody}
				<tbody data-streamdown-tbody={id} class={streamdown.theme.tbody.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</tbody>
			{:else if token.type === 'tfoot' && !streamdown.snippets.tfoot}
				<tfoot data-streamdown-tfoot={id} class={streamdown.theme.tfoot.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</tfoot>
			{:else if token.type === 'tr' && !streamdown.snippets.tr}
				<tr data-streamdown-tr={id} class={streamdown.theme.tr.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</tr>
			{:else if token.type === 'td' && token.rowspan > 0 && !streamdown.snippets.td}
				<td
					data-streamdown-td={id}
					class={streamdown.theme.td.base}
					{...token.colspan > 1 ? { colspan: token.colspan } : {}}
					{...token.rowspan > 1 ? { rowspan: token.rowspan } : {}}
					{...token.align && ['left', 'center', 'right', 'justify', 'char'].includes(token.align)
						? { align: token.align as 'left' | 'center' | 'right' | 'justify' | 'char' }
						: { align: 'left' }}
				>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</td>
			{:else if token.type === 'th' && token.rowspan > 0 && !streamdown.snippets.th}
				<th
					data-streamdown-th={id}
					class={streamdown.theme.th.base}
					{...token.colspan > 1 ? { colspan: token.colspan } : {}}
					{...token.rowspan > 1 ? { rowspan: token.rowspan } : {}}
					{...token.align && ['left', 'center', 'right', 'justify', 'char'].includes(token.align)
						? { align: token.align as 'left' | 'center' | 'right' | 'justify' | 'char' }
						: { align: 'left' }}
				>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</th>
			{:else if token.type === 'sub' && !streamdown.snippets.sub}
				<sub data-streamdown-sub={id} class={streamdown.theme.sub.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</sub>
			{:else if token.type === 'sup' && !streamdown.snippets.sup}
				<sup data-streamdown-sup={id} class={streamdown.theme.sup.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</sup>
			{:else if token.type === 'strong' && !streamdown.snippets.strong}
				<strong data-streamdown-strong={id} class={streamdown.theme.strong.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</strong>
			{:else if token.type === 'em' && !streamdown.snippets.em}
				<em data-streamdown-em={id} class={streamdown.theme.em.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</em>
			{:else if token.type === 'del' && !streamdown.snippets.del}
				<del data-streamdown-del={id} class={streamdown.theme.del.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</del>
			{:else if token.type === 'hr' && !streamdown.snippets.hr}
				<hr data-streamdown-hr={id} class={streamdown.theme.hr.base} />
			{:else if token.type === 'br'}
				<br data-streamdown-br={id} />
			{:else if token.type === 'descriptionList' && !streamdown.snippets.descriptionList}
				<dl data-streamdown-description-list={id} class={streamdown.theme.descriptionList.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</dl>
			{:else if token.type === 'description' && !streamdown.snippets.description}
				{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
			{:else if token.type === 'descriptionTerm' && !streamdown.snippets.descriptionTerm}
				<dt data-streamdown-description-term={id} class={streamdown.theme.descriptionTerm.base}>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</dt>
			{:else if token.type === 'descriptionDetail' && !streamdown.snippets.descriptionDetail}
				<dd
					data-streamdown-description-detail={id}
					class={streamdown.theme.descriptionDetail.base}
				>
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				</dd>
			{:else}
				<Element
					{token}
					codeTextAppend={
						token.type === 'code' ? lexCache.lastCodeTextAppend : undefined
					}
				>
					{#if isTextOnlyNode}
						{#if isTrailingToken && isSafeTokenPath}
							<LiteralHost text={('text' in token ? token.text : '') || ''} {token} />
						{:else}
							{'text' in token ? token.text : ''}
						{/if}
					{:else}
						{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
					{/if}
				</Element>
			{/if}
		{/if}
	{/each}
{/snippet}

{#if staticHtml !== null}
	{@html staticHtml}
{:else}
	{@render renderChildren(tokens, directAppendTail, true)}
{/if}
