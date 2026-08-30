// The manual install path produced nothing. Its request body — {image, tag,
// title, webUI, …} — went to an endpoint that reads its body as a COMPOSE
// document, and no handler anywhere accepted that shape. The server parsed an
// object with no services, produced an empty project, let Docker name it at
// random, and failed with "no container found for project
// fascinating_mikolaj". The download ran and the progress card completed, so
// the failure was invisible until you went looking for the app.
//
// These pin the document that replaces it. The expected shape was checked
// against a compose file running in production, and the generated output was
// fed to `docker compose config`, which accepted it and resolved the project
// name to "firefox" rather than a random one.

import { describe, it, expect } from 'vitest'
import { buildComposeYaml, projectName } from './composeFromForm'

const base = {
  image: 'linuxserver/firefox',
  tag: 'latest',
  title: 'Firefox',
}

describe('projectName', () => {
  it('derives a compose-legal name from the title', () => {
    expect(projectName('Firefox', 'linuxserver/firefox')).toBe('firefox')
    expect(projectName('My Media Server!', 'x/y')).toBe('my-media-server')
  })

  it('never returns empty — an empty name is what let Docker pick a random one', () => {
    // A title of only punctuation, then no title at all: both must still
    // produce something, because the empty case is the original bug.
    expect(projectName('!!!', 'linuxserver/firefox')).toBe('firefox')
    expect(projectName('', '')).toBe('casanova-app')
  })

  it('strips a leading non-alphanumeric, which compose rejects', () => {
    expect(projectName('-leading', 'x/y')).toBe('leading')
  })
})

describe('buildComposeYaml', () => {
  it('produces a document with a name and a service — the two things that were missing', () => {
    const y = buildComposeYaml(base)
    expect(y).toContain('name: firefox')
    expect(y).toContain('services:')
    expect(y).toContain('app:')
    expect(y).toContain('image: "linuxserver/firefox:latest"')
  })

  it('points x-casaos.main at the service key it actually defines', () => {
    // A mismatch here is what logs "main service not been specified" and
    // leaves the dashboard unable to resolve the app's status.
    const y = buildComposeYaml(base)
    const service = y.match(/^ {4}(\w+):$/m)?.[1]
    expect(service).toBe('app')
    expect(y).toContain('main: app')
  })

  it('carries the title and icon the dashboard renders', () => {
    const y = buildComposeYaml({ ...base, iconUrl: 'https://example.com/i.png' })
    expect(y).toContain('en_us: "Firefox"')
    expect(y).toContain('icon: "https://example.com/i.png"')
  })

  it('does not publish the web UI port twice', () => {
    // Listing it both as an explicit port and again from webUI makes compose
    // reject the file for a duplicate mapping — the app would fail to start
    // for a reason the user cannot see.
    const y = buildComposeYaml({
      ...base,
      ports: [{ host: 3000, container: 3000, protocol: 'tcp' }],
      webUI: { protocol: 'http', host: '', port: 3000, path: '/' },
    })
    expect(y.match(/published: "3000"/g)?.length).toBe(1)
  })

  it('publishes the web UI port when it is not already listed', () => {
    const y = buildComposeYaml({
      ...base,
      webUI: { protocol: 'http', host: '', port: 8080, path: '/' },
    })
    expect(y).toContain('published: "8080"')
    expect(y).toContain('port_map: "8080"')
  })

  it('quotes values that would otherwise break the document', () => {
    // Env values with colons and spaces are ordinary (timezones, connection
    // strings) and split a YAML scalar if left bare.
    const y = buildComposeYaml({
      ...base,
      env: [{ key: 'TZ', value: 'America/Argentina/Buenos_Aires' }],
      volumes: [{ hostPath: '/DATA/App Data/x', containerPath: '/config', readOnly: true }],
    })
    expect(y).toContain('TZ: "America/Argentina/Buenos_Aires"')
    expect(y).toContain('source: "/DATA/App Data/x"')
    expect(y).toContain('read_only: true')
  })

  it('skips blank env keys and half-filled volume rows', () => {
    // The form starts rows empty; emitting them yields an invalid document.
    const y = buildComposeYaml({
      ...base,
      env: [{ key: '', value: 'orphan' }],
      volumes: [{ hostPath: '/only/host', containerPath: '', readOnly: false }],
    })
    expect(y).not.toContain('orphan')
    expect(y).not.toContain('/only/host')
  })
})
