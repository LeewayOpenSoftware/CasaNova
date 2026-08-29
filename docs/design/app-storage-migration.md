# Design plan — Move app data folders between disks

> Status: **planning only** (no implementation yet). Requested 2026-07-17.
> Goal: let a user see every app's data folder(s) and move them to another
> disk from the UI, safely, with the app coming back up pointing at the new
> location.

## Why this is non-trivial

CasaNova apps are compose stacks whose persistent data lives in **bind mounts**
under `/DATA/AppData/<AppID>/...` (e.g. `/DATA/AppData/immich/postgres`). The
data is on whatever filesystem backs `/DATA`. "Moving an app to another disk"
means:

1. Relocating the actual bytes to the target disk (a cross-filesystem copy, not
   a rename).
2. Rewriting the compose file's `volumes[].source` paths to the new location.
3. Recreating the container so it mounts the new path.
4. Doing all of that **without data loss** even if the box loses power mid-move.

A naïve `mv` fails or is unsafe because: the source and target are different
filesystems (rename doesn't work, it's a copy+delete); the container may be
holding the files open; a partial copy interrupted by a crash must not be
mistaken for a complete one; and permissions/ownership/xattrs must survive.

## Current state (what exists to build on)

- **App data path:** `/DATA/AppData/<AppID>/...`, referenced verbatim as
  `volumes[].source` bind mounts in `/var/lib/casaos/apps/<id>/docker-compose.yml`.
- **Disk inventory:** CasaNova-LocalStorage exposes the disk/mount list; mounted
  data disks live under `/mnt/*` and `/media/*`. The dashboard's Disk-health
  widget already reads per-disk SMART + free space.
- **App lifecycle:** CasaNova-AppManagement can stop/recreate a compose app and
  now reconciles orphaned containers (`Repair`, startup sweep). The compose
  file is the source of truth for mounts.
- **Move primitive:** none yet. `rsync` is the natural tool (preserves perms,
  resumable, `--checksum` verify); confirm it's a shipped dependency.

## Proposed UX

A new **"Storage"** section on each app's detail page (or a global "App storage"
view):

1. **List** every bind-mount folder the app owns, each with:
   its host path, current disk (derived from the mountpoint the path falls
   under), and size (`du -sb`, cached — sizing can be slow for big folders).
2. **"Move to…"** per folder (or "move all of this app's data"): a picker of
   target disks showing free space, with a guard that refuses a target with
   insufficient space (size + headroom).
3. A **progress panel** (reuse the file-transfer progress surface) showing
   copy %, current file, and a cancel button while it's still safe to cancel.
4. On completion: a short report (moved N GiB, app restarted, old location
   reclaimed) with a **rollback** affordance until the old copy is deleted.

## Proposed engineering (backend)

A single **transactional move** operation per app (or per folder), driven by a
new AppManagement endpoint, executed as a background job with a status file the
UI polls (mirror the self-update engine's status-file pattern):

1. **Pre-flight**
   - Resolve the app's bind-mount sources from its compose file.
   - Validate the target: exists, is a mounted writable filesystem under
     `/mnt` or `/media`, has `size + 10%` free.
   - Refuse if the target path would collide with an existing app's data.
   - Snapshot the compose file (backup) for rollback.
2. **Stop the app** (compose stop) so no process holds the files open — a hot
   copy of an open DB is inconsistent. Publish "stopping" status.
3. **Copy** with `rsync -aHAX --info=progress2` (or two-pass: `rsync` while
   running for the bulk, then a final `rsync` after stop for the delta — an
   optimization, not v1) to `<target>/casanova-appdata/<AppID>/...`. Preserve
   perms/ACLs/xattrs/hardlinks. Stream progress to the status file.
4. **Verify** the copy: compare file counts + a `rsync --checksum --dry-run`
   pass (or `du` byte totals as a cheaper gate). Abort → rollback on mismatch.
5. **Rewrite** the compose file's `volumes[].source` for the moved paths to the
   new location. Write atomically (temp + rename).
6. **Recreate + start** the app via the existing compose-up path (the same one
   Repair uses). Verify all services reach running.
7. **Commit:** only after the app is confirmed healthy on the new path, delete
   the **old** data folder. Until this step, nothing is destroyed — a crash at
   any earlier point leaves the original intact and the compose file restorable
   from the backup.
8. **Rollback** (on any failure before commit): restore the compose backup,
   remove the partial target copy, restart the app on the original path.

### State machine (crash-safe)

```
preflight → stopped → copying → verifying → rewrite-compose → starting → verify-health → commit(delete-old)
     └────────────────────────── rollback ──────────────────────────────┘ (any failure before commit)
```

Persist the current phase to a job file so a reboot mid-move can resume or
cleanly roll back on next boot (same idea as the app orphan sweep).

## Edge cases to handle

- **Named volumes** (not bind mounts) — Docker-managed volumes live under
  `/var/lib/docker/volumes`, a different beast. v1 scope = bind mounts under
  `/DATA/AppData` only; detect and skip/flag named volumes.
- **Symlinked or shared paths** — two apps sharing a folder, or a path that's
  already a symlink. Detect and refuse rather than corrupt.
- **The app's data IS on the system disk and can't be unmounted** — fine, we're
  copying out of it, not unmounting it.
- **Target disk unmounts mid-move** (USB pulled) — the job must fail closed and
  roll back, never leave the compose pointing at a vanished path.
- **Very large folders** (media libraries, TBs) — sizing + copy can take hours;
  the job must survive UI disconnects (it already runs server-side) and the app
  stays down for the whole copy in the simple design → surface an ETA and warn.
  A two-pass online copy (bulk while running, delta after stop) shortens
  downtime and is the natural v2.
- **Permissions/UID mapping** — some apps run as specific UIDs; `rsync -a`
  preserves numeric ownership, which is what we want across disks.
- **Free-space accounting** — reserve headroom; a target that fills up mid-copy
  must abort + roll back.
- **Path uniqueness on target** — namespace under `<target>/casanova-appdata/`
  so moves from multiple sources don't collide and are easy to identify.

## Open questions (for when we build it)

- Per-folder moves, or always move an app's whole data set together? (Per-folder
  is more flexible but multiplies the compose-rewrite surface.)
- Do we offer a "symlink instead of rewrite" mode (leave a symlink at the old
  path)? Simpler for apps with hardcoded paths, but symlinks-across-disks are a
  footgun; prefer the compose-rewrite approach.
- Should the target layout mirror `/DATA/AppData/<AppID>` exactly (so a future
  "move back" is trivial) or be free-form?
- Is `rsync` guaranteed present, or do we bundle/require it (like we'll require
  `genisoimage` for the VM cloud-init work)?

## Rough phasing

- **P1** — read-only: list each app's folders, disk, and size. (Low risk; ships
  the visibility half of the value.)
- **P2** — the transactional move (stop → rsync → rewrite → start → verify →
  commit) with the crash-safe state machine and rollback.
- **P3** — online two-pass copy to cut downtime; per-folder granularity; "move
  back" shortcut.

---
No code is written for this feature yet — this is the plan to react to before we
commit engineering time.
