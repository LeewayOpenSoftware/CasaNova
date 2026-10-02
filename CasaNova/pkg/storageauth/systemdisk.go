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
//   - It does not let anyone READ what is in a kernel pseudo-filesystem (rule
//     3). /proc, /sys, /dev and the rest may be LISTED by a switched-on account
//     (AuthorizeListing) — their entries are part of "the whole disk from /" —
//     but opening a file there stays SYSTEM_LOCATION for everyone: /proc/kcore
//     is the machine's memory, a sysfs attribute read can wake hardware (an
//     IIO read froze a Surface for seconds), and device nodes are raw disks
//     and watchdogs. An allow that could promote those reads would turn this
//     into a disable-security control.
//   - It does not hand out the appliance's own credentials. The JWT signing
//     key and the user databases are withheld (systemDiskWithheld): reading
//     the signing key is not a read, it is the ability to mint an
//     administrator's token, and the switch can be given to non-admins.
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

// SystemDiskReads reports whether the switch widens op for p: the account has
// it on, is not a guest, and op does not mutate. Handlers use it to decide
// whether the legacy blocked-prefix sanitizer applies to a read at all.
func SystemDiskReads(p Principal, op Op) bool { return systemDiskReads(p, op) }

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

// withheldPaths are files and subtrees whose CONTENTS the switch never opens,
// even though they lie outside authorized storage and the account may list
// their names. Each is a credential of the appliance or of the host: reading it
// grants more than reading. They are matched on both the requested and the
// resolved form, so a symlink to one is withheld too.
var withheldPaths = []string{
	// The CasaNova data path: the JWT signing key and the user and app
	// databases live directly in it or in db/ depending on the install.
	"/var/lib/casaos/db",
	"/var/lib/casaos/jwt_ec_private.pem",
	"/var/lib/casaos/user.db",
	"/var/lib/casaos/casaOS.db",
	// Host password hashes.
	"/etc/shadow", "/etc/shadow-", "/etc/gshadow", "/etc/gshadow-",
}

// withheldNames are the same secrets matched by file name, for an install whose
// DBPath was configured somewhere else. -wal/-shm/-journal are the SQLite
// companions, which hold recent rows of the same database.
var withheldNames = []string{"jwt_ec_private.pem", "user.db", "casaOS.db"}

// isWithheld reports whether path is one of the secrets above.
func isWithheld(path string) (string, bool) {
	for _, w := range withheldPaths {
		if within(w, path) {
			return w, true
		}
	}
	if st := stateDir(); st != "" && within(st, path) {
		return st, true
	}
	base := filepath.Base(path)
	for _, n := range withheldNames {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if base == n+suffix {
				return path, true
			}
		}
	}
	// SSH host private keys: ssh_host_<type>_key (the .pub halves are public).
	if filepath.Dir(path) == "/etc/ssh" && strings.HasPrefix(base, "ssh_host_") && strings.HasSuffix(base, "_key") {
		return path, true
	}
	return "", false
}

// systemDiskWithheld is the refusal for opening a withheld secret through the
// switch. SYSTEM_LOCATION because, like a pseudo-filesystem, it is a place the
// file manager never treats as ordinary storage and no configuration changes
// that; the Rule names which secret so an audit log is unambiguous.
func systemDiskWithheld(clean, resolved string) (Decision, bool) {
	hit, ok := isWithheld(clean)
	if !ok {
		hit, ok = isWithheld(resolved)
	}
	if !ok {
		return Decision{}, false
	}
	return Decision{
		Reason:     ReasonSystemLocation,
		Rule:       "withheld:" + hit,
		Detail:     "this file holds a credential of the appliance or the host; the system-disk switch lets it be listed but never read",
		Remediable: false,
	}, true
}

// stateDir is the authority's own state directory (grants, rules, this
// switch), read under the lock.
func stateDir() string {
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.dir
}
