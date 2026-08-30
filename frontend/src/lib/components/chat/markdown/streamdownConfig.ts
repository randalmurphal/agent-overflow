import StreamdownCodeHost from './StreamdownCodeHost.svelte';
import StreamdownMermaidHost from './StreamdownMermaidHost.svelte';
import StreamdownMermaidHostDeferred from './StreamdownMermaidHostDeferred.svelte';
import StreamdownMathHost from './StreamdownMathHost.svelte';
import StreamdownMathHostDeferred from './StreamdownMathHostDeferred.svelte';
import { renderCachedStaticCodeBlockHtml } from './staticCodeBlock';
import { createAnimationFrameBatcher } from '../../../utils/animationFrameBatcher';

export const STREAMDOWN_CONTROLS = Object.freeze({
  code: false,
  table: false,
});

export const STREAMDOWN_STATIC_RENDERERS = Object.freeze({
  code: renderCachedStaticCodeBlockHtml,
});

// Completed code islands retire through the app's one frame coordinator.
// One island per Streamdown instance per frame bounds the parse/style burst
// when several active panes finish long highlighted turns together.
export const STREAMDOWN_STATIC_WORK_SCHEDULER = Object.freeze(
  createAnimationFrameBatcher('streamdown-static-retirement'),
);

const COMPLETE_STREAMDOWN_COMPONENTS = Object.freeze({
  code: StreamdownCodeHost,
  mermaid: StreamdownMermaidHost,
  math: StreamdownMathHost,
});

const INCOMPLETE_STREAMDOWN_COMPONENTS = Object.freeze({
  code: StreamdownCodeHost,
  mermaid: StreamdownMermaidHostDeferred,
  math: StreamdownMathHostDeferred,
});

export function streamdownComponentsFor(parseIncompleteMarkdown: boolean) {
  return parseIncompleteMarkdown
    ? INCOMPLETE_STREAMDOWN_COMPONENTS
    : COMPLETE_STREAMDOWN_COMPONENTS;
}
