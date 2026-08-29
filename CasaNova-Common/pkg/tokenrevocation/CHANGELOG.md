# tokenrevocation — operator-visible changes

Only changes an operator can notice on a running box belong here. Code-level
history is in git.

## Unreleased

- **The first start of this build on an existing box signs everyone out, once.**
  Adopting a revocation list written by a build that could lose revocations
  silently sets a global cutoff at that instant: every token issued at or before
  it is refused, for every principal, so all signed-in sessions (web UI, mobile
  clients, anything holding a refresh token) must sign in again. This happens on
  one start only — the migrated file declares the format contract, so the other
  services and every later restart adopt it and change nothing — and the cutoff
  expires on its own after 8 days.

  Why it is not avoidable: those builds could report a revocation as done while
  it never reached the shared file, and the file does not record which ones were
  lost. Keeping the old credentials working would mean a deleted account keeps
  access from the internet until its refresh token expires. Signing everyone out
  is the automatic form of the manual remedy (re-revoke every deleted user, or
  rotate the signing key).

  It is now announced instead of being discovered: the process that performs the
  migration writes a line to stderr — captured by `journalctl -u <service>` —
  naming the file, the cutoff instant and the reason, and
  `tokenrevocation.CurrentHealth()` reports `legacy_cutoff_applied` /
  `legacy_cutoff_at` for that process plus `revoked_before` for as long as the
  cutoff is in force. If someone asks "why was I logged out?", that is the
  answer, and it distinguishes this upgrade from a break-in response.

  A fresh install sets no cutoff and announces nothing.
