// Every t('ns.key') the source calls must exist in every locale.
//
// This exists because an entire namespace was destroyed in one line. A script
// adding new keys wrote `d['apps'] = {...}` — an assignment, not a merge — and
// replaced 110 translated strings with the 14 it was adding. The app kept
// building, every test kept passing, and the app-detail page silently rendered
// raw keys ("apps.uninstall", "apps.container_id") until a human looked at it.
//
// Nothing in the type system or the build can catch that: i18n keys are
// strings resolved at runtime. This test is the only thing standing between a
// careless locale edit and a page of identifiers, so it fails loudly and names
// exactly what is missing.

import { describe, it, expect } from 'vitest'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, resolve } from 'node:path'

const SRC = resolve(__dirname, '..')
const LOCALES = resolve(__dirname, 'locales')

function sourceFiles(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry)
    if (statSync(p).isDirectory()) {
      if (entry !== 'node_modules') sourceFiles(p, out)
    } else if (p.endsWith('.vue') || p.endsWith('.ts')) {
      if (!p.endsWith('.spec.ts')) out.push(p)
    }
  }
  return out
}

/** Every key the source asks for, as "namespace.key". */
function keysUsed(): Set<string> {
  const used = new Set<string>()
  // t('ns.key') and $t('ns.key'), single or double quoted. Deliberately does
  // NOT try to resolve computed keys (`t(someVar)`) — those cannot be checked
  // statically and are rare here.
  const re = /\$?t\(\s*['"]([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)['"]/g
  for (const f of sourceFiles(SRC)) {
    const text = readFileSync(f, 'utf8')
    for (const m of text.matchAll(re)) {
      // A literal ending in "_" is the PREFIX half of a computed key —
      // t('settings.diagnosis.cat_' + group.category). The full key only
      // exists at runtime, so demanding it as written would fail forever on
      // four honest call sites. Their translations are still covered: every
      // concrete cat_*/sev_*/power_*_ key that IS defined gets checked by the
      // cross-locale test below.
      if (!m[1].endsWith('_')) used.add(m[1])
    }
  }
  return used
}

function flatten(obj: unknown, prefix = '', out = new Set<string>()): Set<string> {
  if (obj && typeof obj === 'object') {
    for (const [k, v] of Object.entries(obj as Record<string, unknown>)) {
      const path = prefix ? `${prefix}.${k}` : k
      if (v && typeof v === 'object') flatten(v, path, out)
      else out.add(path)
    }
  }
  return out
}

const localeFiles = readdirSync(LOCALES).filter((f) => f.endsWith('.json'))

describe('i18n completeness', () => {
  it('finds the locale files and some usage', () => {
    // Guards the guard: a regex that matched nothing, or a locale directory
    // that moved, would make every assertion below pass vacuously.
    expect(localeFiles.length).toBeGreaterThanOrEqual(3)
    expect(keysUsed().size).toBeGreaterThan(200)
  })

  for (const file of localeFiles) {
    it(`${file} defines every key the source uses`, () => {
      const defined = flatten(JSON.parse(readFileSync(join(LOCALES, file), 'utf8')))
      const missing = [...keysUsed()].filter((k) => !defined.has(k)).sort()
      expect(
        missing,
        `${file} is missing ${missing.length} key(s) the source calls; the UI renders the raw ` +
          `identifier for each. Most likely a locale edit ASSIGNED a namespace instead of merging into it.`,
      ).toEqual([])
    })
  }

  it('every locale defines the same key set', () => {
    // A key present in one language and absent in another falls back to the
    // identifier for the users of that language only — the kind of gap that
    // survives review because whoever reviews it reads one language.
    const sets = localeFiles.map((f) => ({
      file: f,
      keys: flatten(JSON.parse(readFileSync(join(LOCALES, f), 'utf8'))),
    }))
    const reference = sets[0]
    for (const other of sets.slice(1)) {
      const missing = [...reference.keys].filter((k) => !other.keys.has(k)).sort()
      const extra = [...other.keys].filter((k) => !reference.keys.has(k)).sort()
      expect(missing, `${other.file} lacks keys that ${reference.file} has`).toEqual([])
      expect(extra, `${other.file} has keys ${reference.file} lacks`).toEqual([])
    }
  })
})
