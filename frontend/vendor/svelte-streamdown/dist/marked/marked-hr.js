export const markedHr = {
    name: 'hr',
    level: 'block',
    tokenizer(src) {
        // Match horizontal rules according to CommonMark spec:
        // 3 or more matching -, _, or * characters, each optionally followed by
        // spaces/tabs ("- - - -" is a thematic break, not a list). Must be at
        // start of string and match the entire line.
        const match = src.match(/^[ \t]*(?:(?:-[ \t]*){3,}|(?:_[ \t]*){3,}|(?:\*[ \t]*){3,})(?:\n|$)/);
        if (match) {
            const raw = match[0].replace(/\n$/, ''); // Remove trailing newline from raw
            return {
                type: 'hr',
                raw: raw
            };
        }
        return undefined;
    }
};
