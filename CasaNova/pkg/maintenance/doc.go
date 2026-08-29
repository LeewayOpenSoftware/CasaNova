// Package maintenance reclaims disk space that belongs to CasaNova itself or
// to the OS, one explicitly-catalogued category at a time.
//
// It is the engine behind Settings → Maintenance. The design that governs it
// is docs/design/maintenance-module.md; §4 of that document is a list of
// non-negotiable implementation rules, and the code below is organised around
// them rather than around the categories:
//
//   - guard.go   — rule 3: the SINGLE deletion chokepoint. Every unlink in this
//     package goes through Guard.Remove. Nothing else in the package calls
//     os.Remove/os.RemoveAll on a scanned path.
//   - fsops.go   — rules 4 and 5: openat(O_NOFOLLOW|O_DIRECTORY) descent from a
//     compiled-in root, unlinkat against a verified dirfd, and revalidation
//     (identity, kind, mtime, open fd) immediately before each unlink.
//   - procfd.go  — rule 6: open-descriptor detection by (st_dev, st_ino), never
//     by path, because logrotate renames a file out from under its writer.
//   - usage.go   — rule 14: st_blocks*512 per UNIQUE inode, not st_size; the
//     orphan journal is sparse (375 MB apparent, 180 MB real).
//   - manifest.go — rule 12: the plan is written to disk BEFORE anything is
//     touched, and entries are ordered leaves-first.
//   - catalog.go — the compiled category map. The API takes a KEY from this
//     map plus opaque item IDs, never a path from the client.
//
// Two properties are worth stating once, because everything else leans on them:
//
//  1. This package never recurses at delete time. A tree is expanded into an
//     ordered list of leaves at PLAN time; at run time every removal is either
//     an unlinkat of one name or an rmdir of a directory that must already be
//     empty. There is no RemoveAll anywhere. A directory can therefore never
//     take unexpected content with it — rmdir answers ENOTEMPTY instead.
//
//  2. Deleting less than we could is a bug report; deleting more than we should
//     is unrecoverable data loss on a box with 5.4 TB of the user's files. Every
//     ambiguous case in here resolves to "skip it and say why".
package maintenance
