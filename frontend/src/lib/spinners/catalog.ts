// Built-in working-indicator sprite animations. Each is a horizontal
// strip PNG (frames side by side, equal width, 72px tall = 3x the 24px
// CSS render height, so the art stays crisp on HiDPI). The strips are
// static Vite assets: the browser fetches one only when a sprite
// actually renders, so users with animations off never pay for them.
//
// Frame geometry is baked here rather than measured at runtime — the
// numbers are verified against the committed PNGs by
// catalog.browser.test.ts, so a re-exported strip that disagrees fails
// the gate instead of rendering a smeared cycle.
//
// Custom sprites from <configDir>/spinners/ carry the same fields via
// their JSON sidecar (see the SPINNERS.md the backend seeds there) and
// merge with these in stores/spinners.svelte.ts.

import roboJam from '../../assets/spinners/robo-jam.png';
import roboDance from '../../assets/spinners/robo-dance.png';
import roboTwerk from '../../assets/spinners/robo-twerk.png';
import roboTyping from '../../assets/spinners/robo-typing.png';
import roboPapers from '../../assets/spinners/robo-papers.png';
import roboRepair from '../../assets/spinners/robo-repair.png';
import roboDash from '../../assets/spinners/robo-dash.png';
import roboCollapse from '../../assets/spinners/robo-collapse.png';
import roboMarathon from '../../assets/spinners/robo-marathon.png';
import happyCat from '../../assets/spinners/happy-cat.png';
import nyanCat from '../../assets/spinners/nyan-cat.png';
import partyParrot from '../../assets/spinners/party-parrot.png';
import partyParrotClassic from '../../assets/spinners/party-parrot-classic.png';
import nyanParrot from '../../assets/spinners/nyan-parrot.png';

export interface SpinnerSprite {
  id: string;
  /** Human name for the settings pool list. */
  label: string;
  /** Strip image URL (bundled asset or data URL for customs). */
  src: string;
  frames: number;
  /** Per-frame duration in milliseconds. */
  frameMs: number;
  frameWidth: number;
  frameHeight: number;
  /** True for sprites loaded from <configDir>/spinners/. */
  custom: boolean;
}

function sprite(
  id: string,
  label: string,
  src: string,
  frames: number,
  frameMs: number,
  frameWidth: number,
): SpinnerSprite {
  return { id, label, src, frames, frameMs, frameWidth, frameHeight: 72, custom: false };
}

export const BUILTIN_SPRITES: readonly SpinnerSprite[] = [
  sprite('robo-typing', 'Robot · typing', roboTyping, 6, 200, 75),
  sprite('robo-papers', 'Robot · hauling paperwork', roboPapers, 6, 200, 84),
  sprite('robo-repair', 'Robot · wrench and gears', roboRepair, 6, 200, 72),
  sprite('robo-dash', 'Robot · jet dash', roboDash, 6, 160, 100),
  sprite('robo-collapse', 'Robot · collapse and recover', roboCollapse, 6, 260, 95),
  sprite('robo-marathon', 'Robot · all thirty poses', roboMarathon, 30, 150, 84),
  sprite('robo-jam', 'Robot · full dance set', roboJam, 9, 105, 72),
  sprite('robo-dance', 'Robot · dancing', roboDance, 4, 140, 72),
  sprite('robo-twerk', 'Robot · butt wiggle', roboTwerk, 4, 140, 72),
  sprite('happy-cat', 'Happi the dancing cat', happyCat, 72, 30, 60),
  sprite('nyan-cat', 'Nyan cat', nyanCat, 8, 100, 179),
  sprite('party-parrot', 'Party parrot', partyParrot, 10, 40, 96),
  sprite('party-parrot-classic', 'Party parrot · classic green', partyParrotClassic, 10, 50, 96),
  sprite('nyan-parrot', 'Nyan parrot', nyanParrot, 10, 50, 93),
];

/** The compaction slot's built-in default (settings value ""). */
export const DEFAULT_COMPACTION_SPRITE_ID = 'robo-papers';
