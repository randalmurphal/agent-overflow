import StreamdownCodeHost from './StreamdownCodeHost.svelte';
import StreamdownMermaidHost from './StreamdownMermaidHost.svelte';
import StreamdownMermaidHostDeferred from './StreamdownMermaidHostDeferred.svelte';
import StreamdownMathHost from './StreamdownMathHost.svelte';
import StreamdownMathHostDeferred from './StreamdownMathHostDeferred.svelte';

export const STREAMDOWN_CONTROLS = Object.freeze({
  code: false,
  table: false,
});

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
