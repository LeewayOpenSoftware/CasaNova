// Turns the install wizard's form into a real compose document.
//
// WHY THIS EXISTS. installManual used to POST its structured body —
// {image, tag, title, webUI, …} — straight to /v2/app_management/compose.
// That endpoint reads its body through YAMLfromRequest, which treats a JSON
// body as a COMPOSE DOCUMENT, and no handler anywhere accepts the structured
// shape. So the server parsed an object with no `services` key, got an empty
// project, let Docker name it at random, and failed with "no container found
// for project fascinating_mikolaj". The download ran, the progress bar
// completed, and nothing was installed — the manual path (and the Docker Hub
// tab that feeds into it) could never have worked.
//
// Every app in CasaNova IS a compose project, so the honest fix is to build
// one. The shape below is modelled on a compose file installed and running in
// production rather than on the spec: same service key, same ports mapping,
// same x-casaos block the dashboard reads for title, icon and web UI.

export interface ComposeFormInput {
  image: string
  tag: string
  title: string
  iconUrl?: string
  webUI?: { protocol: 'http' | 'https'; host: string; port: number; path: string }
  network?: string
  ports?: Array<{ host: number; container: number; protocol: string }>
  volumes?: Array<{ hostPath: string; containerPath: string; readOnly: boolean }>
  env?: Array<{ key: string; value: string }>
}

/**
 * projectName — a compose project name derived from the app's title.
 *
 * Compose only accepts lowercase alphanumerics, dashes and underscores, and
 * rejects a leading non-alphanumeric. An empty result would hand naming back
 * to Docker, which is the random-name failure this module exists to prevent,
 * so it falls back to the image's last path segment and finally to a constant.
 */
export function projectName(title: string, image: string): string {
  const clean = (s: string) =>
    s
      .toLowerCase()
      .replace(/[^a-z0-9_-]+/g, '-')
      .replace(/^[^a-z0-9]+/, '')
      .replace(/-+$/g, '')
      .slice(0, 60)

  return clean(title) || clean(image.split('/').pop() || '') || 'casanova-app'
}

/** YAML-quote a scalar. Compose values are frequently URLs, paths and
 * key=value strings, all of which need quoting to survive as one token. */
function q(v: string | number): string {
  return JSON.stringify(String(v))
}

export function buildComposeYaml(form: ComposeFormInput): string {
  const name = projectName(form.title, form.image)
  const image = form.tag ? `${form.image}:${form.tag}` : form.image
  const L: string[] = []

  L.push(`name: ${name}`)
  L.push('services:')
  // "app" is the conventional main service key in this codebase; x-casaos.main
  // below points at it, and the dashboard resolves the app's status through
  // that pointer. A mismatch here is what produces "main service not been
  // specified" in the logs.
  L.push('    app:')
  L.push(`        image: ${q(image)}`)
  L.push(`        container_name: ${name}`)
  L.push('        restart: unless-stopped')

  if (form.env?.length) {
    L.push('        environment:')
    for (const e of form.env) {
      if (!e.key.trim()) continue
      L.push(`            ${e.key}: ${q(e.value)}`)
    }
  }

  // The web UI port is a published port like any other; listing it here as
  // well would make compose reject the file for a duplicate mapping, so it is
  // merged rather than appended.
  const ports = [...(form.ports ?? [])]
  if (form.webUI && form.webUI.port && !ports.some((p) => p.host === form.webUI!.port)) {
    ports.push({ host: form.webUI.port, container: form.webUI.port, protocol: 'tcp' })
  }
  if (ports.length) {
    L.push('        ports:')
    for (const p of ports) {
      L.push('            - mode: ingress')
      L.push(`              target: ${p.container}`)
      L.push(`              published: ${q(p.host)}`)
      L.push(`              protocol: ${p.protocol || 'tcp'}`)
    }
  }

  if (form.volumes?.length) {
    L.push('        volumes:')
    for (const v of form.volumes) {
      if (!v.hostPath.trim() || !v.containerPath.trim()) continue
      L.push('            - type: bind')
      L.push(`              source: ${q(v.hostPath)}`)
      L.push(`              target: ${q(v.containerPath)}`)
      if (v.readOnly) L.push('              read_only: true')
    }
  }

  if (form.iconUrl) {
    L.push('        labels:')
    L.push(`            icon: ${q(form.iconUrl)}`)
  }

  L.push('        networks:')
  L.push('            default: null')

  L.push('networks:')
  L.push('    default:')
  L.push(`        name: ${name}_default`)

  // The dashboard reads this block for the tile's title, icon and "open"
  // link. Without it the app installs but appears nameless and unopenable.
  L.push('x-casaos:')
  L.push(`    main: app`)
  L.push(`    author: ${q('self')}`)
  L.push(`    developer: ${q('self')}`)
  L.push(`    title:`)
  L.push(`        en_us: ${q(form.title)}`)
  if (form.iconUrl) L.push(`    icon: ${q(form.iconUrl)}`)
  if (form.webUI) {
    L.push(`    scheme: ${form.webUI.protocol}`)
    L.push(`    hostname: ${q(form.webUI.host || '')}`)
    L.push(`    port_map: ${q(form.webUI.port)}`)
    L.push(`    index: ${q(form.webUI.path || '/')}`)
  }

  return L.join('\n') + '\n'
}
