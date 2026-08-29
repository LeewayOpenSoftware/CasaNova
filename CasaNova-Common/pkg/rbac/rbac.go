// Package rbac defines the role and permission model for CasaNova.
//
// The design is deliberately simple — three roles with a fixed permission
// mapping, not a general policy engine. Home-server multi-tenancy needs:
//
//   - Admin: full control (install/uninstall apps, manage users, change
//     system config, modify storage, view all files)
//   - User: day-to-day operation of apps they're allowed to see; access
//     to files in their own home directory; cannot install new apps or
//     touch storage/system config
//   - Guest: read-only access to permitted apps; no file access, no
//     configuration reads, no lifecycle operations
//
// Permissions are a uint64 bitmask so "does actor have permission X" is
// a single AND, cheap enough to check on every request. New permissions
// are added by picking the next unused bit below.
//
// Role → permission mapping lives here (not in the database) so upgrading
// to a new permission is a code change, not a manual row edit.
package rbac

import "strings"

// Role is a short, stable identifier for a user's role. Stored verbatim
// in the UserDBModel.Role column and JWT claims — do not rename existing
// constants without a migration.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
	RoleGuest Role = "guest"
)

// AllRoles is the canonical list for validation in management APIs.
var AllRoles = []Role{RoleAdmin, RoleUser, RoleGuest}

// IsValid reports whether r is one of the recognized roles.
func (r Role) IsValid() bool {
	switch r {
	case RoleAdmin, RoleUser, RoleGuest:
		return true
	}
	return false
}

// Normalize returns a trimmed, lowercased role.
// Empty or unknown role fails closed to RoleGuest. The user service runs
// a one-time DB migration on startup that backfills any legacy rows with
// empty role to "admin", so an empty Role at this point can only come
// from a forged or otherwise malformed claim — never grant admin to it.
func Normalize(s string) Role {
	r := Role(strings.ToLower(strings.TrimSpace(s)))
	if r == "" || !r.IsValid() {
		return RoleGuest
	}
	return r
}

// Permission is a single capability bit. Permissions are OR-ed together
// into a Set, which a role holds and a route requires.
type Permission uint64

// Permission bits. Keep additions append-only — existing bits cannot be
// renumbered without invalidating stored data (JWTs in flight, etc.).
const (
	// File manager
	PermFileRead   Permission = 1 << iota // read files and directories
	PermFileWrite                         // create, modify, rename files
	PermFileDelete                        // move to trash, permanent delete
	PermFileShare                         // create samba shares, public links

	// Apps
	PermAppView      // see apps in the dashboard
	PermAppStart     // start/stop/restart containers
	PermAppInstall   // install new apps, update, uninstall
	PermAppConfigure // edit compose files, ports, volumes

	// Storage
	PermStorageRead  // list disks, SMART, usage
	PermStorageWrite // mount, unmount, format, TRIM

	// System
	PermSystemRead    // hardware info, version, logs (read)
	PermSystemConfig  // change system settings, shutdown, reboot, update
	PermSystemNetwork // network interfaces, gateway routes

	// User management
	PermUserRead   // list users, view roles
	PermUserManage // create, delete, modify other users (not self)

	// Backup / restore
	PermBackupRead  // view backup status, history
	PermBackupWrite // trigger backups, restore snapshots

	// Destructive storage maintenance. Appended at the END of the block, not
	// inserted next to the other storage bits, because iota renumbers every
	// constant below an insertion point and this block is append-only.
	//
	// They are SEPARATE bits rather than part of PermStorageWrite on purpose.
	// PermStorageWrite is held by anyone allowed to mount, unmount and trim a
	// drive. The two below destroy a physical device: a format writes a new
	// partition table and filesystem over whatever was there, and a sanitize
	// deallocates or overwrites the blocks themselves. That is materially more
	// destructive than any write PermStorageWrite covers, and the recovery
	// story is "restore from backup", not "undo". Someone trusted to unmount a
	// USB stick must NOT inherit the ability to wipe it, so these are granted
	// on their own and checked on their own.
	PermStorageSanitize // discard or overwrite the blocks of a physical device
	PermStorageFormat   // repartition and mkfs a physical device
)

// Set is an OR-ed collection of Permission bits.
type Set uint64

// Has reports whether every bit in p is also set in s.
func (s Set) Has(p Permission) bool {
	return Set(p)&s == Set(p)
}

// Add returns a new Set with p bits turned on.
func (s Set) Add(p Permission) Set { return s | Set(p) }

// Remove returns a new Set with p bits turned off.
func (s Set) Remove(p Permission) Set { return s &^ Set(p) }

// adminPerms: everything. Explicitly listed rather than MaxUint64 so a
// new permission bit defaults to NOT granted to admin — forcing the dev
// to add it intentionally. This prevents accidentally widening admin
// privileges just by introducing a new bit.
var adminPerms = Set(0).
	Add(PermFileRead).Add(PermFileWrite).Add(PermFileDelete).Add(PermFileShare).
	Add(PermAppView).Add(PermAppStart).Add(PermAppInstall).Add(PermAppConfigure).
	Add(PermStorageRead).Add(PermStorageWrite).
	Add(PermSystemRead).Add(PermSystemConfig).Add(PermSystemNetwork).
	Add(PermUserRead).Add(PermUserManage).
	Add(PermBackupRead).Add(PermBackupWrite).
	// Destructive storage maintenance: admin only, and stated here rather
	// than inherited from PermStorageWrite. Neither userPerms nor guestPerms
	// gets these — see the bits' own comment for why holding PermStorageWrite
	// must not imply being able to wipe a disk.
	Add(PermStorageSanitize).Add(PermStorageFormat)

// userPerms: operation of their own files and apps the admin granted.
// No user management, no storage writes, no system config.
var userPerms = Set(0).
	Add(PermFileRead).Add(PermFileWrite).Add(PermFileDelete).
	Add(PermAppView).Add(PermAppStart).
	Add(PermStorageRead).
	Add(PermSystemRead).
	Add(PermBackupRead)

// guestPerms: view-only, and deliberately NO file access — matching this
// package's own doc ("read-only access to permitted apps; no file access, no
// configuration reads"). PermFileRead used to be here, which combined with the
// ungated read handlers in the file API let a guest read anything outside the
// blocked-prefix list. PermSystemRead stays: several internal tokenless reads
// resolve to guest and the UI's read-only widgets depend on it.
var guestPerms = Set(0).
	Add(PermAppView).
	Add(PermSystemRead)

// PermissionsFor returns the permission set attached to a role. Unknown
// roles get the empty set (no access) — fail closed.
func PermissionsFor(r Role) Set {
	switch r {
	case RoleAdmin:
		return adminPerms
	case RoleUser:
		return userPerms
	case RoleGuest:
		return guestPerms
	}
	return 0
}

// Allows is a convenience for "does role r have permission p".
func Allows(r Role, p Permission) bool {
	return PermissionsFor(r).Has(p)
}
