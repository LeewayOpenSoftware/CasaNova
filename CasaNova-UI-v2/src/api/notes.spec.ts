// Regression tests for the create path of the Notes API.
//
// Context: the first version of createNote() called only writeFileContent()
// (PUT /v1/file) on a path that did not exist yet. Every attempt failed with
// HTTP 500, and the body said "File already exists" for a file that did not —
// so the error text pointed away from the cause. The file API splits the job:
// POST creates an empty file and PUT fills it, and neither does the other's
// half.
//
// The second trap is quieter. POST /v1/file replies ctx.JSON(SUCCESS, ...)
// where SUCCESS is the constant 200, so it answers HTTP 200 unconditionally
// and reports the real outcome in the body's `success` field. A helper that
// waits for an axios rejection reads every failure as a success — including
// a name collision, which would then be silently overwritten.
//
// These pin: the verb sequence, the collision, and the resilient save.

import { describe, it, expect, beforeEach, vi } from 'vitest'

const calls: Array<{ verb: string; url: string; body: unknown }> = []

// Fresh per test so each one declares its own server behaviour.
let postImpl: (url: string, body: unknown) => Promise<{ data: unknown }>
let putImpl: (url: string, body: unknown) => Promise<{ data: unknown }>
let getImpl: (url: string, cfg?: unknown) => Promise<{ data: unknown }>

vi.mock('./client', () => ({
  default: {
    post: (url: string, body: unknown) => {
      calls.push({ verb: 'POST', url, body })
      return postImpl(url, body)
    },
    put: (url: string, body: unknown) => {
      calls.push({ verb: 'PUT', url, body })
      return putImpl(url, body)
    },
    get: (url: string, cfg?: unknown) => {
      calls.push({ verb: 'GET', url, body: cfg })
      return getImpl(url, cfg)
    },
    delete: (url: string) => {
      calls.push({ verb: 'DELETE', url, body: null })
      return Promise.resolve({ data: { success: 200 } })
    },
  },
}))

import { createNote, writeNote, readNote, NoteExistsError, NOTES_ROOT } from './notes'

const ok = { data: { success: 200, message: 'ok', data: null } }
/** What the box actually returns for a name that is taken: HTTP 200, code 20003. */
const taken = { data: { success: 20003, message: 'File or folder already exists', data: null } }
/** The notes folder already exists, so ensureNotesRoot() stops at the listing. */
const emptyDir = { data: { success: 200, data: { content: [] } } }

beforeEach(() => {
  calls.length = 0
  postImpl = async () => ok
  putImpl = async () => ok
  getImpl = async () => emptyDir
})

describe('createNote', () => {
  it('creates the file before writing to it', async () => {
    await createNote('deploy-checklist', '# hola')

    const verbs = calls.filter((c) => c.url === '/v1/file').map((c) => c.verb)
    expect(verbs).toEqual(['POST', 'PUT'])

    // The regression this file exists for: a lone PUT is the broken version.
    expect(verbs[0]).toBe('POST')
  })

  it('skips the write for an empty note — POST already left it empty', async () => {
    await createNote('vacia', '')
    expect(calls.filter((c) => c.url === '/v1/file').map((c) => c.verb)).toEqual(['POST'])
  })

  it('puts the note under the notes folder with a .md extension', async () => {
    await createNote('recordatorio', 'x')
    const post = calls.find((c) => c.verb === 'POST' && c.url === '/v1/file')
    expect(post?.body).toEqual({ path: `${NOTES_ROOT}/recordatorio.md` })
  })

  it('reports a taken name instead of overwriting it', async () => {
    postImpl = async () => taken

    await expect(createNote('ya-existe', 'contenido nuevo')).rejects.toBeInstanceOf(NoteExistsError)

    // The point of the guard: nothing was written over the existing note.
    expect(calls.some((c) => c.verb === 'PUT')).toBe(false)
  })

  it('surfaces the server\'s explanation, not the axios status string', async () => {
    // The create handler now answers a real failure with a failing status, so
    // axios rejects before the body is read. "Request failed with status code
    // 500" reads identically for a full disk, a refused path and a missing
    // parent — the body message is the only text that says which.
    postImpl = async () => {
      throw Object.assign(new Error('Request failed with status code 500'), {
        response: { data: { success: 500, message: 'mkdir /DATA/Notes: read-only file system' } },
      })
    }
    await expect(createNote('sin-espacio', '')).rejects.toThrow('read-only file system')
  })

  it('does not mistake a taken name for success just because HTTP was 200', async () => {
    postImpl = async () => taken
    // If this ever resolves, the helper is reading the status instead of the body.
    await expect(createNote('colision', '')).rejects.toBeInstanceOf(NoteExistsError)
  })
})

describe('readNote', () => {
  // The bug these pin was worth more than the create failure that started the
  // investigation. GET /v1/file/content puts the file text directly in `data`
  // as a bare string — verified against the running box:
  //   {"success":200,"message":"ok","data":"CONTENIDO REAL DE LA NOTA"}
  // The client reached for `data.content`, which is undefined. Notes opened
  // blank with no error, and the first save wrote the empty buffer over the
  // real file.
  it('reads text from the bare-string body the server actually sends', async () => {
    getImpl = async () => ({ data: { success: 200, message: 'ok', data: '# real content' } })
    await expect(readNote(`${NOTES_ROOT}/n.md`)).resolves.toBe('# real content')
  })

  it('still reads the wrapped shape', async () => {
    getImpl = async () => ({ data: { success: 200, data: { content: '# wrapped' } } })
    await expect(readNote(`${NOTES_ROOT}/n.md`)).resolves.toBe('# wrapped')
  })

  it('returns an empty string for a genuinely empty file', async () => {
    // Must NOT throw: '' is a legitimate answer, and conflating it with a
    // failure would block opening every empty note.
    getImpl = async () => ({ data: { success: 200, data: '' } })
    await expect(readNote(`${NOTES_ROOT}/n.md`)).resolves.toBe('')
  })

  it('throws rather than resolving to empty when the body makes no sense', async () => {
    // The inverse of the case above, and the one that protects the file: a
    // read that failed must never look like an empty note, because the editor
    // would then save that emptiness over the content.
    getImpl = async () => ({ data: { success: 200, data: null } })
    await expect(readNote(`${NOTES_ROOT}/n.md`)).rejects.toThrow()
  })
})

describe('writeNote', () => {
  it('saves with a single PUT when the note is there', async () => {
    const r = await writeNote(`${NOTES_ROOT}/n.md`, 'texto')
    expect(calls.map((c) => c.verb)).toEqual(['PUT'])
    expect(r.recreated).toBe(false)
  })

  it('recreates a note that was deleted while it was open', async () => {
    let putsSeen = 0
    putImpl = async () => {
      putsSeen += 1
      if (putsSeen === 1) throw new Error('HTTP 500')
      return ok
    }
    postImpl = async () => ok // the file really was gone, so the create succeeds

    const r = await writeNote(`${NOTES_ROOT}/n.md`, 'lo que el usuario acaba de escribir')
    expect(calls.map((c) => c.verb)).toEqual(['PUT', 'POST', 'PUT'])
    // Reported, so the view can tell the user their save undid someone's delete.
    expect(r.recreated).toBe(true)
  })

  it('rethrows the original error when the note was there all along', async () => {
    // A save can fail for reasons that have nothing to do with the file being
    // missing. Retrying then would bury the real cause.
    putImpl = async () => {
      throw new Error('permiso denegado')
    }
    postImpl = async () => taken // still present → the "it vanished" theory is wrong

    await expect(writeNote(`${NOTES_ROOT}/n.md`, 'x')).rejects.toThrow('permiso denegado')

    // Asserting on the message alone is not enough: a version that retries
    // blindly also ends up rejecting with this same error, just one wasted
    // write later. The sequence is what separates them — the second PUT must
    // never happen once POST reports the file was there all along.
    expect(calls.map((c) => c.verb)).toEqual(['PUT', 'POST'])
  })
})
