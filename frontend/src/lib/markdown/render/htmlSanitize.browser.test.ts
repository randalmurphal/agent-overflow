import { describe, expect, it } from 'vitest';
import { parseHtmlRegion, sanitizeEmbeddedHtmlToken } from './htmlSanitize';

// What only the platform HTML parser shows: its foreign-content elements and
// its limits.

describe('sanitizeEmbeddedHtmlToken in a real parser', () => {
	it('reads a MathML template as an ordinary element', () => {
		const out = sanitizeEmbeddedHtmlToken({
			raw: '<div>before<math><template><mi>label</mi></template></math>after</div>',
			block: true
		});
		expect(out).toBe('<div>beforelabelafter</div>');
	});

	it('reads an HTML template through its content', () => {
		const out = sanitizeEmbeddedHtmlToken({ raw: '<div><template><b>kept</b></template></div>', block: true });
		expect(out).toBe('<div><b>kept</b></div>');
	});
});

describe('parseHtmlRegion in a real parser', () => {
	it('moves more children than a call takes arguments', () => {
		const count = 140_000;
		const parsed = parseHtmlRegion(['<br>'.repeat(count) + '<div>', {}, '</div>'], sanitizeEmbeddedHtmlToken);
		expect(parsed.root.querySelectorAll('br').length).toBe(count);
		expect(parsed.islands.size).toBe(1);
	});
});
