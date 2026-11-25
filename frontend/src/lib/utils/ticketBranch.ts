// The branch Start work names after a ticket (docs/specs/issues.md,
// "Ticket"): a project's template (trackerapp.StartWorkDefaults.Branch)
// with {key} the ticket's key as the tracker spells it and {title} its
// title made a branch name. Git checks the name when the thread is made.

export const DEFAULT_BRANCH_TEMPLATE = '{key}-{title}';

/** The longest {title} part, cut at a word. */
const TITLE_MAX = 40;

/** A title as a branch name's part: lower case, every run of characters
 * that are not ASCII letters or digits one dash, cut at a word within
 * TITLE_MAX characters. */
export function titleSlug(title: string): string {
  const slug = title.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
  if (slug.length <= TITLE_MAX) return slug;
  const cut = slug.slice(0, TITLE_MAX + 1);
  const at = cut.lastIndexOf('-');
  return (at > 0 ? cut.slice(0, at) : slug.slice(0, TITLE_MAX)).replace(/-+$/, '');
}

/** The branch `template` names for the ticket; an empty template is the
 * default. A token with nothing in it leaves no dash at either end. */
export function ticketBranchName(template: string, key: string, title: string): string {
  const name = (template || DEFAULT_BRANCH_TEMPLATE).replaceAll('{key}', key).replaceAll('{title}', titleSlug(title));
  return name.replace(/-{2,}/g, '-').replace(/^-+|-+$/g, '');
}
