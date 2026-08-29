// Notes — a thin convention over the EXISTING file API, not a new backend.
//
// WHY THERE IS NO /v1/notes ENDPOINT.
//
// Notes are files. The file API already resolves paths through the storage
// authority, confines them with openat2, and gates every operation on an RBAC
// permission — the work of an entire hardening pass. A parallel /v1/notes would
// have to reimplement all of it, and the two would drift; "a second
// implementation that forgot half the rules" is the most repeated defect in
// this codebase's history.
//
// So this module owns exactly one thing the file API does not care about: the
// convention that notes live in one folder, as .md files. Everything else is a
// call into the already-hardened API.
//
// The consequence worth knowing: notes are ordinary files. They show up in the
// File Manager, they are covered by backups, and they open in Obsidian by
// pointing a vault at NOTES_ROOT. If CasaNova disappears tomorrow the notes are
// still there — which is the whole reason for choosing markdown on disk over a
// database.

import {
  listDir,
  mkdir,
  readFileContent,
  writeFileContent,
  createFile,
  deleteBatch,
} from './files'

/** Where notes live. A real directory the operator can open, sync or back up. */
export const NOTES_ROOT = '/DATA/Notes'

export const NOTE_EXT = '.md'

export interface NoteMeta {
  /** File name including the extension, e.g. "deploy-checklist.md". */
  name: string
  /** Display title — the name without the extension. */
  title: string
  /** Absolute path, for the file API. */
  path: string
  size: number
  /** Epoch milliseconds, parsed from the API's ISO 8601 string so the list can
   * sort numerically and the UI can render a relative time. */
  modified: number
}

/** The file API returns ISO 8601; an unparseable value sorts last rather than
 * throwing, because a note with a strange timestamp is still a note. */
function toEpochMs(iso: string | undefined): number {
  if (!iso) return 0
  const t = Date.parse(iso)
  return Number.isNaN(t) ? 0 : t
}

/**
 * isValidNoteName — a name that cannot escape NOTES_ROOT.
 *
 * The server already refuses traversal (the storage authority resolves and
 * confines every path), so this is not the security boundary — it is the UI
 * refusing to build a request it knows is wrong, so the user gets a clear
 * message instead of a 403.
 */
export function isValidNoteName(name: string): boolean {
  const t = name.trim()
  if (!t || t.length > 120) return false
  if (/[/\\]/.test(t)) return false // no separators
  if (t === '.' || t === '..' || t.startsWith('.')) return false
  if (/[\x00-\x1f\x7f]/.test(t)) return false // no control characters (escaped, not literal:
  // a raw NUL byte here makes git treat the file as binary)
  return true
}

/** Turns a user-typed title into a safe file name. */
export function noteFileName(title: string): string {
  const base = title.trim().replace(/[/\\]/g, '-').replace(/\s+/g, ' ')
  return base.toLowerCase().endsWith(NOTE_EXT) ? base : base + NOTE_EXT
}

/** Ensures the notes folder exists. Safe to call repeatedly. */
export async function ensureNotesRoot(): Promise<void> {
  try {
    await listDir(NOTES_ROOT)
  } catch {
    // Only reached when the folder is absent. A race between two tabs both
    // creating it is harmless — the second mkdir fails and the listing that
    // follows succeeds either way.
    await mkdir(NOTES_ROOT)
  }
}

/**
 * listNotes — newest first.
 *
 * That order is not cosmetic: the primary use is "I wrote something on the
 * other machine a minute ago", so the thing you want is always at the top.
 */
export async function listNotes(): Promise<NoteMeta[]> {
  await ensureNotesRoot()
  const entries = await listDir(NOTES_ROOT)
  return entries
    .filter((e) => !e.isDir && e.name.toLowerCase().endsWith(NOTE_EXT))
    .map((e) => ({
      name: e.name,
      title: e.name.slice(0, -NOTE_EXT.length),
      path: `${NOTES_ROOT}/${e.name}`,
      size: e.size ?? 0,
      modified: toEpochMs(e.modified),
    }))
    .sort((a, b) => b.modified - a.modified)
}

export async function readNote(path: string): Promise<string> {
  return readFileContent(path)
}

/**
 * writeNote — save an open note.
 *
 * The normal path is a plain PUT. The fallback exists because the whole point
 * of this feature is writing on one machine and reading on another: the file
 * can be deleted from the File Manager, or from the other machine, while it is
 * still open here. PUT is update-only, so that save would fail and the user
 * would lose what they just typed.
 *
 * The retry is deliberately narrow. If createFile reports the file was already
 * there, the "it vanished" theory was wrong and the original error is the real
 * one — rethrow it instead of papering over a permission or size failure.
 */
export async function writeNote(
  path: string,
  content: string,
): Promise<{ recreated: boolean }> {
  try {
    await writeFileContent(path, content)
    return { recreated: false }
  } catch (err) {
    const created = await createFile(path)
    if (!created) throw err
    await writeFileContent(path, content)
    // Reported, not swallowed. Recreating the file is the right call — the
    // user's text wins over a delete they may not know about — but the delete
    // came from somewhere, most likely the other machine, and undoing it
    // without a word is how two people quietly fight over one note.
    return { recreated: true }
  }
}

/** Thrown when a new note would take the name of an existing one. */
export class NoteExistsError extends Error {
  constructor(public readonly noteName: string) {
    super(`a note named ${noteName} already exists`)
    this.name = 'NoteExistsError'
  }
}

/**
 * createNote — bring a new note into existence.
 *
 * Two calls, because the file API splits the job: POST /v1/file creates an
 * empty file and PUT /v1/file fills it, and neither does the other's half.
 * The first version of this called only PUT and every attempt failed with a
 * 500, because the handler's first act is to refuse a path that is not
 * already there.
 *
 * An existing name is reported, never overwritten — silently clobbering a note
 * the user forgot about is the one failure this feature must not have.
 */
export async function createNote(title: string, content = ''): Promise<NoteMeta> {
  await ensureNotesRoot()
  const name = noteFileName(title)
  const path = `${NOTES_ROOT}/${name}`
  const created = await createFile(path)
  if (!created) throw new NoteExistsError(name)
  // Skipped for an empty note: the POST above already left an empty file, so
  // the PUT would be a round-trip that writes nothing. (Not a validation
  // concern — `binding:"required"` on the Go struct is a gin tag and echo's
  // Bind ignores it, and the OpenAPI validator only guards the v2 router.)
  if (content) await writeFileContent(path, content)
  return {
    name,
    title: name.slice(0, -NOTE_EXT.length),
    path,
    size: content.length,
    modified: Date.now(),
  }
}

export async function deleteNote(path: string): Promise<void> {
  await deleteBatch([path])
}
