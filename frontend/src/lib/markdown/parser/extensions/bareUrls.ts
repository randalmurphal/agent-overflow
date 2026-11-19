/**
 * How each forge autolinks a bare URL, as the engine's `bareUrls` option.
 * Both leave GFM's trailing punctuation and an unbalanced `)` out. GitHub
 * links http, https and `www.` only, so `ftp://…` is text. GitLab links
 * every scheme (an `ftp://` URL by the engine's rule, the others by
 * forgeReferenceExtension.ts through `Tokenizer.urlEnd`) and ends a URL
 * before an unbalanced `]` or `}`. Agent chat keeps the engine's rule.
 */
import { backpedalUrl, type BareUrlRule } from '../engine/destination';
import type { Extension } from '../lexer';

const count = (text: string, char: string): number => text.split(char).length - 1;

const GITLAB_CLOSERS: Readonly<Record<string, string>> = { ']': '[', '}': '{' };

const gitlabEnd = (run: string): string => {
	for (;;) {
		const url = backpedalUrl(run);
		const last = url.at(-1) ?? '';
		const opener = GITLAB_CLOSERS[last];
		if (opener === undefined || count(url, last) <= count(url, opener)) return url;
		run = url.slice(0, -1);
	}
};

const GITHUB_BARE_URLS: BareUrlRule = { links: (scheme) => scheme === 'http' || scheme === 'https', end: backpedalUrl };
const GITLAB_BARE_URLS: BareUrlRule = { links: () => true, end: gitlabEnd };

export const markedGithubBareUrls: Extension = { name: 'githubBareUrls', level: 'inline', bareUrls: GITHUB_BARE_URLS };
export const markedGitlabBareUrls: Extension = { name: 'gitlabBareUrls', level: 'inline', bareUrls: GITLAB_BARE_URLS };
