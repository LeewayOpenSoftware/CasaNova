// Files API — wraps the /v1/file, /v1/folder, /v1/batch, and
// /v1/trash routes from CasaNova/route/v1.go. The UI's files store
// reads directory listings, mutates entries (create/rename/delete),
// uploads and downloads files, and browses the trash — every one of
// those maps to a call in this module.
//
// Naming: we drop the HTTP-verb prefix the backend handlers use
// internally (PostCreateFile → createFile). Types match the ObjResp
// struct in CasaNova/route/v1/file.go so swapping the fixture for
// this layer is a one-line change per store method.

import client from './client'

export interface ObjResp {
  name: string
  path: string
  isDir: boolean
  size: number
  modified: string // ISO 8601
  extensions?: Record<string, unknown>
}

// ----- Directory listing -----

/**
 * listDir — GET /v1/folder?path=/DATA
 * Backend route: v1FolderGroup.GET("", v1.DirPath)
 * Returns all direct children of the given path.
 */
// Backend response shape for folder listing — snake_case keys,
// nested under .data.content (not .data directly).
interface RawFolderEntry {
  name: string
  path: string
  is_dir: boolean
  size: number
  modified: string
  date?: string
  extensions?: Record<string, unknown>
}

export async function listDir(
  path: string,
  showHidden = false,
): Promise<ObjResp[]> {
  const res = await client.get<{
    success: number
    data: { content: RawFolderEntry[] }
  }>(
    '/v1/folder',
    { params: { path, show_hidden: showHidden ? 'true' : undefined } },
  )
  const raw = res.data.data?.content ?? []
  return raw.map((e) => ({
    name: e.name,
    path: e.path,
    isDir: e.is_dir,
    size: e.size,
    modified: e.modified || e.date || '',
    extensions: e.extensions,
  }))
}

/**
 * getSize — GET /v1/folder/size?path=/DATA/Media
 * Returns the recursive size of a directory in bytes.
 */
export async function getFolderSize(path: string): Promise<number> {
  const res = await client.get<{ success: number; data: { size: number } }>(
    '/v1/folder/size',
    { params: { path } },
  )
  return res.data.data.size
}

// ----- Mutations -----

/**
 * mkdir — POST /v1/folder { path }
 * Creates the directory (and any missing parents).
 */
export async function mkdir(path: string): Promise<ObjResp> {
  const res = await client.post<{ success: number; data: ObjResp }>(
    '/v1/folder',
    { path },
  )
  return res.data.data
}

/**
 * renamePath — PUT /v1/file/name (works for both files and folders).
 */
export async function renamePath(
  oldPath: string,
  newPath: string,
): Promise<void> {
  await client.put('/v1/file/name', {
    old_path: oldPath,
    new_path: newPath,
  })
}

/**
 * deleteBatch — DELETE /v1/batch
 * Moves entries to the trash. Pass `permanent: true` to hard-delete
 * without going through the recycle bin.
 *
 * Backend shape: the body is a raw JSON string array (not an object),
 * and the `permanent` flag is a query parameter, NOT part of the body.
 * See CasaNova/route/v1/file.go DeleteFile:
 *   ctx.Bind(&paths)          → []string from body
 *   ctx.QueryParam("permanent") → "true" from ?permanent=true
 */
export async function deleteBatch(
  paths: string[],
  permanent = false,
): Promise<void> {
  await client.delete('/v1/batch', {
    params: permanent ? { permanent: 'true' } : undefined,
    data: paths,
  })
}

/**
 * operateBatch — POST /v1/batch/task
 * Bulk copy / move operation. The backend binds to model.FileOperate:
 *   { type: "copy"|"move", item: [{ from: "..." }, ...], to: "..." }
 *
 * The frontend-facing interface uses friendlier names (operation,
 * sources, destination) which are mapped to the backend shape here.
 */
export interface BatchTaskRequest {
  operation: 'copy' | 'move'
  sources: string[]
  destination: string
  // style: "skip" tells the backend to leave any colliding destination
  // entry alone (counted as already-finished). Omitted = default
  // overwrite behavior. There is no "rename" strategy on the backend
  // yet; that's handled client-side by adding a " (N)" suffix.
  style?: 'skip'
}

export interface BatchTaskResponse {
  id: string
  status: 'pending' | 'running' | 'done' | 'error'
  progress: number
  message?: string
}

export async function operateBatch(
  body: BatchTaskRequest,
): Promise<BatchTaskResponse> {
  // Map to the backend's FileOperate struct shape:
  //   type  string     `json:"type"`
  //   item  []FileItem `json:"item"` where FileItem = { from: string }
  //   to    string     `json:"to"`
  const backendBody = {
    type: body.operation,
    item: body.sources.map((path) => ({ from: path })),
    to: body.destination,
    style: body.style ?? '',
  }
  const res = await client.post<{ success: number; data: BatchTaskResponse }>(
    '/v1/batch/task',
    backendBody,
  )
  return res.data.data
}

export async function getBatchStatus(): Promise<BatchTaskResponse[]> {
  const res = await client.get<{ success: number; data: BatchTaskResponse[] }>(
    '/v1/batch',
  )
  return res.data.data ?? []
}

export async function cancelBatchTask(id: string): Promise<void> {
  await client.delete(`/v1/batch/${encodeURIComponent(id)}/task`)
}

// ----- Read / write individual files -----

/**
 * readFileContent — GET /v1/file/content?path=...
 *
 * The handler puts the file text directly in `data` as a bare string
 * (route/v1/file.go: `Data: result`, where result is `string(info)`), so the
 * body is {"success":200,"data":"<file text>"} — there is no `.content`
 * wrapper. Reaching for one yielded `undefined` for every file ever read: the
 * text preview showed blank, and a caller that writes back what it read would
 * truncate the file.
 *
 * Both shapes are accepted because the wrapped form appears in older captures
 * of this API, and an unreadable body throws instead of resolving to ''. That
 * distinction is the point: an empty string is a legitimate answer for an
 * empty file, so it must never double as "the read failed" — otherwise a
 * failed read followed by a save silently destroys the file.
 */
export async function readFileContent(path: string): Promise<string> {
  const res = await client.get<{
    success: number
    data: string | { content?: string } | null
  }>('/v1/file/content', { params: { path } })

  const payload = res.data?.data
  if (typeof payload === 'string') return payload
  if (payload && typeof payload.content === 'string') return payload.content
  throw new Error(`unreadable response for ${path}`)
}

/**
 * writeFileContent — PUT /v1/file { path, content }
 *
 * UPDATE ONLY. The handler opens with `if !file.Exists(path) { 500 }`, so this
 * cannot bring a file into existence — call createFile first for a new path.
 */
export async function writeFileContent(
  path: string,
  content: string,
): Promise<void> {
  await client.put('/v1/file', { path, content })
}

/** Body code returned when the target path is already taken. */
export const FILE_OR_DIR_EXISTS = 20003

/**
 * backendMessage — the server's own explanation of a failed request.
 *
 * The file handlers answer failures with {"success":<code>,"message":"..."},
 * and that message is the only text that says WHAT went wrong. Axios rejects
 * with "Request failed with status code 500", which is the same sentence for a
 * permission refusal, a full disk, a missing file and the 100 MB cap — so an
 * error surfaced from the axios object alone tells the user, and anyone reading
 * their bug report, nothing at all.
 *
 * Returns undefined when there is no body message, so callers can fall back.
 */
export function backendMessage(err: unknown): string | undefined {
  const body = (err as { response?: { data?: { message?: unknown } } })?.response?.data
  const message = body?.message
  return typeof message === 'string' && message.trim() ? message : undefined
}

/**
 * createFile — POST /v1/file { path }
 *
 * Creates an empty file, which is the half writeFileContent refuses to do.
 *
 * Read the body, not the status. This handler replies `ctx.JSON(SUCCESS, ...)`
 * where SUCCESS is the constant 200, so it answers HTTP 200 unconditionally and
 * reports the real outcome in the body's `success` field. An axios caller that
 * only waits for a rejection would read every failure as a success.
 *
 * Returns true when the file was created and false when it already existed —
 * the caller decides whether an existing path is an error, because "create"
 * and "create if missing" want opposite answers there.
 */
export async function createFile(path: string): Promise<boolean> {
  let res
  try {
    res = await client.post<{ success: number; message?: string }>(
      '/v1/file',
      { path },
    )
  } catch (err) {
    // The handler answers a genuine create failure with a failing status, so
    // axios rejects before this code sees the body — but the body is where the
    // reason lives.
    const detail = backendMessage(err)
    if (detail) throw new Error(detail)
    throw err
  }
  const code = res.data?.success
  if (code === 200) return true
  if (code === FILE_OR_DIR_EXISTS) return false
  throw new Error(res.data?.message || `create failed (code ${code})`)
}

// ----- Upload -----

/**
 * Chunked upload. Large files go through the v2 flow.js-style protocol
 * (POST /v2/file/upload per chunk, sparse server-side assembly) so
 * transfers survive proxies with per-request body caps — a Cloudflare
 * tunnel rejects anything over 100MB — and flaky links (per-chunk retry
 * with resume via the GET preflight). Small files stay single-shot on
 * the v1 endpoint. Chunk size is tiered by file size so huge files
 * don't explode into thousands of requests, and halves automatically
 * when a connection keeps timing out.
 */
export interface UploadProgressEvent {
  loaded: number
  total: number
  file: string
}

export interface UploadFileResult {
  file: string // relative path within the destination
  ok: boolean
  error?: string
}

const MiB = 1024 * 1024
const SINGLE_SHOT_MAX = 32 * MiB
const MIN_CHUNK = 8 * MiB
const SINGLE_SHOT_TIMEOUT = 30 * 60_000
const CHUNK_TIMEOUT = 10 * 60_000

/** Tiered chunk size: bigger files get bigger chunks (80MB ceiling stays
 * comfortably under Cloudflare's 100MB request-body limit). */
function pickChunkSize(fileSize: number): number {
  if (fileSize <= 2048 * MiB) return 32 * MiB
  if (fileSize <= 8192 * MiB) return 64 * MiB
  return 80 * MiB
}

function relPathOf(f: File): string {
  const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath
  return rel && rel.length > 0 ? rel : f.name
}

function isTimeoutish(e: unknown): boolean {
  const err = e as { code?: string; message?: string }
  return err?.code === 'ECONNABORTED' || err?.code === 'ERR_NETWORK'
    || /timeout/i.test(err?.message ?? '')
}

function isSessionReset(e: unknown): boolean {
  return (e as { response?: { status?: number } })?.response?.status === 409
}

/** Abortable sleep — a cancel must not sit out the retry backoff. */
function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const abort = () => {
      clearTimeout(timer)
      reject(new DOMException('Aborted', 'AbortError'))
    }
    if (signal?.aborted) { abort(); return }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', abort)
      resolve()
    }, ms)
    signal?.addEventListener('abort', abort, { once: true })
  })
}

/** djb2 — compact fingerprint for the session identifier. */
function hashStr(s: string): string {
  let h = 5381
  for (let i = 0; i < s.length; i++) h = ((h * 33) ^ s.charCodeAt(i)) >>> 0
  return h.toString(16)
}

/** GET preflight: 200 = server already has this chunk (resume), else absent.
 * The codegen wrapper requires the full field set as query params (it 400s
 * on missing ones before the handler even runs). */
async function hasChunk(
  destDir: string, f: File, relPath: string, identifier: string,
  chunkNumber: number, totalChunks: number, signal?: AbortSignal,
): Promise<boolean> {
  try {
    // NB: the v2 API mounts under /v2/casaos (OpenAPI servers.url) — the
    // bare "/v2/file/…" path is a gateway 404.
    const res = await client.get('/v2/casaos/file/upload', {
      params: {
        path: destDir,
        relativePath: relPath,
        filename: f.name,
        identifier,
        chunkNumber,
        totalChunks,
      },
      signal,
      validateStatus: () => true,
    })
    return res.status === 200
  } catch (e) {
    if (signal?.aborted) throw e
    return false
  }
}

async function postChunk(
  destDir: string, f: File, relPath: string, identifier: string,
  chunkNumber: number, chunkSize: number, totalChunks: number, blob: Blob,
  signal: AbortSignal | undefined,
  onChunkProgress: (loaded: number) => void,
): Promise<void> {
  const form = new FormData()
  form.append('file', blob, f.name)
  form.append('path', destDir)
  form.append('filename', f.name)
  form.append('relativePath', relPath)
  form.append('chunkNumber', String(chunkNumber))
  form.append('chunkSize', String(chunkSize))
  form.append('currentChunkSize', String(blob.size))
  form.append('totalChunks', String(totalChunks))
  form.append('totalSize', String(f.size))
  form.append('identifier', identifier)
  await client.post('/v2/casaos/file/upload', form, {
    timeout: CHUNK_TIMEOUT,
    signal,
    onUploadProgress: (e) => onChunkProgress(Math.min(e.loaded, blob.size)),
  })
}

async function uploadChunked(
  destDir: string, f: File, relPath: string, chunkSize: number,
  signal: AbortSignal | undefined,
  report: (loaded: number) => void,
): Promise<void> {
  const totalChunks = Math.ceil(f.size / chunkSize)
  // The identifier keys the server-side resume session. It fingerprints the
  // full destination (dir + relPath) so the same file uploaded to two folders
  // never shares a session, lastModified so an edited same-size file never
  // resumes into stale chunks, and the chunk size so a tier-downgrade restart
  // never collides with the old grid.
  const identifier = `${f.size}-${f.lastModified ?? 0}-${hashStr(destDir + '/' + relPath)}-${chunkSize}`
  let uploadedBytes = 0
  for (let n = 1; n <= totalChunks; n++) {
    const start = (n - 1) * chunkSize
    const end = Math.min(start + chunkSize, f.size)
    const blob = f.slice(start, end)
    if (await hasChunk(destDir, f, relPath, identifier, n, totalChunks, signal)) {
      uploadedBytes += blob.size
      report(uploadedBytes)
      continue
    }
    let attempt = 0
    for (;;) {
      try {
        const base = uploadedBytes
        await postChunk(destDir, f, relPath, identifier, n, chunkSize, totalChunks, blob,
          signal, (loaded) => report(base + loaded))
        break
      } catch (e) {
        if (signal?.aborted || isSessionReset(e)) throw e
        attempt++
        if (attempt >= 3) throw e
        await sleep(1000 * 3 ** (attempt - 1), signal)
      }
    }
    uploadedBytes += blob.size
    report(uploadedBytes)
  }
}

async function uploadSingleShot(
  destDir: string, f: File, relPath: string,
  signal: AbortSignal | undefined,
  report: (loaded: number) => void,
): Promise<void> {
  const form = new FormData()
  form.append('file', f, f.name)
  form.append('path', destDir)
  form.append('filename', f.name)
  form.append('relativePath', relPath)
  form.append('chunkNumber', '0')
  form.append('totalChunks', '0')
  await client.post('/v1/file/upload', form, {
    timeout: SINGLE_SHOT_TIMEOUT,
    signal,
    onUploadProgress: (e) => report(Math.min(e.loaded, f.size)),
  })
}

export async function uploadFiles(
  destDir: string,
  files: File[],
  onProgress?: (event: UploadProgressEvent) => void,
  signal?: AbortSignal,
): Promise<UploadFileResult[]> {
  const results: UploadFileResult[] = []
  for (const f of files) {
    const relPath = relPathOf(f)
    const report = (loaded: number) => {
      onProgress?.({ loaded: Math.min(loaded, f.size), total: f.size, file: relPath })
    }
    try {
      if (f.size <= SINGLE_SHOT_MAX) {
        // Retry small files too; if a slow link keeps timing out a 8-32MB
        // body (the gateway reads each request under a 120s deadline), fall
        // back to chunking it with the minimum chunk size.
        let attempt = 0
        for (;;) {
          try {
            await uploadSingleShot(destDir, f, relPath, signal, report)
            break
          } catch (e) {
            if (signal?.aborted) throw e
            attempt++
            if (attempt >= 3) {
              if (isTimeoutish(e) && f.size > MIN_CHUNK) {
                await uploadChunked(destDir, f, relPath, MIN_CHUNK, signal, report)
                break
              }
              throw e
            }
            await sleep(1000 * 3 ** (attempt - 1), signal)
          }
        }
      } else {
        // Tier-downgrade loop: a link that keeps timing out gets its chunk
        // size halved (fresh resume session) down to MIN_CHUNK. A 409 from
        // the server means the resume state was invalid and got dropped —
        // restart the file from chunk 1 (fresh identifier grid, once).
        let chunkSize = pickChunkSize(f.size)
        let resetRetried = false
        for (;;) {
          try {
            await uploadChunked(destDir, f, relPath, chunkSize, signal, report)
            break
          } catch (e) {
            if (signal?.aborted) throw e
            if (isSessionReset(e) && !resetRetried) {
              resetRetried = true
              continue
            }
            if (isTimeoutish(e) && chunkSize > MIN_CHUNK) {
              chunkSize = Math.max(MIN_CHUNK, chunkSize / 2)
              continue
            }
            throw e
          }
        }
      }
      report(f.size)
      results.push({ file: relPath, ok: true })
    } catch (e) {
      if (signal?.aborted) {
        results.push({ file: relPath, ok: false, error: 'canceled' })
        break // stop the whole batch on cancel
      }
      const err = e as { message?: string }
      results.push({ file: relPath, ok: false, error: err?.message ?? String(e) })
      // keep going with the remaining files — one failure must not sink the batch
    }
  }
  return results
}

// ----- Download -----

/**
 * downloadFileUrl — returns the direct URL for a single file, WITHOUT
 * a token in the query string. This is suitable for `<img src>`,
 * `<video src>`, and `<a href>` attributes where the browser's
 * credentialed same-origin cookie/session can serve the request,
 * or for contexts where a different auth layer gates the request.
 *
 * Wave 34-Y: previously this embedded `token=<JWT>` in the query
 * string so that `downloadPath()` could `a.click()` through an
 * unauthenticated <a> tag. That token ended up in:
 *   - nginx / gateway access logs (logged by default)
 *   - the browser history/URL bar the moment the user middle-clicked
 *   - Referer headers on any linked-to image/script/iframe the
 *     destination of the download itself contained
 *   - every browser cache index
 * A 7-day refresh-family access token leaking there is effectively a
 * full credential dump. Callers that want to DOWNLOAD a file should
 * use `fetchFileBlob(path)` instead — it streams via axios with the
 * bearer Authorization header, returns a blob, and the anchor href
 * is a `blob:` URL that is tied to the current document and never
 * serialized anywhere.
 */
export function downloadFileUrl(path: string): string {
  return `/v1/file?path=${encodeURIComponent(path)}`
}

/**
 * fetchFileBlob — stream a file via the authenticated axios client
 * and hand back a blob URL the caller can attach to an anchor.
 * The returned URL is synthetic (`blob:<origin>/<uuid>`); callers
 * are responsible for calling URL.revokeObjectURL(url) once the
 * download has completed (or on component teardown) to free the
 * underlying bytes.
 */
export async function fetchFileBlob(
  path: string,
  onProgress?: (loaded: number, total: number) => void,
  signal?: AbortSignal,
): Promise<string> {
  const res = await client.get('/v1/file', {
    params: { path },
    responseType: 'blob',
    // A download takes as long as it takes — the client-wide 10s default
    // silently killed anything big (console.error + return, no user
    // feedback: the "press download 5 times" bug). Progress + cancel are
    // the guardrails now, not a timer.
    timeout: 0,
    signal,
    onDownloadProgress: (e) => onProgress?.(e.loaded, e.total ?? 0),
  })
  return URL.createObjectURL(res.data as Blob)
}

/**
 * fetchImageThumbnail — GET /v1/image?path=...&type=thumbnail
 * Returns a blob URL pointing at a 100×100 PNG thumbnail generated
 * server-side. Used by the file grid/list to preview image files
 * inline. The backend route is defined in CasaNova/route/v1/file.go
 * GetFileImage — it streams the generated PNG body directly with no
 * JSON envelope, so we request responseType: 'blob' on the axios
 * call and never try to parse the body as JSON.
 *
 * Throws on non-2xx; callers treat the rejection as "fall back to
 * the generic FileIcon".
 */
export async function fetchImageThumbnail(path: string): Promise<string> {
  const res = await client.get('/v1/image', {
    params: { path, type: 'thumbnail' },
    responseType: 'blob',
  })
  return URL.createObjectURL(res.data as Blob)
}

/**
 * fetchImageOriginal — GET /v1/image?path=... (no type param = full)
 * Returns a blob URL for the original image file. Used by the
 * FilePreviewModal's image branch so the `<img>` src is a
 * `blob:<origin>/<uuid>` URL instead of a public HTTP URL with the
 * bearer token leaking in the query string.
 */
export async function fetchImageOriginal(path: string): Promise<string> {
  const res = await client.get('/v1/image', {
    params: { path },
    responseType: 'blob',
  })
  return URL.createObjectURL(res.data as Blob)
}

/**
 * fileSearch — GET /v1/file/search?root=…&q=…&limit=…&hidden=…
 *
 * Recursive name search. Backend caps at 100 000 files visited and a
 * 30 s walk deadline; `truncated: true` in the response means the user
 * should refine the query rather than scrolling for more hits.
 */
export interface FileSearchHit {
  name: string
  path: string
  is_dir: boolean
  size: number
  modified: string
}
export interface FileSearchResponse {
  hits: FileSearchHit[]
  visited: number
  truncated: boolean
  query: string
  root: string
}

// ----- Archive operations -----

/**
 * compressFiles — POST /v1/file/compress. Synchronous on the backend
 * (15 min cap); the UI shows a spinner. Returns the final archive path
 * (the backend appends .zip/.tar.gz when missing).
 */
export async function compressFiles(
  paths: string[],
  destination: string,
  format: 'zip' | 'targz' = 'zip',
): Promise<string> {
  const res = await client.post<{ success: number; data: { archive: string } }>(
    '/v1/file/compress',
    { paths, destination, format },
  )
  return res.data.data.archive
}

/**
 * extractArchive — POST /v1/file/extract. Empty destination = sibling
 * folder named after the archive (desktop-manager convention).
 */
export async function extractArchive(
  path: string,
  destination = '',
): Promise<string> {
  const res = await client.post<{ success: number; data: { destination: string } }>(
    '/v1/file/extract',
    { path, destination },
  )
  return res.data.data.destination
}

/**
 * downloadAsArchive — GET /v1/file/download?format=zip&files=a,b,c
 * Multi-select download. Returns a blob URL the caller anchors-clicks.
 * Folders are always archived; a single plain file short-circuits to a
 * direct download on the backend.
 */
export async function downloadAsArchive(
  paths: string[],
  format: 'zip' | 'targz' = 'zip',
  onProgress?: (loaded: number, total: number) => void,
  signal?: AbortSignal,
): Promise<{ url: string; suggestedName: string }> {
  const res = await client.get('/v1/file/download', {
    params: { format: format === 'targz' ? 'targz' : 'zip', files: paths.join(',') },
    responseType: 'blob',
    timeout: 0, // archives stream with no content-length — no timer, see fetchFileBlob
    signal,
    onDownloadProgress: (e) => onProgress?.(e.loaded, e.total ?? 0),
  })
  const url = URL.createObjectURL(res.data as Blob)
  const base = paths.length === 1
    ? paths[0].substring(paths[0].lastIndexOf('/') + 1)
    : 'casanova-files'
  return { url, suggestedName: `${base}.${format === 'targz' ? 'tar.gz' : 'zip'}` }
}

// ----- Public share links -----

export interface ShareLinkView {
  id: string
  path: string
  isDir: boolean
  protected: boolean
  createdAt: number
  expiresAt?: number
  downloads: number
  url: string
}

export async function createShareLink(
  path: string,
  opts?: { password?: string; expiresHours?: number },
): Promise<ShareLinkView> {
  const res = await client.post<{ success: number; data: ShareLinkView }>(
    '/v1/file/share-links',
    { path, password: opts?.password ?? '', expiresHours: opts?.expiresHours ?? 0 },
  )
  return res.data.data
}

export async function listShareLinks(): Promise<ShareLinkView[]> {
  const res = await client.get<{ success: number; data: ShareLinkView[] }>(
    '/v1/file/share-links',
  )
  return res.data.data ?? []
}

export async function revokeShareLink(id: string): Promise<void> {
  await client.delete(`/v1/file/share-links/${id}`)
}

export async function fileSearch(
  root: string,
  q: string,
  opts?: { limit?: number; hidden?: boolean },
): Promise<FileSearchResponse> {
  const res = await client.get<{ success: number; data: FileSearchResponse }>(
    '/v1/file/search',
    {
      params: {
        root,
        q,
        limit: opts?.limit ?? 200,
        hidden: opts?.hidden ? 'true' : 'false',
      },
    },
  )
  return res.data.data
}

// ----- Global search (casanova-search, /v2/search) -----
// ZimaOS-compatible file search across ALL storage roots (not just the current
// folder like fileSearch). Backed by the bounded live walk in casanova-search.
export interface SearchHitSource {
  name: string
  dir: string
  is_dir: boolean
  is_ai: boolean
  mtime: number
  size: number
  type: string
  hl_name: string
  hl_content: string
}
export interface SearchHit {
  _id: number
  _score: number
  _source: SearchHitSource
}
export interface GlobalSearchResponse {
  message: string
  took: number
  timed_out: boolean
  data: { cursor: string; total: number; hits: SearchHit[] }
}

export async function globalSearch(
  keyword: string,
  opts?: { dir?: string; count?: number },
): Promise<GlobalSearchResponse> {
  const res = await client.post<GlobalSearchResponse>('/v2/search/file', {
    keyword,
    count: opts?.count ?? 60,
    dir: opts?.dir ?? '',
  })
  return res.data
}

// ----- Trash (recycle bin) -----

// Mirrors trash.Entry in CasaNova/pkg/trash. The previous shape here
// (name / deleted_at / auto_purge_at, with data typed as a bare array) never
// matched what the service sends — it went unnoticed because "/v1/trash" was
// missing from the gateway's route table, so these calls never reached the
// service at all.
export interface TrashEntry {
  id: string
  trashed_name: string
  original_path: string
  deletion_date: string
  size: number
  is_dir: boolean
}

export interface TrashListing {
  count: number
  entries: TrashEntry[]
  total_bytes: number
}

export async function listTrash(): Promise<TrashEntry[]> {
  const res = await client.get<{ success: number; data: TrashListing }>('/v1/trash')
  return res.data.data?.entries ?? []
}

/** Same call as listTrash but keeps the totals, for a "N items, X MB" header. */
export async function listTrashWithTotals(): Promise<TrashListing> {
  const res = await client.get<{ success: number; data: TrashListing }>('/v1/trash')
  return res.data.data ?? { count: 0, entries: [], total_bytes: 0 }
}

export async function restoreTrash(ids: string[]): Promise<void> {
  await client.post('/v1/trash/restore', { ids })
}

export async function emptyTrash(): Promise<void> {
  await client.delete('/v1/trash')
}

export async function deleteTrashItem(id: string): Promise<void> {
  await client.delete('/v1/trash', { data: { ids: [id] } })
}

// ----- Samba shares (sidebar "Compartido" section) -----

export interface SambaShare {
  id: string
  name: string
  path: string
  added_at: string
  guest: boolean
}

export async function listShares(): Promise<SambaShare[]> {
  const res = await client.get<{ success: number; data: SambaShare[] }>(
    '/v1/samba/shares',
  )
  return res.data.data ?? []
}

export async function createShare(body: {
  name: string
  path: string
  guest?: boolean
}): Promise<SambaShare> {
  const res = await client.post<{ success: number; data: SambaShare }>(
    '/v1/samba/shares',
    body,
  )
  return res.data.data
}

export async function deleteShare(id: string): Promise<void> {
  await client.delete(`/v1/samba/shares/${encodeURIComponent(id)}`)
}

/**
 * createSambaConnection — attaches a remote SMB share as a mounted
 * entry in the sidebar's Storage section. Matches the CasaOS
 * "Conectar recurso de red" flow.
 */
export async function createSambaConnection(body: {
  host: string
  path: string
  username?: string
  password?: string
  guest?: boolean
}): Promise<{ id: string; mountPoint: string }> {
  const res = await client.post<{ success: number; data: { id: string; mountPoint: string } }>(
    '/v1/samba/connections',
    body,
  )
  return res.data.data
}

export async function listSambaConnections(): Promise<
  Array<{ id: string; host: string; path: string; mountPoint: string }>
> {
  const res = await client.get<{
    success: number
    data: Array<{ id: string; host: string; path: string; mountPoint: string }>
  }>('/v1/samba/connections')
  return res.data.data ?? []
}
