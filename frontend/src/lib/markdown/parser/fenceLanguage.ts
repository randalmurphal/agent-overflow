/**
 * A fenced code block's language: the first word of its info string, as
 * CommonMark and both forges read it (```` ```ruby title="a.rb" ```` is
 * Ruby). `Tokens.Code.lang` holds the whole info string, which hosts read
 * for more than the language (a suggestion fence's mark, a copied fence);
 * every check of which language a fence is asks this function.
 */
export function fenceLanguage(info: string | undefined): string {
	if (!info) return '';
	for (let index = 0; index < info.length; index++) {
		const code = info.charCodeAt(index);
		if (code === 32 /* space */ || code === 9 /* tab */) return info.slice(0, index);
	}
	return info;
}
