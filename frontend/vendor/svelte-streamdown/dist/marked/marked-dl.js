// Hoisted to module scope: these are stateless (no `g` flag) and were previously
// recompiled on every tokenizer call (DL_RULE) and every line of a growing list (DL_LINE_RULE).
// The detail group must accept colons (`Time: 10:30`) and mirror DL_RULE exactly:
// any line the block rule consumes must also match here, or it silently vanishes.
const DL_RULE = /^(?:[ \t]*:[^:\n]+:[ \t]?[^\n]*(?:\n|$))+/;
const DL_LINE_RULE = /^\s*:([^:\n]+):([^\n]*)(?:\n|$)/;
export const markedDl = {
    name: 'descriptionList',
    level: 'block', // Is this a block-level or inline-level tokenizer?
    tokenizer(src) {
        const match = DL_RULE.exec(src);
        if (match) {
            const text = match[0].trim();
            const tokens = [];
            // Parse each line as a description
            const lines = text.split('\n');
            for (const line of lines) {
                const lineMatch = DL_LINE_RULE.exec(line);
                if (lineMatch) {
                    const term = lineMatch[1].trim();
                    const detail = lineMatch[2].trim();
                    tokens.push({
                        type: 'description',
                        raw: lineMatch[0],
                        tokens: [
                            {
                                type: 'descriptionTerm',
                                raw: term,
                                tokens: this.lexer.inlineTokens(term)
                            },
                            {
                                type: 'descriptionDetail',
                                raw: detail,
                                tokens: this.lexer.inlineTokens(detail)
                            }
                        ]
                    });
                }
            }
            const token = {
                type: 'descriptionList',
                raw: match[0],
                text: text,
                tokens: tokens
            };
            return token;
        }
    }
};
