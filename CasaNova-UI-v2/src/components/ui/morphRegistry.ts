// The icons MorphIcon can animate, imported one by one so the bundler can drop
// the rest.
//
// The obvious version of this was `import * as lucide from 'lucide'` and a
// lookup by name. It worked, and it cost 736 kB — 102 kB gzipped — because a
// namespace import pulls in the geometry of every icon in the set. The morphing
// library itself is 7 kB. Almost all of that weight was mine, for eight toggles.
//
// So the names are listed. Adding one is a line here, and the compiler then
// proves it exists rather than leaving a name that silently falls back to a
// static icon at runtime.
//
// These are deliberately only the icons used at genuine STATE toggles. An icon
// that merely distinguishes two different things (a USB stick from an internal
// disk) does not belong here — animating that would claim a transition that
// never happened.

import {
  Check,
  ChevronDown,
  ChevronRight,
  ChevronUp,
  Copy,
  Eye,
  EyeOff,
  LayoutGrid,
  Pencil,
  type IconNode,
} from 'lucide'

export const morphableIcons = {
  Check,
  ChevronDown,
  ChevronRight,
  ChevronUp,
  Copy,
  Eye,
  EyeOff,
  LayoutGrid,
  Pencil,
} satisfies Record<string, IconNode>

/** A name this component knows how to animate. */
export type MorphableIconName = keyof typeof morphableIcons

/** Reports whether a name can be animated, for the runtime fallback. */
export function isMorphable(name: string): name is MorphableIconName {
  return Object.prototype.hasOwnProperty.call(morphableIcons, name)
}
