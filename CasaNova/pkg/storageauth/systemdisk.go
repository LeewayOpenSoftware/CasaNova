package storageauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The per-account system-disk switch: may this principal BROWSE the whole disk
// from "/"?
//
// WHY IT IS NOT A GRANT. A grant is the obvious shape and it cannot work.
// Grants are consulted at rules 8-10, AFTER the ceiling has already decided
// whether CasaNova may manage the object at all, and "/" fails that ceiling at
// rule 7 with OUTSIDE_AUTHORIZED_STORAGE. AddGrant refuses a path outside the
// ceiling for exactly that reason: an entitlement the ceiling does not cover
// would be a promise the authority cannot keep. So this switch has to act on
// rule 7 itself, and it is placed as a NARROWING of that default deny rather
// than as a bypass around the list.
//
// WHY IT LIVES HERE AND NOT IN THE USER RECORD. The role arrives in a JWT
// claim, and a switch carried in the token would only take effect when the
// token was reissued. In the ON direction that is merely confusing; in the OFF
// direction it is a hole — an administrator revoking whole-disk access would
// not actually revoke it until the session expired. Keeping the fact in the
// authority's own persisted state makes both directions take effect on the next
// request, and it is the same place the grants it resembles already live.
//
// WHAT IT DOES NOT DO:
//
//   - It does not permit writing. Only non-mutating operations are widened; a
//     write outside authorized storage is still refused, and writing inside it
//     still needs a root plus a grant exactly as before.
//   - It does not touch rule 3. /proc, /sys and the rest stay refused with
//     SYSTEM_LOCATION for everyone, because the package's own contract says an
//     allow must never override that: a switch that could would turn this into
//     a disable-security control.
//   - It does not touch rule 4. An operator DENY still wins, so a subtree an
//     administrator excluded stays excluded for switched-on accounts too.
//   - It is never available to a guest. Enforced when the switch is set AND
//     again when it is read, because the two checks fail differently: the
//     setter refuses an administrator's mistake, and the decision refuses a
//     record that became wrong afterwards — a switched-on account later
//     demoted to guest.

const systemDiskFileName = "storage-system-disk.json"

// roleGuest is the role string the RBAC model uses. Compared as a string rather
// than imported so this package keeps its no-cycle property.
const roleGuest = "guest"

type systemDiskFile struct {
	Version int `json:"version"`
	// PrincipalIDs are the accounts the switch is ON for. Absence is OFF, so a
	// missing file, an empty list and an unknown account all mean the same
	// thing, and the default for every account — administrators included — is
	// no whole-disk access.
	PrincipalIDs []string `json:"principal_ids"`
}

// SetSystemDiskAccess turns the switch on or off for one account, durably.
//
// role is required so guest can be refused here and not only at decision time:
// an administrator who tries to grant it to the guest account gets an error
// they can see, instead of a switch that appears to be set and never applies.
func SetSystemDiskAccess(principalID, role string, on bool) error {
	global.mu.Lock()
	defer global.mu.Unlock()

	if !global.provisioned {
		return global.failLocked("the authority has not been provisioned",
			errors.New("cannot record system-disk access before provisioning"))
	}
	id := strings.TrimSpace(principalID)
	if id == "" {
		return errors.New("system-disk access must name a principal")
	}
	if strings.EqualFold(strings.TrimSpace(role), roleGuest) {
		return fmt.Errorf("the guest account may never be given system-disk access")
	}

	next := make([]string, 0, len(global.systemDisk)+1)
	for existing := range global.systemDisk {
		if existing != id {
			next = append(next, existing)
		}
	}
	if on {
		next = append(next, id)
	}
	sort.Strings(next)

	if err := writeAtomic(filepath.Join(global.dir, systemDiskFileName),
		systemDiskFile{Version: schemaVersion, PrincipalIDs: next}); err != nil {
		return err
	}

	global.systemDisk = make(map[string]struct{}, len(next))
	for _, p := range next {
		global.systemDisk[p] = struct{}{}
	}
	return nil
}

// SystemDiskAccessFor reports whether the switch is on for one account. It says
// nothing about whether a given operation is permitted; that is systemDiskReads.
func SystemDiskAccessFor(principalID string) bool {
	global.mu.RLock()
	defer global.mu.RUnlock()
	_, on := global.systemDisk[strings.TrimSpace(principalID)]
	return on
}

// SystemDiskPrincipals lists the accounts the switch is on for, so an
// administrative UI can render the switches without guessing.
func SystemDiskPrincipals() []string {
	global.mu.RLock()
	defer global.mu.RUnlock()
	out := make([]string, 0, len(global.systemDisk))
	for id := range global.systemDisk {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// systemDiskReads reports whether p may perform op by virtue of the switch.
//
// Callers must hold no lock; it takes the read lock itself.
func systemDiskReads(p Principal, op Op) bool {
	if opMutates(op) {
		return false
	}
	if strings.EqualFold(p.Role, roleGuest) {
		return false
	}
	return SystemDiskAccessFor(p.ID)
}

// loadSystemDisk reads the persisted switch list. A missing file is the normal
// state of a box where nobody has been given whole-disk access, and reads as an
// empty set. An UNREADABLE file is not: it is returned as an error so the
// caller can fail the authority rather than silently treat a corrupt file as
// "nobody is switched on" — which would look identical to the safe default and
// hide the real problem.
func loadSystemDisk(path string) (map[string]struct{}, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f systemDiskFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(f.PrincipalIDs))
	for _, id := range f.PrincipalIDs {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// systemDiskDecision is the allowed Decision the switch produces for a path the
// ceiling would otherwise refuse at rule 7.
//
// ConfineRoot is "/" because that is genuinely the boundary being authorized:
// confining to anything narrower would be a lie about what was decided, and
// OpenAuthorized needs the real one to resolve beneath.
func systemDiskDecision(clean, resolved string) Decision {
	return Decision{
		Allowed:     true,
		Reason:      ReasonAllowedSystemDisk,
		Path:        clean,
		Rule:        "system-disk-access",
		Detail:      "this account has the system-disk switch on, which permits reading outside authorized storage",
		ConfineRoot: "/",
		Resolved:    resolved,
	}
}
