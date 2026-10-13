// The Skills settings page's read of a computer's skills library
// (docs/specs/standing-agents.md, "Skills library"). The page holds the
// answer itself while it is open; this module owns the event subscription
// so the component never touches the runtime bus.

import { wailsEventOn } from './wailsEvents';

/**
 * Calls `onChange` whenever a backend reports that an Install, Update or
 * Remove changed a provider home (`skills:changed`). Returns the
 * unsubscribe. The nudge carries nothing, so the caller reads again.
 */
export function onSkillsChanged(onChange: () => void): () => void {
  return wailsEventOn('skills:changed', () => onChange());
}
