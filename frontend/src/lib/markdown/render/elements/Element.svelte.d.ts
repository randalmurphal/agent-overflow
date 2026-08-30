import type { Snippet } from 'svelte';
import type { ProvenAppend, StreamdownToken } from '../marked/index.js';
type $$ComponentProps = {
    token: StreamdownToken;
    children: Snippet;
    codeTextAppend?: ProvenAppend;
};
declare const Element: import("svelte").Component<$$ComponentProps, {}, "">;
type Element = ReturnType<typeof Element>;
export default Element;
