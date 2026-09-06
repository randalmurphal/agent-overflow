/** Files that can open as a page at the owning computer's preview origin. */
export function isHTMLFile(path: string): boolean { return /\.html?$/i.test(path); }
