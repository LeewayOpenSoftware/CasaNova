// Tests for the icon registry.
//
// Why this file exists: a bad icon name is the quietest failure mode in the
// whole frontend. `<AppIcon name="CalendarClock" />` type-checks (the prop is
// just `string`), builds clean, and renders literally nothing — AppIcon's
// `v-if="iconComponent"` drops the element when the lookup misses. Nobody sees
// it until a user reports a blank square. The same goes for the Phosphor half:
// a key present in `lucideIcons` but missing from the Phosphor map makes that
// pack fall back to a Lucide glyph, so a Phosphor session silently renders a
// mixed-family icon.
//
// The Phosphor map's header has always *stated* that invariant, but nothing
// enforced it. It cannot be enforced from the registry module either, now that
// the Phosphor map sits behind a dynamic import — a module-level check would
// have to await a chunk. So it is enforced here instead, and it is enforced
// against the app's real usage rather than against the maps alone:
//
//   1. the two maps expose exactly the same key set;
//   2. no key in either map resolves to undefined (a renamed upstream export);
//   3. every icon name the app actually asks for exists in the registry.
//
// (3) scans the source tree because there is no runtime seam that sees every
// name: they arrive from templates, from lookup tables, and from helpers that
// return a name per case. The scanner below is deliberately conservative — it
// only reads the shapes this codebase actually uses — and the last test guards
// the scanner itself, so a future refactor that renames AppIcon can't turn this
// suite into a no-op that passes by finding nothing.

import { describe, it, expect, vi } from 'vitest'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, extname, join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

// The registry reads one preference, purely to decide whether to warm the
// Phosphor chunk at startup. Stubbed so importing the registry doesn't drag in
// the api client — and through it the router, which touches `location` and so
// cannot be constructed in this runner's node environment.
vi.mock('@/composables/syncedPrefs', () => ({ getPref: () => null }))

import { lucideIcons } from './iconRegistry'

const SRC_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', '..')

// Registry keys are Lucide names: PascalCase, no separators. Everything the
// scanners pick up is filtered through this, which is what keeps the loose
// patterns below from dragging in labels, enum values or CSS class names.
const ICON_NAME = /^[A-Z][A-Za-z0-9]*$/

interface Usage {
  name: string
  where: string
}

function sourceFiles(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) {
      sourceFiles(full, out)
      continue
    }
    // The registry files themselves are the answer key, not a usage site.
    if (['.vue', '.ts'].includes(extname(full)) && !full.includes('iconRegistry')) out.push(full)
  }
  return out
}

/**
 * Collect every icon name the app can hand to AppIcon that is knowable
 * statically. Three shapes cover the codebase:
 *
 *   a. the `name` prop on an AppIcon tag, literal or a ternary of literals;
 *   b. an `icon`-ish object property with a literal value — `icon: 'Play'`,
 *      `titleIcon: 'Bell'`, `_iconName: 'Copy'` — the lookup tables that
 *      feed `:name="row.icon"`;
 *   c. anything declared under an `icon`-ish identifier — `const iconByKey`,
 *      `const metricIcons`, `function getProviderIcon()` — where the names
 *      are bare literals with no icon-ish key next to them.
 *
 * Names reached purely at runtime (a name assembled from a string, or read
 * from the backend) are out of reach here by construction.
 */
function collectUsages(): Usage[] {
  const found: Usage[] = []

  for (const file of sourceFiles(SRC_DIR)) {
    const text = readFileSync(file, 'utf8')
    const rel = relative(SRC_DIR, file).split(sep).join('/')
    const lines = text.split('\n')
    const at = (line: number) => `src/${rel}:${line}`
    const push = (name: string, line: number) => {
      if (ICON_NAME.test(name)) found.push({ name, where: at(line) })
    }

    // (a) AppIcon tags. The `s` flag matters — plenty of these tags wrap
    // across several lines with the name on one of the inner ones.
    for (const tag of text.matchAll(/<AppIcon\b[^>]*>/gs)) {
      const line = text.slice(0, tag.index).split('\n').length
      const literal = tag[0].match(/\sname="([^"]*)"/)
      if (literal) push(literal[1], line)
      const bound = tag[0].match(/:name="([^"]*)"/)
      if (bound) for (const quoted of bound[1].matchAll(/'([^']*)'/g)) push(quoted[1], line)
    }

    lines.forEach((lineText, i) => {
      // (b) `icon: 'Play'`, `titleIcon: "Bell"`, `_iconName: 'Copy'`, …
      for (const m of lineText.matchAll(/\b[\w$]*icon[\w$]*\s*:\s*['"]([^'"]+)['"]/gi)) push(m[1], i + 1)
    })

    // (c) Declarations whose identifier reads as icon-related: take every
    // quoted PascalCase literal in the declaration's body. Brace depth marks
    // the end so a map or a switch is captured whole.
    for (let i = 0; i < lines.length; i++) {
      const decl = lines[i].match(/\b(?:const|let|var|function)\s+([\w$]+)/)
      if (!decl || !/icon/i.test(decl[1])) continue
      let depth = 0
      let opened = false
      for (let j = i; j < lines.length; j++) {
        for (const quoted of lines[j].matchAll(/'([^']*)'/g)) push(quoted[1], j + 1)
        for (const ch of lines[j]) {
          if (ch === '{') {
            depth++
            opened = true
          } else if (ch === '}') depth--
        }
        if (opened && depth <= 0) break
        if (!opened && j > i) break // single-expression declaration
      }
    }
  }

  return found
}

// Timeout: this suite builds and cross-checks the full icon maps, so it is the
// only spec in the module that runs anywhere near a runner budget. It is not
// slow because of a regression -- measured twice, the file takes 7.1s/8.5s and
// its slowest single test 5.5s/7.1s, against Vitest's 5000ms default. What
// moved is the environment: the suite grew from 20 to 21 spec files and the
// build host runs 3-4x oversubscribed, so the default budget was measuring
// machine load rather than this code. 11s is set from the measured maximum
// (7148ms) plus ~54% headroom, and is scoped to this describe deliberately so
// the other 20 specs keep the tight default.
describe('icon registry', () => {
  it('maps the same key set in both packs', async () => {
    // Dynamic, because that is how the app loads it: the Phosphor map lives in
    // its own chunk (see ./iconRegistry) and there is no synchronous handle on
    // it any more.
    const phosphor = (await import('./iconRegistry.phosphor')).default

    const lucideKeys = Object.keys(lucideIcons).sort()
    const phosphorKeys = Object.keys(phosphor).sort()

    // Reported as two explicit diffs rather than a raw array comparison, so a
    // failure names the icons to add instead of dumping 157 strings.
    expect({
      missingFromPhosphor: lucideKeys.filter((k) => !(k in phosphor)),
      missingFromLucide: phosphorKeys.filter((k) => !(k in lucideIcons)),
    }).toEqual({ missingFromPhosphor: [], missingFromLucide: [] })
  })

  it('resolves every mapped key to a real component', async () => {
    const phosphor = (await import('./iconRegistry.phosphor')).default

    // Guards the other silent failure: upstream renames or drops an export,
    // the named import becomes undefined, and the key maps to nothing at all.
    const empty = [
      ...Object.keys(lucideIcons).filter((k) => !lucideIcons[k]).map((k) => `lucide:${k}`),
      ...Object.keys(phosphor).filter((k) => !phosphor[k]).map((k) => `phosphor:${k}`),
    ]
    expect(empty).toEqual([])
  })

  it('registers every icon name used in the app', () => {
    const unresolved = collectUsages()
      .filter((u) => !(u.name in lucideIcons))
      .map((u) => `${u.name} (${u.where})`)

    // Each of these renders an invisible icon in production.
    expect([...new Set(unresolved)].sort()).toEqual([])
  })

  it('scans a plausible number of usage sites', () => {
    // Self-check for the scanner above. Without it, renaming AppIcon or
    // reshaping the lookup tables would leave the previous test asserting
    // nothing about nothing — and passing.
    const names = new Set(collectUsages().map((u) => u.name))
    expect(names.size).toBeGreaterThan(100)
  })
}, 11_000)
