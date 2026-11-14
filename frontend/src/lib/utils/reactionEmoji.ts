// Reactions on review comments, by GitLab award name (the backend's
// vocabulary on both forges, gitops.CommentReaction.Name). The eight both
// forges share are the ones the pane adds; any other GitLab award already
// on a comment shows as its emoji when this table knows it, else as
// `:name:`.

/** The reactions both forges take, in the order the picker offers them. */
export const SHARED_REACTIONS = ['thumbsup', 'thumbsdown', 'laughing', 'tada', 'confused', 'heart', 'rocket', 'eyes'] as const;

export type SharedReaction = (typeof SHARED_REACTIONS)[number];

const EMOJI: Readonly<Record<string, string>> = {
  thumbsup: '👍',
  thumbsdown: '👎',
  laughing: '😄',
  tada: '🎉',
  confused: '😕',
  heart: '❤️',
  rocket: '🚀',
  eyes: '👀',
  // Common GitLab awards beyond the shared eight.
  '+1': '👍',
  '-1': '👎',
  '100': '💯',
  smile: '😄',
  smiley: '😃',
  slight_smile: '🙂',
  joy: '😂',
  thinking: '🤔',
  clap: '👏',
  pray: '🙏',
  fire: '🔥',
  sparkles: '✨',
  star: '⭐',
  white_check_mark: '✅',
  heavy_check_mark: '✔️',
  x: '❌',
  warning: '⚠️',
  bug: '🐛',
  ok_hand: '👌',
  wave: '👋',
  raised_hands: '🙌',
  muscle: '💪',
  cry: '😢',
  sob: '😭',
  sweat_smile: '😅',
};

const LABEL: Readonly<Record<SharedReaction, string>> = {
  thumbsup: 'Thumbs up',
  thumbsdown: 'Thumbs down',
  laughing: 'Laugh',
  tada: 'Hooray',
  confused: 'Confused',
  heart: 'Heart',
  rocket: 'Rocket',
  eyes: 'Eyes',
};

/** The emoji for an award name, or `:name:` when the table lacks it. */
export function reactionGlyph(name: string): string {
  return Object.hasOwn(EMOJI, name) ? EMOJI[name] : `:${name}:`;
}

/** Whether the table knows the name's emoji. */
export function reactionKnown(name: string): boolean {
  return Object.hasOwn(EMOJI, name);
}

/** The words for a reaction: a shared one's label, else `:name:`. */
export function reactionLabel(name: string): string {
  return (LABEL as Record<string, string>)[name] ?? `:${name}:`;
}
