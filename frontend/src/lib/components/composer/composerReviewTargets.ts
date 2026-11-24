// `/review` — the second-level completion rows.
//
// Pure. The git data the rows are built from is fetched by the caller
// (`composerSlash.svelte.ts`: `GitListBranches` for branch rows, and
// `ListRecentCommits` — codex's own review-picker source — for commit
// rows); this module only decides what the rows say.
//
// Each row inserts an argument in the grammar the backend reads from the
// sent text (`codexReviewCommandTarget` in `app_codex_review.go`): four
// variants, discriminated by the first argument word, each with its own
// required payload. A bare `/review` is the uncommitted-changes variant.

import type { BranchCommit } from '../../types/git';
import type { ComposerCommandEntry, ComposerCommandSection } from './composerCommandEntries';

export interface ReviewMenuSources {
  branches: readonly { name: string; isCurrent?: boolean; isDefault?: boolean }[];
  commits: readonly BranchCommit[];
  /** True while the git reads are in flight, so the menu can say so. */
  loading: boolean;
  /** Non-empty when a git read failed; rendered instead of an empty section. */
  error: string;
}

/** How many commit rows the menu offers before the list stops being a menu. */
const COMMIT_ROW_LIMIT = 25;

function targetEntry(
  name: string,
  label: string,
  description: string,
  searchText?: string,
): ComposerCommandEntry {
  return {
    kind: 'intercepted',
    name,
    label,
    // The review trigger's range starts at the argument, so the inserted text
    // is the argument alone — never a second `/review`.
    insertText: `${name} `,
    description,
    searchText,
  };
}

/**
 * Rows for `/review <target>`, before filtering.
 *
 * A branch already checked out is still offered: reviewing against the branch
 * you are on is empty, but so is reviewing an unrelated branch, and hiding a
 * name the user can see in the branch picker is more confusing than an empty
 * review.
 */
export function buildReviewSections(sources: ReviewMenuSources): ComposerCommandSection[] {
  const sections: ComposerCommandSection[] = [
    {
      id: 'review-scope',
      header: 'Review',
      entries: [
        targetEntry(
          'uncommitted',
          'Uncommitted changes',
          'Review the working tree as it stands',
        ),
        targetEntry('custom', 'Custom…', 'Describe what to review in your own words'),
      ],
    },
  ];

  if (sources.error !== '') {
    sections.push({
      id: 'review-git-error',
      header: 'Git',
      entries: [
        {
          kind: 'intercepted',
          name: 'git-error',
          label: 'Branches and commits unavailable',
          insertText: '',
          description: sources.error,
          disabled: true,
          disabledReason: sources.error,
        },
      ],
    });
    return sections;
  }

  if (sources.loading) {
    sections.push({
      id: 'review-git-loading',
      header: 'Git',
      entries: [
        {
          kind: 'intercepted',
          name: 'git-loading',
          label: 'Loading branches and commits…',
          insertText: '',
          disabled: true,
          disabledReason: 'Still loading',
        },
      ],
    });
    return sections;
  }

  const branches = sources.branches
    .filter((branch) => branch.name.trim() !== '')
    .map((branch) =>
      targetEntry(
        `branch ${branch.name}`,
        branch.name,
        branch.isDefault ? 'Default branch' : 'Review this branch as the base',
        `branch ${branch.name}`,
      ),
    );
  if (branches.length > 0) {
    sections.push({ id: 'review-branches', header: 'Against branch', entries: branches });
  }

  const commits = sources.commits.slice(0, COMMIT_ROW_LIMIT).map((commit) =>
    targetEntry(
      `commit ${commit.shortSha}`,
      commit.shortSha,
      commit.subject,
      `commit ${commit.shortSha} ${commit.subject}`,
    ),
  );
  if (commits.length > 0) {
    sections.push({ id: 'review-commits', header: 'Commit', entries: commits });
  }
  return sections;
}
