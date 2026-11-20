<script lang="ts">
	import type { Snippet } from 'svelte';
	import Link from './Link.svelte';
	import Image from './Image.svelte';
	import Alert from './Alert.svelte';
	import HtmlRegion from './HtmlRegion.svelte';
	import SourceText from './SourceText.svelte';
	import type { GenericToken, ProvenAppend, StreamdownToken } from '../../parser/index';
	import Slot from './Slot.svelte';
	import { useBlockScope, useStreamdown } from '../context.svelte';
	import { codespanClass } from '../codespan';
	import FootnoteRef from './FootnoteRef.svelte';
	import { classifyLinkHref, tableCellRenders } from './url';
	import { labelChipBorderStyle, labelChipKeyStyle } from '../../parser/extensions/labelChip';
	import { headingAnchor, htmlAnchorName, readingFor, renderUnder } from '../../parser/documentScope';
	import { fenceLanguage } from '../../parser/fenceLanguage';
	let {
		token,
		children,
		codeTextAppend
	}: {
		token: StreamdownToken;
		children: Snippet;
		codeTextAppend?: ProvenAppend;
	} = $props();
	const streamdown = useStreamdown();
	const blockScope = useBlockScope();

	// The host owns code, mermaid and math rendering: the library ships no
	// highlighter, no diagram engine and no TeX typesetter of its own. With
	// no host component the block falls back to its own source text.
	const CodeComponent = $derived(streamdown.components?.code);
	const MermaidComponent = $derived(streamdown.components?.mermaid);
	const MathComponent = $derived(streamdown.components?.math);

	const id = $props.id();

	// Compile-time exhaustiveness: a StreamdownToken type with no branch
	// above fails the type check here instead of rendering nothing.
	function extensionToken(token: never): GenericToken {
		return token;
	}
</script>

{#if token.type === 'heading'}
	<Slot
		props={{
			children,
			token
		}}
		render={streamdown.snippets.heading}
	>
		{#if token.depth === 1}
			<h1 data-streamdown-heading-1={id} data-md-anchor={headingAnchor(token)} class={streamdown.theme[`h${token.depth}`].base}>
				{@render children()}
			</h1>
		{:else if token.depth === 2}
			<h2 data-streamdown-heading-2={id} data-md-anchor={headingAnchor(token)} class={streamdown.theme[`h${token.depth}`].base}>
				{@render children()}
			</h2>
		{:else if token.depth === 3}
			<h3 data-streamdown-heading-3={id} data-md-anchor={headingAnchor(token)} class={streamdown.theme[`h${token.depth}`].base}>
				{@render children()}
			</h3>
		{:else if token.depth === 4}
			<h4 data-streamdown-heading-4={id} data-md-anchor={headingAnchor(token)} class={streamdown.theme[`h${token.depth}`].base}>
				{@render children()}
			</h4>
		{:else if token.depth === 5}
			<h5 data-streamdown-heading-5={id} data-md-anchor={headingAnchor(token)} class={streamdown.theme[`h${token.depth}`].base}>
				{@render children()}
			</h5>
		{:else if token.depth === 6}
			<h6 data-streamdown-heading-6={id} data-md-anchor={headingAnchor(token)} class={streamdown.theme[`h${token.depth}`].base}>
				{@render children()}
			</h6>
		{/if}
	</Slot>
{:else if token.type === 'paragraph'}
	<Slot props={{ children, token }} render={streamdown.snippets.paragraph}>
		<p
			data-streamdown-paragraph={id}
			class={`${streamdown.theme.paragraph.base}${streamdown.parseIncompleteMarkdown === true ? ' sd-volatile-paragraph' : ''}`}
		>
			{@render children()}
		</p>
	</Slot>
{:else if token.type === 'align'}
	<!-- A marker renders nothing; Block.svelte applies its alignment. -->
{:else if token.type === 'blockquote'}
	<Slot props={{ children, token }} render={streamdown.snippets.blockquote}>
		<blockquote data-streamdown-blockquote={id} class={streamdown.theme.blockquote.base}>
			{@render children()}
		</blockquote>
	</Slot>
{:else if token.type === 'code' && fenceLanguage(token.lang) === 'mermaid'}
	<Slot props={{ children, token }} render={streamdown.snippets.code}>
		{#if MermaidComponent}
			<MermaidComponent {id} {token} />
		{:else}
			<div data-streamdown-mermaid={id}><SourceText text={token.text} /></div>
		{/if}
	</Slot>
{:else if token.type === 'code'}
	<Slot props={{ children, token }} render={streamdown.snippets.code}>
		{#if CodeComponent}
			<CodeComponent {id} {token} textAppend={codeTextAppend} />
		{:else}
			<div data-streamdown-code={id} class={streamdown.theme.code.base}>
				<SourceText text={token.text} />
			</div>
		{/if}
	</Slot>
{:else if token.type === 'codespan'}
	<Slot props={{ children, token }} render={streamdown.snippets.codespan}>
		<code
			data-streamdown-codespan={id}
			class={codespanClass(streamdown.theme.codespan.base, token.text)}
		>
			{@render children()}{#if token.color}<span
					data-streamdown-color-swatch
					aria-hidden="true"
					style:background-color={token.color}
				></span>{/if}
		</code>
	</Slot>
{:else if token.type === 'list'}
	{#if token.ordered}
		<Slot props={{ children, token }} render={streamdown.snippets.ol}>
			<ol
				data-streamdown-ol={id}
				start={token.start === 1 ? undefined : token.start}
				style:list-style-type={token.listType}
				class={streamdown.theme.ol.base}
			>
				{@render children()}
			</ol>
		</Slot>
	{:else}
		<Slot props={{ children, token }} render={streamdown.snippets.ul}>
			<ul data-streamdown-ul={id} class={streamdown.theme.ul.base}>
				{@render children()}
			</ul>
		</Slot>
	{/if}
{:else if token.type === 'list_item'}
	<Slot props={{ children, token }} render={streamdown.snippets.li}>
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
			{@render children()}
		</li>
	</Slot>
{:else if token.type === 'table'}
	<Slot props={{ token, children }} render={streamdown.snippets.table}>
		<div
			data-streamdown-table={id}
			class={`${streamdown.theme.table.base} group`}
			style:overscroll-behavior-x="none"
		>
			<table class={streamdown.theme.table.table}>
				{@render children()}
			</table>
		</div>
	</Slot>
{:else if token.type === 'caption'}
	<caption data-streamdown-caption={id}>{@render children()}</caption>
{:else if token.type === 'thead'}
	<Slot props={{ token, children }} render={streamdown.snippets.thead}>
		<thead data-streamdown-thead={id} class={streamdown.theme.thead.base}>
			{@render children?.()}
		</thead>
	</Slot>
{:else if token.type === 'tbody'}
	<Slot props={{ token, children }} render={streamdown.snippets.tbody}>
		<tbody data-streamdown-tbody={id} class={streamdown.theme.tbody.base}>
			{@render children?.()}
		</tbody>
	</Slot>
{:else if token.type === 'tfoot'}
	<Slot props={{ token, children }} render={streamdown.snippets.tfoot}>
		<tfoot data-streamdown-tfoot={id} class={streamdown.theme.tfoot.base}>
			{@render children?.()}
		</tfoot>
	</Slot>
{:else if token.type === 'tr'}
	<Slot props={{ token, children }} render={streamdown.snippets.tr}>
		<tr data-streamdown-tr={id} class={streamdown.theme.tr.base}>
			{@render children?.()}
		</tr>
	</Slot>
{:else if token.type === 'td'}
	{#if tableCellRenders(token)}
		<Slot props={{ children, token }} render={streamdown.snippets.td}>
			<td
				data-streamdown-td={id}
				class={streamdown.theme.td.base}
				{...token.colspan > 1 ? { colspan: token.colspan } : {}}
				{...token.rowspan > 1 ? { rowspan: token.rowspan } : {}}
				{...token.align && ['left', 'center', 'right', 'justify', 'char'].includes(token.align)
					? { align: token.align as 'left' | 'center' | 'right' | 'justify' | 'char' }
					: { align: 'left' }}
			>
				{@render children?.()}
			</td>
		</Slot>
	{/if}
{:else if token.type === 'th'}
	{#if tableCellRenders(token)}
		<Slot props={{ children, token }} render={streamdown.snippets.th}>
			<th
				data-streamdown-th={id}
				class={streamdown.theme.th.base}
				{...token.colspan > 1 ? { colspan: token.colspan } : {}}
				{...token.rowspan > 1 ? { rowspan: token.rowspan } : {}}
				{...token.align && ['left', 'center', 'right', 'justify', 'char'].includes(token.align)
					? { align: token.align as 'left' | 'center' | 'right' | 'justify' | 'char' }
					: { align: 'left' }}
			>
				{@render children?.()}
			</th>
		</Slot>
	{/if}
{:else if token.type === 'image'}
	<Image {id} {token} {children} />
{:else if token.type === 'link'}
	<Link {id} {token} {children} />
{:else if token.type === 'sub'}
	<Slot props={{ children, token }} render={streamdown.snippets.sub}>
		<sub data-streamdown-sub={id} class={streamdown.theme.sub.base}>
			{@render children()}
		</sub>
	</Slot>
{:else if token.type === 'sup'}
	<Slot props={{ children, token }} render={streamdown.snippets.sup}>
		<sup data-streamdown-sup={id} class={streamdown.theme.sup.base}>
			{@render children()}
		</sup>
	</Slot>
{:else if token.type === 'strong'}
	<Slot props={{ children, token }} render={streamdown.snippets.strong}>
		<strong data-streamdown-strong={id} class={streamdown.theme.strong.base}>
			{@render children()}
		</strong>
	</Slot>
{:else if token.type === 'em'}
	<Slot props={{ children, token }} render={streamdown.snippets.em}>
		<em data-streamdown-em={id} class={streamdown.theme.em.base}>
			{@render children()}
		</em>
	</Slot>
{:else if token.type === 'del'}
	<Slot props={{ children, token }} render={streamdown.snippets.del}>
		<del data-streamdown-del={id} class={streamdown.theme.del.base}>
			{@render children()}
		</del>
	</Slot>
{:else if token.type === 'hr'}
	<Slot props={{ children, token }} render={streamdown.snippets.hr}>
		<hr data-streamdown-hr={id} class={streamdown.theme.hr.base} />
	</Slot>
{:else if token.type === 'br'}
	<br data-streamdown-br={id} />
{:else if token.type === 'checkbox'}
	<input
		data-streamdown-checkbox={id}
		disabled
		type="checkbox"
		checked={token.checked}
		class={streamdown.theme.li.checkbox}
	/>
{:else if token.type === 'math'}
	<Slot
		props={{
			children,
			token
		}}
		render={streamdown.snippets.math}
	>
		{#if MathComponent}
			<MathComponent {id} {token} />
		{:else if token.isInline}
			<span data-streamdown-inline-math={id} class={streamdown.theme.math.inline}
				><SourceText text={token.text} inline /></span
			>
		{:else}
			<div data-streamdown-block-math={id}><SourceText text={token.text} /></div>
		{/if}
	</Slot>
{:else if token.type === 'alert'}
	<Alert {id} {token} {children} />
{:else if token.type === 'footnoteRef'}
	<Slot props={{ token }} render={streamdown.snippets.footnoteRef}>
		<FootnoteRef {token} />
	</Slot>
{:else if token.type === 'inline-citations'}
	<!--
		The `citations` tokenizer stays (removing it would change how
		`[foo]`-shaped prose parses), but the library renders nothing for
		it: a host that wants citations supplies the `inlineCitation`
		snippet.
	-->
	{@render streamdown.snippets.inlineCitation?.({ token, children })}
{:else if token.type === 'footnote'}
	<!-- Footnote bodies render from the document's footnote list, not here. -->
{:else if token.type === 'descriptionList'}
	<Slot props={{ children, token }} render={streamdown.snippets.descriptionList}>
		<dl data-streamdown-description-list={id} class={streamdown.theme.descriptionList.base}>
			{@render children()}
		</dl>
	</Slot>
{:else if token.type === 'description'}
	<Slot props={{ children, token }} render={streamdown.snippets.description}>
		{@render children()}
	</Slot>
{:else if token.type === 'descriptionTerm'}
	<Slot props={{ children, token }} render={streamdown.snippets.descriptionTerm}>
		<dt data-streamdown-description-term={id} class={streamdown.theme.descriptionTerm.base}>
			{@render children()}
		</dt>
	</Slot>
{:else if token.type === 'descriptionDetail'}
	<Slot props={{ children, token }} render={streamdown.snippets.descriptionDetail}>
		<dd
			data-streamdown-description-detail={id}
			class={streamdown.theme.descriptionDetail.base}
		>
			{@render children()}
		</dd>
	</Slot>
{:else if token.type === 'def'}
	<!-- TODO This does not seems to be tokenized for now -->
{:else if token.type === 'escape'}
	{token.text}
{:else if token.type === 'space'}
	<!-- TODO This does not seems to be tokenized for now -->
{:else if token.type === 'text'}
	{@render children()}
{:else if token.type === 'details'}
	<!-- Embedded forge HTML (opt-in; parser/extensions/embeddedHtml.ts).
	     Chrome comes from the `.markdown-body details` cascade rules, so
	     no theme slot exists for these two. -->
	<details data-streamdown-details={id} class="md-blk" open={token.open}>
		{@render children()}
	</details>
{:else if token.type === 'detailsSummary'}
	<summary data-streamdown-summary={id}>
		{@render children()}
	</summary>
{:else if token.type === 'inlineDiff'}
	<!-- GitLab's inline diff (parser/extensions/inlineDiff.ts); app.css paints the class. -->
	<svelte:element
		this={token.change === 'added' ? 'ins' : 'del'}
		data-streamdown-inline-diff={id}
		class={`md-inline-diff md-inline-diff-${token.change}`}
	>
		{@render children()}
	</svelte:element>
{:else if token.type === 'labelChip'}
	<!-- A label chip (parser/extensions/labelChip.ts); staticHtml.ts renders the same markup. -->
	{@const chipLink = classifyLinkHref(token.href, streamdown.allowedLinkPrefixes ?? [], !!streamdown.document?.forge)}
	<svelte:element
		this={chipLink.kind === 'anchor' ? 'a' : 'span'}
		data-streamdown-label-chip={id}
		class={token.scope === null ? 'md-label-chip' : 'md-label-chip md-label-chip-scoped'}
		href={chipLink.kind === 'anchor' ? chipLink.href : undefined}
		target={chipLink.kind === 'anchor' ? '_blank' : undefined}
		rel={chipLink.kind === 'anchor' ? 'noopener noreferrer' : undefined}
		title={token.title ?? undefined}
		style={labelChipBorderStyle(token) ?? undefined}
		><span class="md-label-chip-key" style={labelChipKeyStyle(token) ?? undefined}>{token.scope ?? token.name}</span
		>{#if token.scope !== null}<span class="md-label-chip-value">{token.name}</span>{/if}</svelte:element
	>
{:else if token.type === 'codeSnippet'}
	<!-- A file excerpt (parser/extensions/codeSnippet.ts): its header and
	     code render as themselves; staticHtml.ts renders the same markup. -->
	<div data-streamdown-code-snippet={id} class="md-blk md-code-snippet">
		{@render children()}
	</div>
{:else if token.type === 'htmlInline'}
	<!-- The tag is one of the parser's fixed names, never authored text. -->
	<!-- Its attributes are the ones the HTML policy keeps (htmlPolicy.ts). -->
	<svelte:element this={token.tag} {...token.attrs} data-streamdown-html-inline={id} data-md-anchor={htmlAnchorName(token.raw, streamdown.document?.forge)}>
		{@render children()}
	</svelte:element>
{:else if token.type === 'html'}
	{#if streamdown.renderHtml}
		<!-- The block's reading stays the same object while every key the
		     last render read resolves alike, so only such a change renders
		     (sanitizes) the token again. A render is a fresh box: a derived
		     whose value comes out equal keeps its old version, and Svelte
		     would run it again each time the reading is checked; the box's
		     string is what the page compares. -->
		{@const render = streamdown.renderHtml}
		{@const reading = readingFor(token, blockScope())}
		{@const rendered = { html: typeof render === 'function' ? renderUnder(reading, () => render(token, reading)) : token.raw }}
		{@const content = rendered.html}
		<!-- The host may own the injection (to hydrate claimed media inside
		     the fragment); the fallback is the plain injection. -->
		<Slot props={{ children, token, content }} render={streamdown.snippets.html}>
			{@html content}
		</Slot>
	{/if}
{:else if token.type === 'htmlRegion'}
	<!-- Without `renderHtml`, a region shows its Markdown islands and no HTML. -->
	{#if streamdown.renderHtml}
		<HtmlRegion {token} />
	{:else}
		{@render children()}
	{/if}
{:else if token.type === 'mdx'}
	{@const Component = streamdown.mdxComponents?.[token.tagName]}
	{#if Component}
		<Component {token} {children} props={token.attributes} />
	{:else}
		<Slot props={{ token, children, props: token.attributes }} render={streamdown.snippets.mdx}>
			{@render children()}
		</Slot>
	{/if}
{:else}
	<!-- Every StreamdownToken type has a branch above (`extensionToken`
	     takes `never`); a host extension's own token type reaches the
	     host's `children` snippet. -->
	{@render streamdown.children?.({ token: extensionToken(token), children, streamdown })}
{/if}
