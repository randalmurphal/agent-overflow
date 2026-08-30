import type { ProvenAppend } from './marked/index.js';
type $$ComponentProps = {
    block: string;
    append?: ProvenAppend;
    static?: boolean;
    directAppendTail?: boolean;
    compactStaticHtml?: boolean;
};
declare const Block: import("svelte").Component<$$ComponentProps, {}, "">;
type Block = ReturnType<typeof Block>;
export default Block;
