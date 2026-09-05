import { materializeString, matchesProvenAppend, type ProvenAppend } from './provenAppend';

/**
 * Exact source window for incremental parsing; token raws may normalize whitespace.
 * Reclassification into omitted HTML/definitions can move the live boundary
 * backward. Those rare reads use the canonical source, then retain the new window.
 */
export class ParseBlockSource {
    private text = '';
    private fullSource = '';
    private offset = 0;

    reset(text: string): void {
        this.fullSource = text;
        this.text = text;
        this.offset = 0;
    }

    update(fullSource: string, proof?: ProvenAppend): string | null {
        const previous = this.fullSource;
        let delta: string | null = null;
        if (previous.length > 0 && fullSource.length > previous.length) {
            if (proof !== undefined) {
                if (matchesProvenAppend(proof, previous, fullSource)) delta = proof.delta;
            } else if (fullSource.startsWith(previous)) {
                delta = fullSource.slice(previous.length);
            }
        }
        if (delta === null) {
            this.reset(fullSource);
        } else {
            this.fullSource = fullSource;
            this.text += delta;
        }
        return delta;
    }

    charAt(index: number): string | undefined {
        return index < this.offset
            ? this.fullSource[index]
            : this.text[index - this.offset];
    }

    slice(start: number, end?: number): string {
        return start < this.offset
            ? this.fullSource.slice(start, end)
            : this.text.slice(start - this.offset, end === undefined ? undefined : end - this.offset);
    }

    retainFrom(start: number): void {
        if (start !== this.offset) {
            // Detach the suffix from any historical flat document backing it.
            this.text = materializeString(this.slice(start));
            this.offset = start;
        }
    }
}
