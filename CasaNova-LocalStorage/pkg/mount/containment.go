package mount

import (
	"errors"
	"path/filepath"
	"strings"
)

// AllowedRoots are the only directories under which a CasaNova mount may be
// created via the API. Anything outside is rejected.
//
// /mnt and /media are the standard Linux conventions for removable storage;
// /DATA is CasaNova's user-data root. Mounting *over* /etc, /usr, /var,
// /proc, /sys, /dev, /boot, or any other system path would shadow real files
// and is precisely the privilege-escalation primitive the API must not
// expose.
//
// OLA 3 — this list and its validator used to be copy-pasted, byte-identical,
// into route/v1 and route/v2 (two separate Go packages, so "shared" meant
// "kept in sync by hand"). They agreed today only because nobody had edited
// one without the other yet. Single source now.
var AllowedRoots = []string{
	"/mnt",
	"/media",
	"/DATA",
}

// ValidatePoint canonicalizes mp and rejects it unless it lies strictly
// *inside* one of AllowedRoots — never the root itself (mounting over /mnt
// would hide every other mount under it) and never outside the allowed
// roots.
func ValidatePoint(mp string) error {
	if mp == "" {
		return errors.New("mount point is required")
	}
	clean := filepath.Clean(mp)
	if !filepath.IsAbs(clean) {
		return errors.New("mount point must be absolute")
	}
	if strings.Contains(clean, "..") {
		return errors.New("mount point must not contain ..")
	}
	for _, root := range AllowedRoots {
		if clean == root {
			return errors.New("mount point cannot be the root itself; use a subdirectory")
		}
		if strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return nil
		}
	}
	return errors.New("mount point must live under /mnt, /media, or /DATA")
}

// ValidateMergePoint is ValidatePoint for a mergerfs pool, which differs in
// exactly one way: a merge point MAY be an allowed root itself.
//
// WHY A SECOND FUNCTION AND NOT A SECOND CALL TO THE FIRST. /DATA is the merge
// point -- common.DefaultMountPoint is literally "/DATA", GetMergeInitStatus
// asks whether /DATA is already merged, and EnsureDefaultMergePoint creates it
// there. ValidatePoint refuses an allowed root ("use a subdirectory"), so
// wiring it straight into InitMerge/SetMerge -- the obvious one-line fix --
// would reject the only mount point the feature is ever called with and break
// merge initialisation outright. Measured, not assumed:
//
//	ValidatePoint("/DATA") = mount point cannot be the root itself
//
// The root exception is safe here because it is the DIRECTION that matters:
// refusing everything outside /mnt, /media and /DATA is what stops
// InitMerge from being handed /etc and moving the host's configuration tree
// into /var/lib/casaos/files. Allowing the root itself does not widen that at
// all -- /DATA was always going to be a legal merge point.
func ValidateMergePoint(mp string) error {
	if mp == "" {
		return errors.New("mount point is required")
	}
	clean := filepath.Clean(mp)
	if !filepath.IsAbs(clean) {
		return errors.New("mount point must be absolute")
	}
	if strings.Contains(clean, "..") {
		return errors.New("mount point must not contain ..")
	}
	for _, root := range AllowedRoots {
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return nil
		}
	}
	return errors.New("merge point must be /mnt, /media or /DATA, or live under one of them")
}
