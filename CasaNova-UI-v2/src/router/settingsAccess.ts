// Which settings pages a principal may open, decided in the UI.
//
// WHY THIS EXISTS
// ---------------
// Some settings pages have no content at all below PermSystemConfig: their
// one mount-time read is admin-only, so a non-admin who opens them gets an
// empty page plus an error toast. That is not a security hole — the backend
// refuses correctly — it is a dead end presented as a feature. This module
// decides, from the token the browser already holds, whether a page is worth
// showing at all, and the router guard plus the settings nav both ask it.
//
// THIS IS PRESENTATION, NOT AUTHORIZATION. Every one of these pages is
// enforced server-side and stays enforced. Hiding is cosmetic: hiding too
// much costs a user a page they could not have used anyway, showing too much
// costs them a toast in the face. So every unknown here resolves to "hide".
//
// WHERE THE ROLE COMES FROM  (no new endpoint — see effectiveRole below)
// ---------------------------------------------------------------------
// The access token's `role` claim, because that is the exact value the
// backend evaluates: CasaNova/route/v1.go passes `claims.Role` straight into
// SetRoleInContext with no database re-read, and requirePerm resolves it
// through rbac.Normalize. Reading the same bytes the server reads is what
// keeps this module's answer and the server's answer from diverging.
//
// The obvious alternative — auth.user.role, the row cached from the login
// response — is NOT equivalent and is the wrong source. PostUserLogin
// computes an `effectiveRole` for the token but returns the raw database row
// in `data.user`, and the two deliberately disagree for legacy accounts: a
// row with an empty role, or an unrecognised one like "administrator", mints
// an *admin* token while the returned row still reads "" or "administrator".
// A UI keyed on the row would fail closed on those and hide the settings
// section from the box's actual owner.
import { decodeJwtPayload } from './jwtPayload'

export type Role = 'admin' | 'user' | 'guest'

// Mirrors rbac.Normalize in CasaNova-Common/pkg/rbac/rbac.go: trim, lower,
// and fail closed to guest for anything empty or unrecognised.
export function normalizeRole(value: unknown): Role {
  const r = typeof value === 'string' ? value.trim().toLowerCase() : ''
  return r === 'admin' || r === 'user' || r === 'guest' ? r : 'guest'
}

// The role the server will attribute to this token, or null when the token
// is not a JWT at all.
//
// A token that parses is the whole answer, including when it carries no role
// claim: the claim is `omitempty`, so an absent role is what a pre-role-claim
// token looks like, and CasaNova normalizes that to guest. Falling back to a
// cached row there would show pages the server is about to refuse.
export function roleFromAccessToken(token: string): Role | null {
  const claims = decodeJwtPayload(token)
  return claims ? normalizeRole(claims.role) : null
}

// The role to present by, in precedence order:
//   1. the access token's claim, whenever the token is a JWT;
//   2. the cached user row, only when the token cannot be decoded at all —
//      which in practice is the dev scaffold's `dev-<ts>` placeholder token,
//      where there is no claim to read and no server to disagree with;
//   3. guest, when neither yields anything.
export function effectiveRole(accessToken: string, storedRole?: string | null): Role {
  const fromToken = roleFromAccessToken(accessToken)
  return fromToken ?? normalizeRole(storedRole)
}

// Settings pages whose mount-time read requires PermSystemConfig, each with
// the gate that puts it here. Audited against the whole settings section, not
// just the page that was reported:
//
//   scheduled — v1.GetCronJobs,     cron.go:252   requirePerm(PermSystemConfig)
//   audit     — v1.GetAuditEvents,  events.go:25  requireSystemPrincipal(PermSystemConfig)
//   logs      — v1.GetSystemLogs,   logs.go:63    requireSystemPrincipal(PermSystemConfig)
//   maintenance — every handler in maintenance.go opens with the same two
//                 lines: UserIDFromContext(ctx) == 0 -> 401, then
//                 requirePerm(PermSystemConfig). Listed even though the module
//                 is currently absent from the build entirely (route/v1.go
//                 registers its group behind a compile-time false, so the rail
//                 hides it on a 404 probe): absence and permission are separate
//                 facts, and the day the constant flips this is already right.
//
// Pages that only *write* under PermSystemConfig are deliberately absent:
// Backups, Alerts, Host, Network, Storage, General and Server all load their
// content at PermSystemRead and refuse individual actions, so they are usable
// pages with some disabled controls — not dead ends. Hiding them would remove
// what a non-admin legitimately came to read.
export const ADMIN_ONLY_SETTINGS_PATHS: readonly string[] = [
  '/app/settings/scheduled',
  '/app/settings/audit',
  '/app/settings/logs',
  '/app/settings/maintenance',
]

// Where a non-admin is sent when they deep-link into one of the above.
// General's reads are PermSystemRead, which every role including guest holds,
// so this target is reachable by anyone who is logged in at all.
export const SETTINGS_FALLBACK_PATH = '/app/settings/general'

function canonicalPath(path: string): string {
  const noQuery = path.split('?')[0].split('#')[0]
  return noQuery.length > 1 && noQuery.endsWith('/') ? noQuery.slice(0, -1) : noQuery
}

export function isAdminOnlySettingsPath(path: string): boolean {
  return ADMIN_ONLY_SETTINGS_PATHS.includes(canonicalPath(path))
}

// The single question the router guard and the nav both ask.
export function canAccessSettingsPath(path: string, role: Role): boolean {
  return role === 'admin' || !isAdminOnlySettingsPath(path)
}
