package oauthflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func hashState(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ownedByUs asserts the file/directory belongs to this process's effective uid.
// On a platform whose FileInfo carries no uid we refuse rather than assume —
// this whole subsystem only ever runs on Linux, so an unknown Sys() shape means
// something we do not understand and must not trust.
func ownedByUs(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine owner")
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("owned by uid %d, not %d", st.Uid, os.Geteuid())
	}
	return nil
}

// RemoteInfo is the subset of an rclone remote this package reasons about.
type RemoteInfo struct {
	Name       string
	Type       string
	MountPoint string
}

// oauthRemoteTypes are the rclone backends the OAuth callback path creates.
var oauthRemoteTypes = map[string]bool{"drive": true, "dropbox": true, "onedrive": true}

// IsOAuthShaped reports whether a remote has the shape this callback path
// produces: one of the three OAuth backends AND a mount point. The mount-point
// condition keeps administrator-created backup remotes (POST /v1/sys/cloud/remotes,
// which are not mounted) out of the audit.
func (r RemoteInfo) IsOAuthShaped() bool {
	return oauthRemoteTypes[r.Type] && r.MountPoint != ""
}

// RemoteRecord is the provenance stamp for a remote this authority created.
type RemoteRecord struct {
	Provider    Provider  `json:"provider"`
	FlowID      string    `json:"flow_id"`
	PrincipalID int       `json:"principal_id"`
	MountPoint  string    `json:"mount_point"`
	CreatedAt   time.Time `json:"created_at"`
}

// ledger records what this authority created, plus a one-time baseline of what
// already existed the first time it could look.
//
// The baseline matters because of deployment semantics: a box that ran the
// pre-fix release can already hold injected remotes, and route/init.go's
// CheckAndMountAll re-mounts every persisted remote on every boot — so the
// dangerous state outlives the vulnerable binary. This package does NOT delete
// them: silently unmounting what may be the user's real Google Drive would be a
// worse outcome than the bug. It separates them instead, so "was here before the
// authority existed" (review these) is distinguishable from "appeared after the
// authority existed without passing through it" (investigate these).
type ledger struct {
	Captured   bool                    `json:"captured"`
	CapturedAt time.Time               `json:"captured_at"`
	Baseline   []string                `json:"baseline"`
	Created    map[string]RemoteRecord `json:"created"`
}

// EnsureBaseline records the pre-existing OAuth-shaped remotes, once.
//
// listErr is the caller's error from enumerating rclone. It is a parameter and
// not a detail the caller swallows because an EMPTY list caused by rclone being
// down must never be committed as the baseline: that would silently reclassify
// every real remote as "appeared after the authority" and cry wolf forever.
func (a *Authority) EnsureBaseline(remotes []RemoteInfo, listErr error) {
	if listErr != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ledger.Captured {
		return
	}
	names := make([]string, 0, len(remotes))
	for _, r := range remotes {
		if r.IsOAuthShaped() {
			names = append(names, r.Name)
		}
	}
	sort.Strings(names)
	a.ledger.Captured = true
	a.ledger.CapturedAt = a.now()
	a.ledger.Baseline = names
	a.rewriteLocked()
}

// RecordRemote stamps a remote as created by a completed flow. Called after the
// credential is persisted and the mount succeeded, so the ledger describes what
// actually exists.
func (a *Authority) RecordRemote(name string, rec RemoteRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ledger.Created == nil {
		a.ledger.Created = make(map[string]RemoteRecord)
	}
	a.ledger.Created[name] = rec
	a.rewriteLocked()
}

// ForgetRemote removes a stamp. Used by the rollback path when a mount fails
// and the credential is deleted again — a ledger entry for a remote that no
// longer exists would make the audit under-report.
func (a *Authority) ForgetRemote(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.ledger.Created, name)
	a.rewriteLocked()
}

// Health is the structured self-report. FailedInvariant names the invariant
// that is not holding, in words, so an operator reading it does not have to
// infer which property broke.
type Health struct {
	Healthy         bool   `json:"healthy"`
	FailedInvariant string `json:"failed_invariant,omitempty"`
	Detail          string `json:"detail,omitempty"`

	StorePath string `json:"store_path"`
	Durable   bool   `json:"durable"`

	PendingFlows int    `json:"pending_flows"`
	Issued       uint64 `json:"issued"`
	Consumed     uint64 `json:"consumed"`
	Rejected     uint64 `json:"rejected_callbacks"`
	Evicted      uint64 `json:"evicted"`

	BaselineCaptured    bool     `json:"baseline_captured"`
	PreAuthorityRemotes int      `json:"pre_authority_remotes"`
	AttributedRemotes   int      `json:"attributed_remotes"`
	UnattributedRemotes []string `json:"unattributed_remotes,omitempty"`
}

// Health audits the live remote list against the ledger and reports.
//
// remotes/listErr come from the caller because this package deliberately owns
// no rclone client. When the list cannot be read, the audit fields are reported
// as unknown rather than as zero.
//
// An entry in UnattributedRemotes is a REVIEW SIGNAL, not proof of compromise:
// it means an OAuth-shaped, mounted remote exists that neither predates this
// authority nor was created by a completed flow. A pre-fix injection looks like
// this; so does an administrator who hand-built a mounted drive remote through
// another endpoint. It is reported, never acted on.
func (a *Authority) Health(remotes []RemoteInfo, listErr error) Health {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked()

	h := Health{
		Healthy:          true,
		StorePath:        a.storePath(),
		Durable:          a.durable,
		PendingFlows:     len(a.flows),
		Issued:           a.issued,
		Consumed:         a.consumed,
		Rejected:         a.rejected,
		Evicted:          a.evicted,
		BaselineCaptured: a.ledger.Captured,
	}

	if !a.durable {
		h.Healthy = false
		h.FailedInvariant = "state_store_ownership"
		h.Detail = "pending-flow mirror is disabled; the authority is running from memory only " +
			"and in-flight authorizations do not survive a restart. Authorization itself is unaffected. " +
			a.storeFailure
	}

	if listErr != nil {
		h.Detail = strings.TrimSpace(h.Detail + " remote audit unavailable: " + listErr.Error())
		return h
	}

	baseline := make(map[string]bool, len(a.ledger.Baseline))
	for _, n := range a.ledger.Baseline {
		baseline[n] = true
	}
	for _, r := range remotes {
		if !r.IsOAuthShaped() {
			continue
		}
		switch {
		case baseline[r.Name]:
			h.PreAuthorityRemotes++
		case a.ledger.Created[r.Name].FlowID != "":
			h.AttributedRemotes++
		case !a.ledger.Captured:
			// No baseline yet: everything is "pre-authority" as far as we can
			// prove. Counting it as unattributed here would be an invented
			// alarm, which is worse than a missing one.
			h.PreAuthorityRemotes++
		default:
			h.UnattributedRemotes = append(h.UnattributedRemotes, r.Name)
		}
	}
	sort.Strings(h.UnattributedRemotes)
	if len(h.UnattributedRemotes) > 0 && h.FailedInvariant == "" {
		h.Healthy = false
		h.FailedInvariant = "remote_provenance"
		h.Detail = strings.TrimSpace(h.Detail + " " + strconv.Itoa(len(h.UnattributedRemotes)) +
			" mounted cloud remote(s) exist that no completed flow created; review them.")
	}
	return h
}

// remoteNameMax bounds the identity-derived part of a remote name. rclone
// section names and the mount path both live in bounded namespaces, and a
// provider is free to return a very long display name.
const remoteNameMax = 32

// SanitizeRemoteName turns a PROVIDER-SUPPLIED identity into a remote name that
// is safe as both an rclone section name and the final path segment of a mount
// point.
//
// The pre-authority code did `strings.Split(email, "@")[0]` and concatenated the
// result straight into "/mnt/"+name. That treats the provider's user-info
// response as trusted path input: everything after the callback is attacker
// influenced, and only the provider's own formatting rules stood between that
// string and a root FUSE mount at an arbitrary path. Here the identity is
// reduced to [A-Za-z0-9_-], so no separator, traversal element or leading dash
// can survive regardless of what the provider returns.
func SanitizeRemoteName(identity string, p Provider, unix int64) string {
	if i := strings.IndexByte(identity, '@'); i >= 0 {
		identity = identity[:i]
	}
	var b strings.Builder
	for _, r := range identity {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
		if b.Len() >= remoteNameMax {
			break
		}
	}
	clean := strings.Trim(b.String(), "-_")
	if clean == "" {
		clean = "user"
	}
	return clean + "_" + p.Slug() + "_" + strconv.FormatInt(unix, 10)
}

// MountRoot is where cloud remotes are mounted, unchanged from the pre-authority
// behaviour.
const MountRoot = "/mnt"

// MountPointFor builds the mount path for a sanitized remote name and refuses
// anything that does not land directly under MountRoot. SanitizeRemoteName
// already makes that impossible; this is the assertion that says so out loud,
// because the consumer of this value is a root-privileged mount.
func MountPointFor(name string) (string, error) {
	if name == "" {
		return "", errors.New("oauthflow: empty remote name")
	}
	p := path.Join(MountRoot, name)
	if path.Dir(p) != MountRoot || path.Base(p) != name {
		return "", fmt.Errorf("oauthflow: unsafe remote name %q", name)
	}
	return p, nil
}
