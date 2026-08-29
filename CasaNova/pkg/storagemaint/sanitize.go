//go:build linux

package storagemaint

// FREE-SPACE SANITIZATION
//
// This file sanitizes the FREE SPACE of a mounted filesystem without touching a
// single allocated file. It is the counterpart of the formatter: the formatter
// destroys everything, this destroys only what the user already deleted.
//
// # WHAT EACH METHOD ACTUALLY MEANS
//
// The single most dangerous thing this subsystem could do is let three very
// different mechanisms hide behind one reassuring word. They are kept apart on
// purpose, and every one of them carries a machine-readable statement of what
// it does and what it does NOT reach:
//
//	TRIM / DISCARD      Tells the storage device that a range of blocks is no
//	                    longer in use. The host writes nothing. Whether the
//	                    device ever erases the stale NAND pages, and when, is
//	                    firmware's business and is not observable from here.
//	                    This is NOT a cryptographic erase and NOT a proof of
//	                    destruction. It is never presented as one.
//
//	ZERO-FILL           Overwrites the space the filesystem can currently hand
//	                    out, once, with zeros. Real host writes, real bytes on
//	                    the media the filesystem allocated from.
//
//	MULTI-PASS          The same thing several times with an explicitly named
//	                    sequence of patterns. Rotational media only. The
//	                    sequence is enumerated in multiSanitizePassSequences —
//	                    it is not called "DoD", "NIST" or "certified", because
//	                    this package implements a MECHANISM and holds no
//	                    certification.
//
//	DEVICE SANITIZE     A firmware primitive (ATA SANITIZE, NVMe Format with
//	                    secure erase). It is a genuinely different class of
//	                    operation with genuinely different semantics, so it has
//	                    its own constant — and this package REFUSES it rather
//	                    than quietly substituting one of the above. Silently
//	                    mapping "secure erase" onto TRIM is the exact defect
//	                    this design exists to prevent.
//
// # WHY NOT `dd if=/dev/zero until ENOSPC`
//
// Because that is a way to take a NAS down. It fills the filesystem to the last
// block, which breaks every service writing to it, can wedge the journal, and
// on a full filesystem the cleanup itself may not be possible. The writer here
// is filesystem-aware: it asks statfs how much space is available to it, holds
// back a reserve it never spends (see sanitizeReserveBytes), respects the
// per-file size limit of the target filesystem (FAT32 cannot hold a file of
// 4 GiB, so several artifacts are used), and treats ENOSPC as the expected
// terminal condition it is rather than as a catastrophe.
//
// # WHAT A CRASH LEAVES BEHIND
//
// Artifacts are named so they are unmistakably CasaNova temporary state, and
// the job manifest is written and fsynced BEFORE any large file is created. On
// the next start, RecoverSanitizeJobs finds the interrupted job, PROVES the
// artifacts are ours, removes only those, and verifies the space came back. A
// crash must never leave a disk that merely looks full forever.
//
// A directory that looks like ours but cannot be proven to be ours is REPORTED
// and NOT deleted. Deleting a user's data because its name matched a pattern is
// worse than leaving a stray directory an operator can see in health output.

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/blockdev"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
)

// ---------------------------------------------------------------- methods

// SanitizeMethod names a free-space sanitization mechanism. The constants are
// deliberately not interchangeable: see the package comment above.
type SanitizeMethod string

const (
	// SanitizeTrim issues discard for the filesystem's unused blocks.
	SanitizeTrim SanitizeMethod = "trim"
	// SanitizeZeroFill overwrites allocatable free space once with zeros.
	SanitizeZeroFill SanitizeMethod = "zero_fill"
	// SanitizeMultiPass overwrites allocatable free space several times.
	SanitizeMultiPass SanitizeMethod = "multi_pass"
	// SanitizeDeviceErase is a DEVICE-LEVEL firmware primitive. It is declared
	// so that callers have a name for it that is not one of the above, and it
	// is refused by this package.
	SanitizeDeviceErase SanitizeMethod = "device_sanitize"
)

// SanitizeSemantics is the honest description of one method. The UI renders
// this verbatim; nothing here may claim more than the code does.
type SanitizeSemantics struct {
	Method SanitizeMethod `json:"method"`
	// UILabel is the exact wording the interface may show.
	UILabel string `json:"ui_label"`
	// HostOverwrites reports whether this host writes bytes over the free
	// space. False for TRIM: TRIM writes nothing.
	HostOverwrites bool `json:"host_overwrites"`
	// CryptographicErase reports whether data is rendered unrecoverable by
	// destroying a key. This package never does that, so it is always false.
	CryptographicErase bool `json:"cryptographic_erase"`
	// DeviceLevelPrimitive reports whether a firmware sanitize command is
	// used. Always false here.
	DeviceLevelPrimitive bool `json:"device_level_primitive"`
	// Implemented reports whether this package can perform the method at all.
	Implemented bool `json:"implemented"`
	// WhatItDoes is the claim, stated no more strongly than the code earns.
	WhatItDoes string `json:"what_it_does"`
	// DoesNotReach enumerates what survives. A sanitization feature that does
	// not say what it misses is selling a guarantee it does not have.
	DoesNotReach []string `json:"does_not_reach"`
	// RotationalOnly gates multi-pass to spinning disks.
	RotationalOnly bool `json:"rotational_only"`
	// DeterminateProgress reports whether byte-accurate progress and an ETA
	// exist. False for TRIM, where neither the kernel nor the device reports
	// how far along the operation is.
	DeterminateProgress bool `json:"determinate_progress"`
	// WriteCostNote explains endurance cost where there is one.
	WriteCostNote string `json:"write_cost_note,omitempty"`
}

// sanitizeCommonMisses is what NO host-side free-space overwrite can reach. It
// is attached to every overwriting method so the limitation travels with the
// claim instead of living in documentation nobody reads.
var sanitizeCommonMisses = []string{
	"blocks the filesystem has retired or the device has remapped as bad",
	"flash over-provisioning and stale NAND pages the controller has not yet erased",
	"data still referenced by a snapshot, a reflink or an open deleted file",
	"the tail of the last block of a file (file slack), which is allocated space",
	"filesystem metadata and journal internals, which are not free space",
	"any copy that already left this filesystem (backups, caches, other disks)",
}

// SanitizeSemanticsFor is the exported form: the HTTP layer renders these
// verbatim instead of maintaining its own parallel wording, which is how a UI
// string ends up promising more than the engine delivers.
func SanitizeSemanticsFor(m SanitizeMethod) SanitizeSemantics { return sanitizeSemanticsFor(m) }

// ImplementedSanitizeMethods is the closed set the engine can actually perform.
// The API offers exactly this and nothing else.
func ImplementedSanitizeMethods() []SanitizeMethod {
	var out []SanitizeMethod
	for _, m := range []SanitizeMethod{SanitizeTrim, SanitizeZeroFill, SanitizeMultiPass, SanitizeDeviceErase} {
		if sanitizeSemanticsFor(m).Implemented {
			out = append(out, m)
		}
	}
	return out
}

func sanitizeSemanticsFor(m SanitizeMethod) SanitizeSemantics {
	switch m {
	case SanitizeTrim:
		return SanitizeSemantics{
			Method: SanitizeTrim, UILabel: "Discard unused blocks (TRIM)",
			HostOverwrites: false, Implemented: true, DeterminateProgress: false,
			WhatItDoes: "Tells the storage device which blocks the filesystem no longer uses. " +
				"The host writes nothing. What the device does with the data in those blocks, " +
				"and when, is decided by its firmware and cannot be observed or verified from here.",
			DoesNotReach: []string{
				"this is NOT a cryptographic erase and NOT a proof that anything was destroyed",
				"a device may report the discard as complete and still return the old data",
				"blocks still allocated to files are untouched, as intended",
			},
			WriteCostNote: "No host writes, so no measurable wear from this operation.",
		}
	case SanitizeZeroFill:
		return SanitizeSemantics{
			Method: SanitizeZeroFill, UILabel: "Overwrite free space with zeros (single pass)",
			HostOverwrites: true, Implemented: true, DeterminateProgress: true,
			WhatItDoes: "Writes zeros over the space this filesystem can currently allocate, once, " +
				"then removes what it wrote. Allocated files are never opened or modified.",
			DoesNotReach: sanitizeCommonMisses,
			WriteCostNote: "Writes approximately one filesystem's worth of free space. On flash this is " +
				"high-write work with real endurance cost and is NOT stronger than a device-level " +
				"sanitize; it is offered only when explicitly acknowledged and is never chosen automatically.",
		}
	case SanitizeMultiPass:
		return SanitizeSemantics{
			Method: SanitizeMultiPass, UILabel: "Multi-pass overwrite (HDD only)",
			HostOverwrites: true, Implemented: true, DeterminateProgress: true, RotationalOnly: true,
			WhatItDoes: "Writes an explicitly named sequence of patterns over the space this filesystem " +
				"can currently allocate, one pass at a time, freeing the space between passes. " +
				"The sequence is listed in the plan. This is a mechanism, not a certification: " +
				"CasaNova holds no standards accreditation and claims none.",
			DoesNotReach: append(append([]string{}, sanitizeCommonMisses...),
				"successive passes are not guaranteed to land on the same physical blocks — the "+
					"filesystem allocator decides, so this is a heuristic and not a device-level guarantee"),
			WriteCostNote: "Each pass writes approximately one filesystem's worth of free space. " +
				"Not offered for solid-state media, where repeated overwriting costs endurance " +
				"without a corresponding benefit.",
		}
	case SanitizeDeviceErase:
		return SanitizeSemantics{
			Method: SanitizeDeviceErase, UILabel: "Device sanitize / secure erase (not available here)",
			DeviceLevelPrimitive: true, Implemented: false, DeterminateProgress: false,
			WhatItDoes: "A firmware-level erase issued to the device itself. It has established semantics " +
				"that host-side overwriting does not have, and it destroys the WHOLE device, not free space.",
			DoesNotReach: []string{
				"this subsystem does not implement it, and deliberately does not substitute TRIM or " +
					"an overwrite in its place",
			},
		}
	default:
		return SanitizeSemantics{Method: m, UILabel: string(m), Implemented: false,
			WhatItDoes: "unknown method"}
	}
}

// SanitizeMethodCatalog is the full, honest method list for a UI.
func SanitizeMethodCatalog() []SanitizeSemantics {
	return []SanitizeSemantics{
		sanitizeSemanticsFor(SanitizeTrim),
		sanitizeSemanticsFor(SanitizeZeroFill),
		sanitizeSemanticsFor(SanitizeMultiPass),
		sanitizeSemanticsFor(SanitizeDeviceErase),
	}
}

// ---------------------------------------------------------------- pass sequences

type sanitizePassKind string

const (
	passZero    sanitizePassKind = "zero"
	passPattern sanitizePassKind = "pattern"
	passRandom  sanitizePassKind = "random"
)

// SanitizePassSpec is one overwrite pass, named exactly.
type SanitizePassSpec struct {
	Index int              `json:"index"`
	Kind  sanitizePassKind `json:"kind"`
	Byte  byte             `json:"byte"`
	Label string           `json:"label"`
}

var (
	passSpecZero   = SanitizePassSpec{Kind: passZero, Byte: 0x00, Label: "zeros (0x00)"}
	passSpecFF     = SanitizePassSpec{Kind: passPattern, Byte: 0xFF, Label: "ones (0xFF)"}
	passSpecAA     = SanitizePassSpec{Kind: passPattern, Byte: 0xAA, Label: "alternating (0xAA)"}
	passSpec55     = SanitizePassSpec{Kind: passPattern, Byte: 0x55, Label: "alternating complement (0x55)"}
	passSpecRandom = SanitizePassSpec{Kind: passRandom, Label: "pseudorandom (ChaCha8 seeded from the kernel CSPRNG)"}
	passSpecClean  = SanitizePassSpec{Kind: passZero, Byte: 0x00, Label: "final zero clean pass"}
)

// multiSanitizePassSequences enumerates the EXACT sequence used for each pass
// count. Naming every pass is the point: "3-pass DoD wipe" is a marketing
// phrase that tells an operator nothing about what will be written, and the
// standard it alludes to was withdrawn. This table is the specification.
//
// Every sequence ends with a zero pass so the media is not left holding
// pseudorandom data, which would make a later forensic review of the drive
// harder rather than easier and looks like corruption to a casual reader.
var multiSanitizePassSequences = map[int][]SanitizePassSpec{
	2: {passSpecFF, passSpecClean},
	3: {passSpecFF, passSpecRandom, passSpecClean},
	4: {passSpecFF, passSpecAA, passSpecRandom, passSpecClean},
	5: {passSpecFF, passSpec55, passSpecAA, passSpecRandom, passSpecClean},
	6: {passSpecFF, passSpec55, passSpecAA, passSpecRandom, passSpecRandom, passSpecClean},
	7: {passSpecFF, passSpec55, passSpecAA, passSpecZero, passSpecRandom, passSpecRandom, passSpecClean},
}

// DefaultSanitizePasses is the multi-pass default: 0xFF, pseudorandom, zeros.
const DefaultSanitizePasses = 3

// MinSanitizePasses is 2 because one pass is zero-fill and has its own name.
const MinSanitizePasses = 2

// MaxSanitizePasses bounds a request. Seven passes on a 5 TB disk is already
// days of I/O; more is not a feature, it is an outage.
const MaxSanitizePasses = 7

func sanitizePassSequence(m SanitizeMethod, passes int) ([]SanitizePassSpec, error) {
	if m == SanitizeZeroFill {
		s := passSpecZero
		s.Index = 1
		return []SanitizePassSpec{s}, nil
	}
	seq, ok := multiSanitizePassSequences[passes]
	if !ok {
		return nil, fmt.Errorf("no pass sequence is defined for %d passes (supported: %d-%d)",
			passes, MinSanitizePasses, MaxSanitizePasses)
	}
	out := make([]SanitizePassSpec, len(seq))
	for i, s := range seq {
		s.Index = i + 1
		out[i] = s
	}
	return out, nil
}

// ---------------------------------------------------------------- fs profiles

// SanitizeFSProfile is what the writer must know about a filesystem before it
// writes anything to it. These are not cosmetic notes: MaxFileBytes changes the
// algorithm (FAT32 needs several artifacts), and the reserved-block and
// allocation semantics are why the reserve exists.
type SanitizeFSProfile struct {
	Filesystem Filesystem `json:"filesystem"`
	// MaxFileBytes is the hard per-file ceiling. 0 means "far beyond anything
	// this writer will produce".
	MaxFileBytes uint64 `json:"max_file_bytes,omitempty"`
	// SparseFiles reports whether the filesystem can represent holes. It is
	// recorded so nobody is tempted to "speed up" the writer with fallocate or
	// ftruncate: a hole or an unwritten extent overwrites NOTHING, and the
	// operation would silently become a no-op that reports success.
	SparseFiles bool `json:"sparse_files"`
	// FreeSpaceQuery names how free space is established.
	FreeSpaceQuery string `json:"free_space_query"`
	// ReservedBlocks explains the space statfs will not offer, or offers only
	// to root — which is exactly the trap a root service falls into.
	ReservedBlocks string `json:"reserved_blocks"`
	// Allocation describes how the allocator hands out the blocks we are
	// overwriting.
	Allocation string `json:"allocation"`
	// TrimSupport describes discard for this filesystem.
	TrimSupport string `json:"trim_support"`
	// ENOSPCBehaviour describes what the filesystem does as it fills.
	ENOSPCBehaviour string `json:"enospc_behaviour"`
	// CrashCleanup describes what a crash leaves and how it is recovered.
	CrashCleanup string `json:"crash_cleanup"`
}

// sanitizeFSProfiles characterizes the filesystems this writer is allowed to
// run on. A filesystem that is not in this table is REFUSED rather than handled
// with a generic default: btrfs and ZFS in particular would make the claim
// false outright, because copy-on-write and snapshots keep the old blocks
// referenced no matter how much free space is overwritten.
var sanitizeFSProfiles = map[Filesystem]SanitizeFSProfile{
	FSExt4: {
		Filesystem: FSExt4, MaxFileBytes: 0, SparseFiles: true,
		FreeSpaceQuery: "statfs(2): f_bavail x f_bsize is the space available to this writer",
		ReservedBlocks: "ext4 reserves 5% of the volume for privileged processes by default. " +
			"CasaNova runs as root, so f_bfree would let it spend that reserve and starve the " +
			"journal and every daemon that needs to write. The writer uses f_bavail and then " +
			"holds back its own reserve on top.",
		Allocation: "extent-based with delayed allocation; blocks are only bound to a file when " +
			"the page cache is written back, which is why the writer fsyncs periodically instead " +
			"of trusting write(2) to have reached the media",
		TrimSupport:     "yes, via FITRIM (fstrim) or the discard mount option",
		ENOSPCBehaviour: "write(2) returns ENOSPC cleanly; delayed allocation can surface it at fsync time too",
		CrashCleanup:    "journal replay leaves the artifacts as ordinary files; recovery removes them by name and manifest",
	},
	FSNTFS: {
		Filesystem: FSNTFS, MaxFileBytes: 0, SparseFiles: true,
		FreeSpaceQuery: "statfs(2) through the ntfs driver",
		ReservedBlocks: "NTFS reserves space for the MFT zone; it is not exposed separately, so the " +
			"writer's own reserve is the only protection against filling the volume",
		Allocation:      "cluster runs from a bitmap; the MFT zone is preferentially avoided for data",
		TrimSupport:     "yes on ntfs3; ntfs-3g support depends on the build",
		ENOSPCBehaviour: "ENOSPC from write(2); a FUSE-backed mount may surface it as EIO instead",
		CrashCleanup:    "artifacts survive as ordinary files and are removed by recovery",
	},
	FSExFAT: {
		Filesystem: FSExFAT, MaxFileBytes: 0, SparseFiles: false,
		FreeSpaceQuery: "statfs(2) from the allocation bitmap",
		ReservedBlocks: "none reserved; the volume can genuinely be filled to the last cluster, " +
			"which makes the writer's reserve the only thing standing between a maintenance " +
			"action and an unusable disk",
		Allocation:      "contiguous cluster allocation from a bitmap; no sparse files, so every written byte is a real byte",
		TrimSupport:     "kernel exfat supports FITRIM; older exfat implementations may not",
		ENOSPCBehaviour: "ENOSPC from write(2)",
		CrashCleanup:    "no journal: a crash can leave a stale directory entry, which fsck resolves; recovery removes what it can prove",
	},
	FSFAT32: {
		// The limit that changes the algorithm: a single file cannot reach
		// 4 GiB, so free space is consumed with a sequence of artifacts rather
		// than one growing file.
		Filesystem: FSFAT32, MaxFileBytes: 4<<30 - 1, SparseFiles: false,
		FreeSpaceQuery: "statfs(2) from the FAT",
		ReservedBlocks: "none reserved; FAT32 can be filled completely",
		Allocation:     "FAT chain, cluster by cluster; no sparse files",
		TrimSupport:    "kernel vfat exposes FITRIM on recent kernels only",
		ENOSPCBehaviour: "ENOSPC from write(2); the per-file limit surfaces as EFBIG, which the writer " +
			"avoids by capping every artifact below 4 GiB",
		CrashCleanup: "no journal; a crash can leave cross-linked clusters that fsck resolves; " +
			"recovery removes the artifacts it can prove are ours",
	},
}

// sanitizeFSTypeMap maps the kernel's own fstype string to the profile.
// Deliberately conservative: "fuseblk" is NOT mapped to NTFS even though
// ntfs-3g mounts that way, because the string does not prove which filesystem
// is underneath and guessing is how a destructive feature gets the answer wrong.
var sanitizeFSTypeMap = map[string]Filesystem{
	"ext2": FSExt4, "ext3": FSExt4, "ext4": FSExt4,
	"ntfs": FSNTFS, "ntfs3": FSNTFS,
	"exfat": FSExFAT,
	// vfat covers FAT16 and FAT32; the 4 GiB per-file limit applies to both, so
	// the FAT32 profile is the safe characterization for either.
	"vfat": FSFAT32, "msdos": FSFAT32, "fat": FSFAT32,
}

// ---------------------------------------------------------------- options

// Bounds on the writer's memory and pacing. The ceiling on ChunkBytes is the
// whole answer to "do not buffer free space in RAM": the writer allocates ONE
// buffer of at most maxSanitizeChunkBytes for an entire run, whatever the size
// of the filesystem.
const (
	minSanitizeChunkBytes     = 64 << 10
	maxSanitizeChunkBytes     = 32 << 20
	defaultSanitizeChunkBytes = 4 << 20

	defaultSanitizePartBytes = 4 << 30
	minSanitizePartBytes     = 1 << 20

	defaultSanitizeSyncEvery = 256 << 20
	defaultTrimTimeout       = 30 * time.Minute
	defaultProgressInterval  = time.Second
)

// SanitizeOptions tunes the run. Every field has a documented default; none of
// them can weaken a safety property.
type SanitizeOptions struct {
	// ChunkBytes is the single reusable write buffer. Bounded on both sides.
	ChunkBytes uint64 `json:"chunk_bytes,omitempty"`
	// MaxPartBytes caps one artifact file. Always clamped down further by the
	// filesystem's own per-file limit.
	MaxPartBytes uint64 `json:"max_part_bytes,omitempty"`
	// ReserveBytes overrides the computed safety reserve. It may RAISE the
	// reserve; it can never lower it below minSanitizeReserveBytes.
	ReserveBytes uint64 `json:"reserve_bytes,omitempty"`
	// MaxBytesPerSecond paces the writer so a maintenance action does not
	// starve everything else the appliance serves. 0 means unpaced.
	MaxBytesPerSecond uint64 `json:"max_bytes_per_second,omitempty"`
	// SyncEveryBytes bounds dirty page cache growth.
	SyncEveryBytes uint64 `json:"sync_every_bytes,omitempty"`
	// TrimTimeout bounds the discard command.
	TrimTimeout time.Duration `json:"trim_timeout,omitempty"`
	// ProgressInterval throttles the progress callback.
	ProgressInterval time.Duration `json:"progress_interval,omitempty"`
	// Progress receives a snapshot. It must not block.
	Progress func(SanitizeProgress) `json:"-"`
}

func (o SanitizeOptions) normalized(profile SanitizeFSProfile) (SanitizeOptions, error) {
	if o.ChunkBytes == 0 {
		o.ChunkBytes = defaultSanitizeChunkBytes
	}
	if o.ChunkBytes < minSanitizeChunkBytes || o.ChunkBytes > maxSanitizeChunkBytes {
		return o, fmt.Errorf("chunk size must be between %s and %s (got %s); "+
			"the writer streams and never buffers free space in memory",
			humanBytes(minSanitizeChunkBytes), humanBytes(maxSanitizeChunkBytes), humanBytes(o.ChunkBytes))
	}
	if o.MaxPartBytes == 0 {
		o.MaxPartBytes = defaultSanitizePartBytes
	}
	if o.MaxPartBytes < minSanitizePartBytes {
		return o, fmt.Errorf("maximum artifact size must be at least %s", humanBytes(minSanitizePartBytes))
	}
	// The filesystem's limit always wins. FAT32 cannot hold 4 GiB in one file,
	// so asking for a 4 GiB artifact there must produce smaller artifacts, not
	// an EFBIG hours into the run.
	if profile.MaxFileBytes > 0 && o.MaxPartBytes > profile.MaxFileBytes {
		o.MaxPartBytes = profile.MaxFileBytes
	}
	if o.MaxPartBytes < o.ChunkBytes {
		o.ChunkBytes = o.MaxPartBytes
	}
	if o.SyncEveryBytes == 0 {
		o.SyncEveryBytes = defaultSanitizeSyncEvery
	}
	if o.TrimTimeout <= 0 {
		o.TrimTimeout = defaultTrimTimeout
	}
	if o.ProgressInterval <= 0 {
		o.ProgressInterval = defaultProgressInterval
	}
	return o, nil
}

// sanitizePartSizeFor is the per-artifact ceiling actually used, exported for
// tests because it is where the FAT32 limit is enforced.
func sanitizePartSizeFor(profile SanitizeFSProfile, requested uint64) uint64 {
	if requested == 0 {
		requested = defaultSanitizePartBytes
	}
	if profile.MaxFileBytes > 0 && requested > profile.MaxFileBytes {
		return profile.MaxFileBytes
	}
	return requested
}

// ---------------------------------------------------------------- the reserve

// The safety reserve. The writer NEVER intentionally consumes the last of the
// filesystem, because a full data volume on an appliance is an outage: services
// stop writing, the journal has nowhere to go, and on some filesystems the
// cleanup itself needs a block to proceed.
const (
	// sanitizeReserveFraction of the volume, for metadata, journal, CasaNova's
	// own bookkeeping, the artifacts' own directory entries, and enough room
	// for a user to keep working while this runs.
	sanitizeReserveFraction = 0.02
	// sanitizeReserveFloor and sanitizeReserveCeiling bracket the fraction so
	// a 200 GB disk does not hold back a useless 4 GB and a 100 TB array does
	// not hold back 2 TB.
	sanitizeReserveFloor   = 256 << 20
	sanitizeReserveCeiling = 8 << 30
	// minSanitizeReserveBytes is the absolute floor. No option, no caller and
	// no configuration may go below it.
	minSanitizeReserveBytes = 64 << 20
	// sanitizeReserveMaxShare stops the reserve from swallowing a small
	// filesystem entirely, which would make sanitization impossible on a 1 GB
	// USB stick.
	sanitizeReserveMaxShare = 4
)

// sanitizeReserveBytes computes the space the writer will not spend.
//
// INVARIANT (checked by sanitizeSelfTest at startup, not merely asserted here):
// the result is always at least minSanitizeReserveBytes and never exceeds the
// filesystem size.
func sanitizeReserveBytes(totalBytes uint64, override uint64) uint64 {
	r := uint64(float64(totalBytes) * sanitizeReserveFraction)
	if r < sanitizeReserveFloor {
		r = sanitizeReserveFloor
	}
	if r > sanitizeReserveCeiling {
		r = sanitizeReserveCeiling
	}
	if share := totalBytes / sanitizeReserveMaxShare; share > 0 && r > share {
		r = share
	}
	if r < minSanitizeReserveBytes {
		r = minSanitizeReserveBytes
	}
	// An operator may only ever RAISE the reserve. Allowing it to be lowered
	// would put "fill the disk to 100%" one config field away, which is the
	// behaviour this whole section exists to make unreachable.
	if override > r {
		r = override
	}
	if totalBytes > 0 && r > totalBytes {
		r = totalBytes
	}
	return r
}

// sanitizeTargetBytes is how much one pass may write: everything available
// above the reserve, and not one byte more.
func sanitizeTargetBytes(availBytes, reserveBytes uint64) uint64 {
	if availBytes <= reserveBytes {
		return 0
	}
	return availBytes - reserveBytes
}

// ---------------------------------------------------------------- refusals

// SanitizeRefusalCode is the machine-readable reason a sanitization was
// refused, so a UI explains rather than showing a greyed-out button.
type SanitizeRefusalCode string

const (
	RefuseSubsystemUnhealthy    SanitizeRefusalCode = "SANITIZE_SUBSYSTEM_UNHEALTHY"
	RefuseInvalidOptions        SanitizeRefusalCode = "INVALID_OPTIONS"
	RefuseUnauthorizedPath      SanitizeRefusalCode = "PATH_NOT_AUTHORIZED_STORAGE"
	RefuseNotAMountPoint        SanitizeRefusalCode = "NOT_A_MOUNT_POINT"
	RefuseMountTableUnreadable  SanitizeRefusalCode = "MOUNT_TABLE_UNREADABLE"
	RefuseDiscoveryIncomplete   SanitizeRefusalCode = "DEVICE_DISCOVERY_INCOMPLETE"
	RefuseDeviceProtected       SanitizeRefusalCode = "DEVICE_PROTECTED"
	RefuseDeviceUnidentifiable  SanitizeRefusalCode = "DEVICE_CANNOT_BE_IDENTIFIED"
	RefuseReadOnly              SanitizeRefusalCode = "FILESYSTEM_READ_ONLY"
	RefuseFSNotCharacterized    SanitizeRefusalCode = "FILESYSTEM_NOT_CHARACTERIZED"
	RefuseMethodUnavailable     SanitizeRefusalCode = "METHOD_UNAVAILABLE_ON_THIS_HOST"
	RefuseMultiPassOnSolidState SanitizeRefusalCode = "MULTI_PASS_NOT_OFFERED_FOR_SOLID_STATE"
	RefuseOverwriteNotAcked     SanitizeRefusalCode = "SOLID_STATE_WRITE_COST_NOT_ACKNOWLEDGED"
	RefuseDeviceEraseNotHere    SanitizeRefusalCode = "DEVICE_SANITIZE_NOT_IMPLEMENTED"
	RefuseNoSpaceAboveReserve   SanitizeRefusalCode = "NO_FREE_SPACE_ABOVE_SAFETY_RESERVE"
	RefusePlanChanged           SanitizeRefusalCode = "TARGET_CHANGED_SINCE_PLANNING"
	RefuseConfinementMissing    SanitizeRefusalCode = "KERNEL_PATH_CONFINEMENT_UNAVAILABLE"
	RefuseDeviceBusy            SanitizeRefusalCode = "DEVICE_BUSY"
)

// SanitizeRefusal is every "no" this subsystem gives, with the reason.
type SanitizeRefusal struct {
	Code          SanitizeRefusalCode         `json:"code"`
	Detail        string                      `json:"detail"`
	DeviceReasons []blockdev.ProtectionReason `json:"device_protection_reasons,omitempty"`
}

func (r *SanitizeRefusal) Error() string {
	if len(r.DeviceReasons) > 0 {
		parts := make([]string, 0, len(r.DeviceReasons))
		for _, x := range r.DeviceReasons {
			parts = append(parts, string(x))
		}
		return string(r.Code) + ": " + r.Detail + " [" + strings.Join(parts, ", ") + "]"
	}
	return string(r.Code) + ": " + r.Detail
}

func refuse(code SanitizeRefusalCode, format string, args ...interface{}) *SanitizeRefusal {
	return &SanitizeRefusal{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// SanitizeRefusalOf extracts the code from an error.
func SanitizeRefusalOf(err error) (SanitizeRefusalCode, bool) {
	var r *SanitizeRefusal
	if errors.As(err, &r) {
		return r.Code, true
	}
	return "", false
}

// ---------------------------------------------------------------- mount table

// sanitizeProcRoot is indirected so the test lab can present a mount table
// without hardware. Production reads the kernel's.
var (
	sanitizeRootsMu sync.RWMutex
	sanitizeProcDir = "/proc"
)

// SetSanitizeProcRoot redirects mount-table reads. Tests only; pair it with
// blockdev.SetRoots so the inventory and the mount table describe one world.
func SetSanitizeProcRoot(dir string) {
	sanitizeRootsMu.Lock()
	defer sanitizeRootsMu.Unlock()
	sanitizeProcDir = dir
}

func sanitizeProcRoot() string {
	sanitizeRootsMu.RLock()
	defer sanitizeRootsMu.RUnlock()
	return sanitizeProcDir
}

// sanitizeMount is one row of the mount table, reduced to what this feature
// needs: which filesystem serves a path, on which device, and whether it is
// writable.
//
// This is not a duplicate of blockdev's inventory. blockdev answers "what block
// devices exist and which may be destroyed"; this answers "which filesystem
// serves this directory and what type is it", which the inventory does not
// carry and which the writer cannot proceed without.
type sanitizeMount struct {
	Point      string
	Source     string
	DeviceName string
	FSType     string
	ReadOnly   bool
}

func readSanitizeMounts() ([]sanitizeMount, error) {
	data, err := os.ReadFile(filepath.Join(sanitizeProcRoot(), "self", "mountinfo"))
	if err != nil {
		return nil, err
	}
	var out []sanitizeMount
	for _, line := range strings.Split(string(data), "\n") {
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 6 || len(right) < 2 {
			continue
		}
		m := sanitizeMount{
			Point:  unescapeSanitizeMountinfo(left[4]),
			FSType: right[0],
			Source: right[1],
		}
		if strings.HasPrefix(m.Source, "/dev/") {
			m.DeviceName = strings.TrimPrefix(m.Source, "/dev/")
		}
		// Read-only can be set on the mount or on the superblock; either one
		// makes the write impossible, so both are checked.
		for _, opts := range []string{left[5], strings.Join(right[2:], ",")} {
			for _, o := range strings.Split(opts, ",") {
				if o == "ro" {
					m.ReadOnly = true
				}
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// deepestSanitizeMount returns the filesystem that actually serves path.
func deepestSanitizeMount(mounts []sanitizeMount, path string) (sanitizeMount, bool) {
	best, found := sanitizeMount{}, false
	for _, m := range mounts {
		if m.Point == path || strings.HasPrefix(path, strings.TrimSuffix(m.Point, "/")+"/") {
			if !found || len(m.Point) > len(best.Point) {
				best, found = m, true
			}
		}
	}
	return best, found
}

func unescapeSanitizeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			v, ok := 0, true
			for _, c := range s[i+1 : i+4] {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---------------------------------------------------------------- statfs

type sanitizeFSStat struct {
	TotalBytes uint64
	AvailBytes uint64 // f_bavail: what an unprivileged writer may use
	FreeBytes  uint64 // f_bfree: includes the privileged reserve
	BlockSize  uint64
}

// sanitizeStatfs is indirected so the test lab can present a filesystem of a
// chosen size on an ordinary temp directory. Production asks the kernel.
var sanitizeStatfs = realSanitizeStatfs

func realSanitizeStatfs(path string) (sanitizeFSStat, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return sanitizeFSStat{}, &os.PathError{Op: "statfs", Path: path, Err: err}
	}
	bs := uint64(st.Bsize)
	if bs == 0 {
		return sanitizeFSStat{}, fmt.Errorf("statfs %s reported a zero block size", path)
	}
	// f_bavail, deliberately, NOT f_bfree. CasaNova runs as root, so f_bfree
	// would include ext4's 5% privileged reserve — the writer would be allowed
	// to spend exactly the space the filesystem set aside to keep working.
	return sanitizeFSStat{
		TotalBytes: st.Blocks * bs,
		AvailBytes: st.Bavail * bs,
		FreeBytes:  st.Bfree * bs,
		BlockSize:  bs,
	}, nil
}

// ---------------------------------------------------------------- the plan

// SanitizeRequest is what a caller asks for. Note what it does NOT contain: a
// device path, and any form of force flag.
type SanitizeRequest struct {
	// Root is the MOUNT POINT of the filesystem whose free space is sanitized.
	Root string `json:"root"`
	// Method is chosen explicitly by the caller. There is no automatic
	// fallback from one method to another: a request for an overwrite that
	// cannot be performed is refused, never silently downgraded to TRIM.
	Method SanitizeMethod `json:"method"`
	// Passes applies to SanitizeMultiPass only.
	Passes int `json:"passes,omitempty"`
	// AcknowledgeSolidStateWriteCost must be set to overwrite free space on
	// non-rotational media. It is a caller acknowledgement, not a force flag:
	// it cannot make a refused target eligible, it only records that the
	// endurance cost was shown and accepted.
	AcknowledgeSolidStateWriteCost bool `json:"acknowledge_solid_state_write_cost,omitempty"`
	// PrincipalID is recorded in the audit log.
	PrincipalID string          `json:"principal_id,omitempty"`
	Options     SanitizeOptions `json:"options,omitempty"`
}

// SanitizePlan is a prepared, authorized, revalidatable operation.
type SanitizePlan struct {
	JobID  string         `json:"job_id"`
	Method SanitizeMethod `json:"method"`

	Root       string `json:"root"`
	MountPoint string `json:"mount_point"`

	Filesystem    Filesystem        `json:"filesystem"`
	FilesystemRaw string            `json:"filesystem_raw"`
	Profile       SanitizeFSProfile `json:"profile"`

	// Device is the IDENTITY the operation is bound to. The kernel name is
	// carried for display and for re-lookup only; the identity is what is
	// compared before anything destructive runs.
	Device     blockdev.DeviceIdentity `json:"device"`
	DeviceName string                  `json:"device_name"`
	// Fingerprint identifies THIS device — the partition, if the filesystem
	// sits on one. It is what the identity re-check compares.
	Fingerprint string `json:"device_fingerprint"`
	// LockFingerprint identifies the PHYSICAL device, which for a partition is
	// its parent disk. It exists separately because the exclusion lock has to
	// be taken on the hardware, not on the partition: the formatter locks the
	// parent disk, and a key that disagreed with it would let a format and a
	// sanitization run on the same drive at the same time.
	LockFingerprint string `json:"device_lock_fingerprint"`
	Rotational      bool   `json:"rotational"`
	SupportsDiscard bool   `json:"supports_discard"`

	Passes []SanitizePassSpec `json:"passes"`

	FilesystemBytes    uint64 `json:"filesystem_bytes"`
	AvailableBytes     uint64 `json:"available_bytes"`
	ReserveBytes       uint64 `json:"reserve_bytes"`
	TargetBytesPerPass uint64 `json:"target_bytes_per_pass"`
	TotalWriteBytes    uint64 `json:"total_write_bytes"`
	// CapacityWrites is how many times the free space will be written, stated
	// plainly so a multi-pass request on a 5 TB disk is a decision and not a
	// surprise.
	CapacityWrites float64 `json:"capacity_writes"`
	MaxPartBytes   uint64  `json:"max_part_bytes"`

	Semantics SanitizeSemantics `json:"semantics"`
	Warnings  []string          `json:"warnings,omitempty"`

	PrincipalID string          `json:"principal_id,omitempty"`
	Options     SanitizeOptions `json:"-"`
	PreparedAt  time.Time       `json:"prepared_at"`
}

// ArtifactDir is where this job's temporary files live.
func (p *SanitizePlan) ArtifactDir() string {
	return filepath.Join(p.Root, sanitizeArtifactDirName(p.JobID))
}

// lockKey is the exclusion key for the physical device.
//
// It falls back to the device's own fingerprint when the parent key is absent,
// which is what a plan decoded from an older release looks like. That fallback
// restores the pre-existing behaviour instead of refusing: AcquireDevice
// rejects an empty key, so treating a missing field as fatal would turn a
// narrower lock into a dead subsystem.
func (p *SanitizePlan) lockKey() string {
	if strings.TrimSpace(p.LockFingerprint) != "" {
		return p.LockFingerprint
	}
	return p.Fingerprint
}

// systemPrincipal is the identity storage operations run under when there is no
// requesting user. It is the same shape storageauth.AuthorizeSystem uses
// internally: the per-principal layer does not apply to background maintenance,
// but the CEILING still does, which is the boundary that matters here.
var systemPrincipal = storageauth.Principal{
	ID: "\x00casanova-storage-maintenance", Role: "system", Admin: true,
}

// sanitizeMutateOps is the operation set the writer needs on the target root.
const sanitizeMutateOps = storageauth.OpCreate | storageauth.OpWrite | storageauth.OpDelete

// PlanSanitize prepares and fully validates an operation without performing it.
//
// Everything that can refuse, refuses here — while nothing has been written.
func PlanSanitize(ctx context.Context, req SanitizeRequest) (*SanitizePlan, error) {
	if h := SanitizeHealthNow(); !h.Enforcing {
		return nil, refuse(RefuseSubsystemUnhealthy,
			"free-space sanitization is not in force (%s); it refuses rather than running unchecked",
			firstNonEmptyStr(h.Invariant, "the subsystem has not been provisioned"))
	}
	if ok, why := storageauth.ConfinementAvailable(); !ok {
		// Without openat2/RESOLVE_BENEATH the writer cannot prove that the
		// artifact it creates and later deletes is the same object beneath the
		// same root. A destructive feature does not run on a weaker guarantee.
		return nil, refuse(RefuseConfinementMissing,
			"the kernel cannot confine path resolution (%s); free-space sanitization refuses to run without it", why)
	}

	switch req.Method {
	case SanitizeTrim, SanitizeZeroFill, SanitizeMultiPass:
	case SanitizeDeviceErase:
		return nil, refuse(RefuseDeviceEraseNotHere,
			"device-level sanitize is a firmware primitive with different semantics and a different blast "+
				"radius; this subsystem does not implement it and will not substitute TRIM or an overwrite for it")
	default:
		return nil, refuse(RefuseInvalidOptions, "unknown sanitization method %q", req.Method)
	}

	passes := req.Passes
	if req.Method == SanitizeMultiPass {
		if passes == 0 {
			passes = DefaultSanitizePasses
		}
		if passes < MinSanitizePasses || passes > MaxSanitizePasses {
			return nil, refuse(RefuseInvalidOptions,
				"pass count must be between %d and %d (got %d); a single pass is the zero-fill method and has its own name",
				MinSanitizePasses, MaxSanitizePasses, passes)
		}
	}

	// 1. AUTHORITY. The path must be inside the storage CasaNova is authorized
	//    to manage at all. This runs before any device work so an unauthorized
	//    path never even causes an inventory scan.
	d := storageauth.AuthorizeSystem(sanitizeMutateOps, req.Root)
	if !d.Allowed {
		return nil, refuse(RefuseUnauthorizedPath,
			"%s is not inside the storage CasaNova is authorized to manage (%s: %s)",
			req.Root, d.Reason, d.Detail)
	}
	root := d.Path

	// 2. WHICH FILESYSTEM. Fail-closed: an unreadable mount table is a refusal,
	//    not an assumption.
	mounts, err := readSanitizeMounts()
	if err != nil {
		return nil, refuse(RefuseMountTableUnreadable,
			"the mount table could not be read (%v), so the filesystem serving %s cannot be established", err, root)
	}
	mnt, ok := deepestSanitizeMount(mounts, root)
	if !ok {
		return nil, refuse(RefuseNotAMountPoint, "no mounted filesystem serves %s", root)
	}
	if mnt.Point != root {
		// Requiring the mount point itself keeps "free space" unambiguous and
		// keeps the artifacts out of the middle of a user's data.
		return nil, refuse(RefuseNotAMountPoint,
			"%s is not a mount point; free space belongs to a filesystem, so the target must be the "+
				"root of one (%s is served by the filesystem mounted at %s)", root, root, mnt.Point)
	}
	if mnt.ReadOnly {
		return nil, refuse(RefuseReadOnly, "the filesystem at %s is mounted read-only", root)
	}
	if mnt.DeviceName == "" {
		return nil, refuse(RefuseDeviceUnidentifiable,
			"the filesystem at %s is served by %q, which is not a block device this subsystem can identify",
			root, mnt.Source)
	}

	fs, known := sanitizeFSTypeMap[strings.ToLower(mnt.FSType)]
	if !known {
		return nil, refuse(RefuseFSNotCharacterized,
			"free-space sanitization is not characterized for %q; it is refused rather than attempted, "+
				"because on a copy-on-write or snapshotting filesystem overwriting free space would not "+
				"remove the old data and the result would be a false assurance", mnt.FSType)
	}
	profile := sanitizeFSProfiles[fs]

	// 3. WHICH DEVICE, AND MAY IT BE TOUCHED. blockdev is the only authority.
	inv, err := blockdev.Discover()
	if err != nil {
		return nil, refuse(RefuseDiscoveryIncomplete, "block device discovery failed (%v); refusing", err)
	}
	dev, found := inv.ByName(mnt.DeviceName)
	if !found {
		return nil, refuse(RefuseDiscoveryIncomplete,
			"the block device %q backing %s is not in the inventory; refusing rather than guessing",
			mnt.DeviceName, root)
	}
	if dev.Protected {
		return nil, &SanitizeRefusal{
			Code: RefuseDeviceProtected,
			Detail: fmt.Sprintf("%s is backed by %s, which CasaNova protects; a system filesystem is never sanitized",
				root, dev.Name),
			DeviceReasons: dev.ProtectionReasons,
		}
	}
	if dev.Identity.Strength() == blockdev.StrengthWeak {
		return nil, refuse(RefuseDeviceUnidentifiable,
			"%s cannot be identified strongly enough to bind an operation to it", dev.Name)
	}
	if dev.ReadOnly {
		return nil, refuse(RefuseReadOnly, "%s is a read-only device", dev.Name)
	}

	// 4. METHOD FEASIBILITY on THIS device.
	caps := CurrentCapabilities(ctx)
	var warnings []string
	switch req.Method {
	case SanitizeTrim:
		if !dev.SupportsDiscard {
			return nil, refuse(RefuseMethodUnavailable,
				"%s does not advertise discard support, so TRIM has nothing to issue", dev.Name)
		}
		if !toolAvailable(caps, ToolFstrim) {
			return nil, refuse(RefuseMethodUnavailable,
				"the fstrim helper is not installed (provided by util-linux), so discard cannot be issued")
		}
		warnings = append(warnings,
			"TRIM tells the device these blocks are unused. It is not a secure erase and provides no "+
				"evidence that the previous contents are gone.")
	case SanitizeMultiPass:
		if !dev.Rotational {
			// Not a warning, a refusal. Repeated overwriting of a
			// wear-levelled device costs endurance and does not reach the
			// stale pages the controller has retired, so offering it would be
			// selling damage as security.
			return nil, refuse(RefuseMultiPassOnSolidState,
				"multi-pass overwrite is offered for rotational media only; %s is solid-state, where "+
					"repeated overwriting costs write endurance without reaching the pages the "+
					"controller has already retired", dev.Name)
		}
	case SanitizeZeroFill:
		if !dev.Rotational {
			if !req.AcknowledgeSolidStateWriteCost {
				return nil, refuse(RefuseOverwriteNotAcked,
					"%s is solid-state: overwriting free space writes roughly one drive's worth of data, "+
						"costs write endurance, and is NOT stronger than a device-level sanitize. "+
						"Discard (TRIM) is the appropriate operation here. Proceed only by acknowledging "+
						"the write cost explicitly.", dev.Name)
			}
			warnings = append(warnings,
				"High-write operation on solid-state media: this writes approximately the whole free "+
					"space once and consumes write endurance. It is unnecessary for normal sanitization "+
					"and is not stronger than a device-level sanitize.")
		}
	}

	// 5. BUDGET.
	opts, err := req.Options.normalized(profile)
	if err != nil {
		return nil, refuse(RefuseInvalidOptions, "%v", err)
	}
	st, err := sanitizeStatfs(root)
	if err != nil {
		return nil, refuse(RefuseDiscoveryIncomplete, "free space on %s could not be established: %v", root, err)
	}
	reserve := sanitizeReserveBytes(st.TotalBytes, opts.ReserveBytes)
	target := sanitizeTargetBytes(st.AvailBytes, reserve)

	seq, err := sanitizePassSequence(req.Method, passes)
	if req.Method != SanitizeTrim {
		if err != nil {
			return nil, refuse(RefuseInvalidOptions, "%v", err)
		}
		if target == 0 {
			return nil, refuse(RefuseNoSpaceAboveReserve,
				"%s has %s available and the safety reserve is %s; there is no free space to overwrite "+
					"without risking filling the filesystem", root, humanBytes(st.AvailBytes), humanBytes(reserve))
		}
	} else {
		seq = nil
	}

	plan := &SanitizePlan{
		JobID:              newSanitizeJobID(),
		Method:             req.Method,
		Root:               root,
		MountPoint:         mnt.Point,
		Filesystem:         fs,
		FilesystemRaw:      mnt.FSType,
		Profile:            profile,
		Device:             dev.Identity,
		DeviceName:         dev.Name,
		Fingerprint:        dev.Identity.Fingerprint(),
		LockFingerprint:    lockFingerprint(inv, dev),
		Rotational:         dev.Rotational,
		SupportsDiscard:    dev.SupportsDiscard,
		Passes:             seq,
		FilesystemBytes:    st.TotalBytes,
		AvailableBytes:     st.AvailBytes,
		ReserveBytes:       reserve,
		TargetBytesPerPass: target,
		TotalWriteBytes:    target * uint64(len(seq)),
		MaxPartBytes:       sanitizePartSizeFor(profile, opts.MaxPartBytes),
		Semantics:          sanitizeSemanticsFor(req.Method),
		Warnings:           warnings,
		PrincipalID:        req.PrincipalID,
		Options:            opts,
		PreparedAt:         time.Now(),
	}
	if st.TotalBytes > 0 {
		plan.CapacityWrites = float64(plan.TotalWriteBytes) / float64(st.TotalBytes)
	}
	if len(seq) > 1 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"%d passes over %s of free space: approximately %s will be written to %s.",
			len(seq), humanBytes(target), humanBytes(plan.TotalWriteBytes), dev.Name))
	}
	// Seal it: the caller receives the plan to display and a job id to execute
	// with, and cannot execute the struct it is holding.
	sealPlan(plan)
	return plan, nil
}

// RecommendSanitizeMethod says what SHOULD be used for a device, and why.
//
// It never recommends an overwrite for solid-state media. That choice is left
// entirely to a human who has been shown the endurance cost.
func RecommendSanitizeMethod(dev blockdev.Device, caps Capabilities) (SanitizeMethod, string) {
	if dev.Rotational {
		return SanitizeZeroFill, "Rotational media: a single zero-fill pass over free space is the " +
			"appropriate operation. Multi-pass is available if a policy requires it."
	}
	if dev.SupportsDiscard && toolAvailable(caps, ToolFstrim) {
		return SanitizeTrim, "Solid-state media: discard (TRIM) is the appropriate operation. " +
			"It writes nothing and costs no endurance. It is not a secure erase."
	}
	return "", "Solid-state media without discard support. Overwriting free space would cost write " +
		"endurance without reaching retired pages, so nothing is recommended automatically; a " +
		"device-level sanitize performed by the drive is the operation with the stronger semantics."
}

func toolAvailable(c Capabilities, t Tool) bool {
	for _, s := range c.Tools {
		if s.Tool == t {
			return s.Available
		}
	}
	return false
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------- revalidation

// revalidateSanitizePlan re-derives every fact the plan rests on and compares.
//
// It runs immediately before the first destructive step AND before every
// subsequent pass. The window between "the user confirmed" and "the write
// happens" is where a disk gets unplugged, a device node gets reused, a
// filesystem gets remounted read-only, or an operator revokes a storage root.
// Anything that changed is an abort, never a warning.
func revalidateSanitizePlan(plan *SanitizePlan) error {
	d := storageauth.AuthorizeSystem(sanitizeMutateOps, plan.Root)
	if !d.Allowed {
		return refuse(RefusePlanChanged,
			"%s is no longer authorized storage (%s); aborting", plan.Root, d.Reason)
	}
	if d.Path != plan.Root {
		return refuse(RefusePlanChanged, "%s now canonicalizes to %s; aborting", plan.Root, d.Path)
	}

	mounts, err := readSanitizeMounts()
	if err != nil {
		return refuse(RefusePlanChanged, "the mount table became unreadable (%v); aborting", err)
	}
	mnt, ok := deepestSanitizeMount(mounts, plan.Root)
	if !ok || mnt.Point != plan.Root {
		return refuse(RefusePlanChanged, "%s is no longer a mount point; aborting", plan.Root)
	}
	if mnt.DeviceName != plan.DeviceName {
		return refuse(RefusePlanChanged,
			"%s is now served by %s instead of %s; aborting", plan.Root, mnt.DeviceName, plan.DeviceName)
	}
	if !strings.EqualFold(mnt.FSType, plan.FilesystemRaw) {
		return refuse(RefusePlanChanged,
			"%s is now %s instead of %s; aborting", plan.Root, mnt.FSType, plan.FilesystemRaw)
	}
	if mnt.ReadOnly {
		return refuse(RefusePlanChanged, "%s has been remounted read-only; aborting", plan.Root)
	}

	inv, err := blockdev.Discover()
	if err != nil {
		return refuse(RefusePlanChanged, "block device discovery failed during the operation (%v); aborting", err)
	}
	dev, found := inv.ByName(plan.DeviceName)
	if !found {
		return refuse(RefusePlanChanged, "%s has disappeared from the inventory; aborting", plan.DeviceName)
	}
	// IDENTITY, not name. A replug that reused the node is exactly the case
	// this comparison exists for, and Equal refuses to match two weak
	// identities so an unidentifiable replacement can never pass.
	if !dev.Identity.Equal(plan.Device) {
		return refuse(RefusePlanChanged,
			"the device at %s is no longer the one this operation was prepared for "+
				"(prepared: %s, now: %s); aborting", plan.DeviceName,
			plan.Device.Fingerprint(), dev.Identity.Fingerprint())
	}
	if dev.Protected {
		return &SanitizeRefusal{Code: RefusePlanChanged,
			Detail:        fmt.Sprintf("%s has become protected during the operation; aborting", dev.Name),
			DeviceReasons: dev.ProtectionReasons}
	}
	if dev.ReadOnly {
		return refuse(RefusePlanChanged, "%s has become read-only; aborting", dev.Name)
	}
	if dev.Rotational != plan.Rotational {
		return refuse(RefusePlanChanged, "%s now reports different media characteristics; aborting", dev.Name)
	}
	return nil
}

// ---------------------------------------------------------------- progress

// SanitizePhase is the coarse state a UI renders.
type SanitizePhase string

const (
	PhasePreparing   SanitizePhase = "preparing"
	PhaseWriting     SanitizePhase = "writing"
	PhaseDiscardSent SanitizePhase = "discard_submitted"
	PhaseCleaningUp  SanitizePhase = "cleaning_up"
	PhaseVerifying   SanitizePhase = "verifying"
	PhaseDone        SanitizePhase = "completed"
)

// SanitizeProgress is one snapshot. Every number a UI needs is here, and the
// ones that cannot honestly be produced are flagged rather than faked.
type SanitizeProgress struct {
	JobID  string         `json:"job_id"`
	Method SanitizeMethod `json:"method"`
	Phase  SanitizePhase  `json:"phase"`

	FilesystemBytes  uint64 `json:"filesystem_bytes"`
	FreeBytesAtStart uint64 `json:"free_bytes_at_start"`
	FreeBytesNow     uint64 `json:"free_bytes_now"`
	ReserveBytes     uint64 `json:"reserve_bytes"`

	TargetBytesPerPass   uint64 `json:"target_bytes_per_pass"`
	BytesWrittenThisPass uint64 `json:"bytes_written_this_pass"`
	BytesWrittenTotal    uint64 `json:"bytes_written_total"`
	BytesDiscarded       uint64 `json:"bytes_discarded"`
	PartsCreated         int    `json:"parts_created"`

	PassesPlanned    int              `json:"passes_planned"`
	PassesCompleted  int              `json:"passes_completed"`
	CurrentPass      int              `json:"current_pass"`
	CurrentPassSpec  SanitizePassSpec `json:"current_pass_spec"`
	CurrentPassLabel string           `json:"current_pass_label,omitempty"`

	ThroughputBytesPerSec float64       `json:"throughput_bytes_per_sec"`
	Elapsed               time.Duration `json:"elapsed_ns"`

	// EstimatedRemainingBytes and EstimatedRemaining are meaningful only when
	// Determinate is true.
	EstimatedRemainingBytes uint64        `json:"estimated_remaining_bytes"`
	EstimatedRemaining      time.Duration `json:"estimated_remaining_ns"`
	// Determinate is false for TRIM. Neither the kernel nor the device reports
	// how far a discard has progressed, so no ETA is offered rather than a
	// fabricated one.
	Determinate bool `json:"determinate"`
	// Note carries the indeterminate explanation for the UI.
	Note string `json:"note,omitempty"`
}

// ---------------------------------------------------------------- artifacts

const (
	sanitizeArtifactPrefix  = ".casanova-wipe-"
	sanitizeManifestName    = "manifest.json"
	sanitizeArtifactMagic   = "casanova.storagemaint.free-space-sanitize"
	sanitizeManifestVersion = 1
	sanitizeMaxManifestSize = 64 << 10
)

// sanitizeJobIDRe is what a job id may look like. Cleanup never accepts a name
// that does not match: a permissive pattern is how a directory belonging to
// someone else ends up deleted.
var sanitizeJobIDRe = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

func sanitizeArtifactDirName(jobID string) string { return sanitizeArtifactPrefix + jobID }

func sanitizePartName(jobID string, n int) string {
	return fmt.Sprintf("%s%s-%06d", sanitizeArtifactPrefix, jobID, n)
}

var sanitizePartRe = regexp.MustCompile(`^\.casanova-wipe-([0-9a-f]{16,64})-([0-9]{6,})$`)

func newSanitizeJobID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		// A job id that is not unique could make recovery clean the wrong job's
		// artifacts, so a failure here is fatal to the operation rather than
		// papered over with a timestamp.
		return ""
	}
	return hex.EncodeToString(b[:])
}

// sanitizeManifest is the proof of provenance. It is written and fsynced BEFORE
// any large file exists, so a crash one millisecond later still leaves behind
// something that can be identified and cleaned automatically.
type sanitizeManifest struct {
	Magic     string         `json:"magic"`
	Version   int            `json:"version"`
	JobID     string         `json:"job_id"`
	Root      string         `json:"root"`
	Method    SanitizeMethod `json:"method"`
	CreatedAt time.Time      `json:"created_at"`
	WriterPID int            `json:"writer_pid"`
	WriterUID int            `json:"writer_uid"`
	BootID    string         `json:"boot_id,omitempty"`
	Passes    int            `json:"passes"`
}

// errUnprovenArtifact is returned when a directory looks like ours but cannot
// be proven to be. It is never followed by a delete.
var errUnprovenArtifact = errors.New("artifact provenance could not be proven")

// openSanitizeRoot opens the target filesystem root through the authority, with
// the kernel confining resolution. Everything else happens relative to the
// returned descriptor, so no later step re-resolves a string.
func openSanitizeRoot(root string, op storageauth.Op) (*os.File, error) {
	f, _, err := storageauth.OpenSystemStrict(op, root, os.O_RDONLY|unix.O_DIRECTORY, 0)
	return f, err
}

func openBeneath(dirFD int, name string, flags int, mode uint32) (int, error) {
	how := &unix.OpenHow{
		Flags:   uint64(flags) | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Mode:    uint64(mode),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	return unix.Openat2(dirFD, name, how)
}

// createSanitizeArtifacts makes the job directory and writes the manifest
// durably. Order matters: manifest first, big files after.
func createSanitizeArtifacts(plan *SanitizePlan) error {
	if _, err := storageauth.MkdirAllAuthorized(systemPrincipal, plan.ArtifactDir(), 0o700); err != nil {
		return fmt.Errorf("create sanitization workspace: %w", err)
	}
	rootF, err := openSanitizeRoot(plan.Root, sanitizeMutateOps)
	if err != nil {
		return err
	}
	defer rootF.Close()

	dirFD, err := openBeneath(int(rootF.Fd()), sanitizeArtifactDirName(plan.JobID),
		unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open sanitization workspace: %w", err)
	}
	defer unix.Close(dirFD)

	m := sanitizeManifest{
		Magic: sanitizeArtifactMagic, Version: sanitizeManifestVersion,
		JobID: plan.JobID, Root: plan.Root, Method: plan.Method,
		CreatedAt: time.Now(), WriterPID: os.Getpid(), WriterUID: os.Geteuid(),
		BootID: currentBootID(), Passes: len(plan.Passes),
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	fd, err := openBeneath(dirFD, sanitizeManifestName, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create sanitization manifest: %w", err)
	}
	f := os.NewFile(uintptr(fd), sanitizeManifestName)
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	// fsync the directory too: without it the manifest's NAME may not survive
	// the crash the manifest exists to be found after.
	return unix.Fsync(dirFD)
}

// proveSanitizeArtifact establishes that a directory found on a user's
// filesystem was created by this subsystem.
//
// This is the guard that stops "clean up stray wipe files" from becoming "delete
// anything whose name starts with a dot and looks temporary". Nothing is
// removed unless ALL of the following hold: the directory name carries a
// well-formed job id, it contains a manifest, the manifest carries this
// subsystem's magic and a supported version, and the manifest's own job id and
// root agree with where the directory was actually found.
func proveSanitizeArtifact(rootFD int, root, dirName string) (sanitizeManifest, int, error) {
	if !strings.HasPrefix(dirName, sanitizeArtifactPrefix) {
		return sanitizeManifest{}, -1, fmt.Errorf("%w: %q does not carry the CasaNova artifact prefix",
			errUnprovenArtifact, dirName)
	}
	jobID := strings.TrimPrefix(dirName, sanitizeArtifactPrefix)
	if !sanitizeJobIDRe.MatchString(jobID) {
		return sanitizeManifest{}, -1, fmt.Errorf("%w: %q does not carry a well-formed job id",
			errUnprovenArtifact, dirName)
	}

	dirFD, err := openBeneath(rootFD, dirName, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return sanitizeManifest{}, -1, fmt.Errorf("%w: %q could not be opened as a directory beneath %s (%v)",
			errUnprovenArtifact, dirName, root, err)
	}

	mfd, err := openBeneath(dirFD, sanitizeManifestName, unix.O_RDONLY, 0)
	if err != nil {
		unix.Close(dirFD)
		return sanitizeManifest{}, -1, fmt.Errorf("%w: %q has no readable manifest (%v)",
			errUnprovenArtifact, dirName, err)
	}
	mf := os.NewFile(uintptr(mfd), sanitizeManifestName)
	body, rerr := io.ReadAll(io.LimitReader(mf, sanitizeMaxManifestSize))
	mf.Close()
	if rerr != nil {
		unix.Close(dirFD)
		return sanitizeManifest{}, -1, fmt.Errorf("%w: manifest of %q is unreadable (%v)",
			errUnprovenArtifact, dirName, rerr)
	}

	var m sanitizeManifest
	if err := json.Unmarshal(body, &m); err != nil {
		unix.Close(dirFD)
		return sanitizeManifest{}, -1, fmt.Errorf("%w: manifest of %q does not parse (%v)",
			errUnprovenArtifact, dirName, err)
	}
	switch {
	case m.Magic != sanitizeArtifactMagic:
		err = fmt.Errorf("%w: manifest of %q carries magic %q, not this subsystem's", errUnprovenArtifact, dirName, m.Magic)
	case m.Version != sanitizeManifestVersion:
		err = fmt.Errorf("%w: manifest of %q is version %d, which this build does not understand",
			errUnprovenArtifact, dirName, m.Version)
	case m.JobID != jobID:
		err = fmt.Errorf("%w: manifest of %q claims job %q", errUnprovenArtifact, dirName, m.JobID)
	case filepath.Clean(m.Root) != filepath.Clean(root):
		err = fmt.Errorf("%w: manifest of %q claims root %q but it was found under %q",
			errUnprovenArtifact, dirName, m.Root, root)
	}
	if err != nil {
		unix.Close(dirFD)
		return sanitizeManifest{}, -1, err
	}
	return m, dirFD, nil
}

// sanitizeCleanupResult reports what a cleanup did.
type sanitizeCleanupResult struct {
	Removed     int
	BytesFreed  uint64
	DirRemoved  bool
	Unexplained []string
}

// cleanupSanitizeArtifacts removes one PROVEN artifact directory.
//
// Every entry inside is checked as well: the manifest, or a part file whose
// name encodes this exact job id, and it must be a regular file. Anything else
// is left alone and reported. A cleanup routine that removes whatever it finds
// inside a directory it half-recognizes is a data-loss bug waiting for a name
// collision.
func cleanupSanitizeArtifacts(root, dirName string) (sanitizeCleanupResult, error) {
	res := sanitizeCleanupResult{}

	rootF, err := openSanitizeRoot(root, sanitizeMutateOps)
	if err != nil {
		return res, err
	}
	defer rootF.Close()
	rootFD := int(rootF.Fd())

	// A workspace that is already gone is a finished cleanup, not a mystery.
	// Reporting it as unexplained would mark the subsystem degraded after every
	// successful run.
	var dst unix.Stat_t
	if err := unix.Fstatat(rootFD, dirName, &dst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			res.DirRemoved = true
			return res, nil
		}
		return res, err
	}

	m, dirFD, err := proveSanitizeArtifact(rootFD, root, dirName)
	if err != nil {
		if errors.Is(err, errUnprovenArtifact) {
			res.Unexplained = append(res.Unexplained, filepath.Join(root, dirName))
		}
		return res, err
	}
	defer unix.Close(dirFD)

	// Readdirnames needs an *os.File that owns its descriptor; dup so closing it
	// does not close the descriptor the unlinks below are relative to.
	ddup, derr := unix.Dup(dirFD)
	if derr != nil {
		return res, fmt.Errorf("duplicate workspace descriptor: %w", derr)
	}
	dir := os.NewFile(uintptr(ddup), dirName)
	names, rerr := dir.Readdirnames(-1)
	dir.Close()
	if rerr != nil && rerr != io.EOF {
		return res, rerr
	}
	sort.Strings(names)

	leftovers := 0
	for _, name := range names {
		switch {
		case name == sanitizeManifestName:
		case sanitizePartRe.MatchString(name):
			if sub := sanitizePartRe.FindStringSubmatch(name); sub[1] != m.JobID {
				// A part naming a DIFFERENT job inside this job's directory is
				// not something this code creates. Refuse it.
				res.Unexplained = append(res.Unexplained, filepath.Join(root, dirName, name))
				leftovers++
				continue
			}
		default:
			res.Unexplained = append(res.Unexplained, filepath.Join(root, dirName, name))
			leftovers++
			continue
		}

		var st unix.Stat_t
		if err := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			res.Unexplained = append(res.Unexplained, filepath.Join(root, dirName, name))
			leftovers++
			continue
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			// Only regular files are ever created here. A symlink or a device
			// node under this name was put there by something else.
			res.Unexplained = append(res.Unexplained, filepath.Join(root, dirName, name))
			leftovers++
			continue
		}
		if err := unix.Unlinkat(dirFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return res, fmt.Errorf("remove %s: %w", filepath.Join(root, dirName, name), err)
		}
		res.Removed++
		res.BytesFreed += uint64(st.Size)
	}

	if leftovers == 0 {
		if err := unix.Unlinkat(rootFD, dirName, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
			return res, fmt.Errorf("remove %s: %w", filepath.Join(root, dirName), err)
		}
		res.DirRemoved = true
	}
	// The directory entries only become durable on some filesystems after a
	// directory fsync; without it a crash right here can resurrect the files.
	_ = unix.Fsync(rootFD)
	return res, nil
}

func currentBootID() string {
	b, err := os.ReadFile(filepath.Join(sanitizeProcRoot(), "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ---------------------------------------------------------------- job records

// SanitizeJobState is the durable lifecycle of a job.
type SanitizeJobState string

const (
	JobRunning     SanitizeJobState = "running"
	JobCompleted   SanitizeJobState = "completed"
	JobCancelled   SanitizeJobState = "cancelled"
	JobFailed      SanitizeJobState = "failed"
	JobInterrupted SanitizeJobState = "interrupted"
)

// SanitizeJobRecord is persisted before any large write, so a crash is
// diagnosable instead of mysterious.
type SanitizeJobRecord struct {
	JobID  string           `json:"job_id"`
	State  SanitizeJobState `json:"state"`
	Method SanitizeMethod   `json:"method"`
	Root   string           `json:"root"`

	Filesystem        Filesystem `json:"filesystem"`
	DeviceName        string     `json:"device_name"`
	DeviceFingerprint string     `json:"device_fingerprint"`
	DeviceModel       string     `json:"device_model,omitempty"`
	DeviceSerial      string     `json:"device_serial,omitempty"`

	ArtifactDir string `json:"artifact_dir"`
	BootID      string `json:"boot_id,omitempty"`
	PID         int    `json:"pid"`

	PassesPlanned   int    `json:"passes_planned"`
	PassesCompleted int    `json:"passes_completed"`
	BytesWritten    uint64 `json:"bytes_written"`
	BytesDiscarded  uint64 `json:"bytes_discarded"`

	FreeBytesAtStart uint64 `json:"free_bytes_at_start"`
	FreeBytesAtEnd   uint64 `json:"free_bytes_at_end,omitempty"`
	ReserveBytes     uint64 `json:"reserve_bytes"`

	StartedAt   time.Time  `json:"started_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	PrincipalID string     `json:"principal_id,omitempty"`
	Detail      string     `json:"detail,omitempty"`
}

func sanitizeJobsDir() string { return filepath.Join(StateDir(), "sanitize-jobs") }

var sanitizeJobMu sync.Mutex

// writeSanitizeJobRecord persists a record atomically and durably.
func writeSanitizeJobRecord(rec SanitizeJobRecord) error {
	sanitizeJobMu.Lock()
	defer sanitizeJobMu.Unlock()

	dir := sanitizeJobsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	rec.UpdatedAt = time.Now()
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	final := filepath.Join(dir, rec.JobID+".json")
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	// Durability of the RENAME, not just of the bytes: the record must exist
	// under its final name after a power cut, or recovery has nothing to find.
	//
	// The error is PROPAGATED, through the same seam and classifier the
	// formatter uses. Discarding it was the sanitizer's copy of a defect just
	// fixed in format.go: the comment above promises the record survives a
	// crash, and swallowing the directory fsync means it may not — a claim
	// stronger than the code, in the one place recovery depends on.
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

// ReadSanitizeJobs returns every persisted job record, newest first.
func ReadSanitizeJobs() ([]SanitizeJobRecord, error) {
	entries, err := os.ReadDir(sanitizeJobsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SanitizeJobRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(sanitizeJobsDir(), e.Name()))
		if rerr != nil {
			continue
		}
		var rec SanitizeJobRecord
		if json.Unmarshal(body, &rec) == nil && rec.JobID != "" {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// liveSanitizeJobs tracks the jobs running in THIS process, so recovery can
// tell "interrupted by a crash" from "running right now".
var (
	liveJobsMu sync.Mutex
	liveJobs   = map[string]bool{}
)

func markJobLive(id string, live bool) {
	liveJobsMu.Lock()
	defer liveJobsMu.Unlock()
	if live {
		liveJobs[id] = true
	} else {
		delete(liveJobs, id)
	}
}

func jobIsLive(id string) bool {
	liveJobsMu.Lock()
	defer liveJobsMu.Unlock()
	return liveJobs[id]
}

// ---------------------------------------------------------------- the run

// SanitizeOutcome is how a run ended.
type SanitizeOutcome string

const (
	OutcomeCompleted SanitizeOutcome = "completed"
	OutcomeCancelled SanitizeOutcome = "cancelled"
	OutcomeAborted   SanitizeOutcome = "aborted"
	OutcomeFailed    SanitizeOutcome = "failed"
)

// SanitizeResult is the full report of a finished run.
type SanitizeResult struct {
	JobID   string          `json:"job_id"`
	Method  SanitizeMethod  `json:"method"`
	Root    string          `json:"root"`
	Outcome SanitizeOutcome `json:"outcome"`

	Final SanitizeProgress `json:"final_progress"`

	FreeBytesAtStart uint64 `json:"free_bytes_at_start"`
	FreeBytesAtEnd   uint64 `json:"free_bytes_at_end"`
	ReserveBytes     uint64 `json:"reserve_bytes"`

	// ArtifactsRemoved and SpaceReclaimed are PROPERTY checks, not existence
	// checks: the run verifies that the workspace is gone AND that the free
	// space actually came back.
	ArtifactsRemoved bool `json:"artifacts_removed"`
	SpaceReclaimed   bool `json:"space_reclaimed"`
	// StoppedAtReserve records that the writer hit its own safety limit, which
	// is the normal way a zero-fill pass ends.
	StoppedAtReserve bool `json:"stopped_at_reserve"`
	// HitENOSPC records that the filesystem reported it was full. Expected, not
	// an error, wherever the algorithm relies on it.
	HitENOSPC bool `json:"hit_enospc"`

	Semantics   SanitizeSemantics `json:"semantics"`
	Warnings    []string          `json:"warnings,omitempty"`
	Unexplained []string          `json:"unexplained_artifacts,omitempty"`

	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Duration   time.Duration `json:"duration_ns"`
	Detail     string        `json:"detail,omitempty"`
}

// sanitizeBufferBytes counts every byte of write buffer this package has
// allocated. It exists so a test can PROVE the writer streams instead of
// buffering free space in memory: the counter must stay within a small multiple
// of the chunk size no matter how many gigabytes are written.
var sanitizeBufferBytes atomic.Uint64

// newSanitizeBuffer is the ONLY place this package allocates a write buffer,
// and it refuses anything above the chunk ceiling.
//
// Funnelling every allocation through one bounded helper is deliberate: "just
// build the payload in memory and write it once" is a plausible-looking
// optimisation that would make a sanitization run allocate as much RAM as the
// disk has free space. Here it cannot be done by changing one line — the
// ceiling is enforced at the allocation itself, not merely validated in
// options parsing where a later caller could bypass it.
func newSanitizeBuffer(n uint64) ([]byte, error) {
	if n < minSanitizeChunkBytes || n > maxSanitizeChunkBytes {
		return nil, fmt.Errorf("refusing to allocate a %s write buffer: the sanitization writer "+
			"streams in chunks of %s to %s and never buffers free space in memory",
			humanBytes(n), humanBytes(minSanitizeChunkBytes), humanBytes(maxSanitizeChunkBytes))
	}
	sanitizeBufferBytes.Add(n)
	return make([]byte, n), nil
}

// sanitizeWriteChunk is indirected so the test lab can inject ENOSPC without a
// full filesystem. Production writes.
var sanitizeWriteChunk = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

// ---------------------------------------------------------------- plan sealing

// A prepared plan is an AUTHORIZATION, not a suggestion, so the engine keeps
// it rather than handing it to the caller.
//
// RunSanitize used to accept any *SanitizePlan a caller passed in: no
// single-use consumption, no expiry, no seal, and every field exported and
// mutable. The runtime safety bound read w.plan.ReserveBytes, so a caller could
// hand over a plan with ReserveBytes: 0 and the writer would fill the
// filesystem to capacity — the one outcome D5 exists to prevent. It also meant
// a plan prepared for one root could be replayed against another.
//
// Now Prepare seals the plan here and returns a job id. Run consumes that id
// once, and the safety bound is RE-DERIVED from the live filesystem instead of
// read back from the document.
const sanitizePlanTTL = 10 * time.Minute

var (
	sealedMu    sync.Mutex
	sealedPlans = map[string]*SanitizePlan{}
)

// sealPlan stores a prepared plan and returns its job id.
func sealPlan(plan *SanitizePlan) {
	sealedMu.Lock()
	defer sealedMu.Unlock()
	// Drop anything that lapsed, so an abandoned prepare cannot accumulate.
	for id, p := range sealedPlans {
		if time.Since(p.PreparedAt) > sanitizePlanTTL {
			delete(sealedPlans, id)
		}
	}
	sealedPlans[plan.JobID] = plan
}

// consumePlan burns a job id and returns its plan. Single use: the entry is
// removed under the same lock that reads it, so two concurrent executions
// cannot both win.
func consumePlan(jobID string) (*SanitizePlan, error) {
	sealedMu.Lock()
	defer sealedMu.Unlock()

	plan, ok := sealedPlans[jobID]
	if !ok {
		return nil, refuse(RefuseInvalidOptions,
			"no prepared sanitization with id %q; prepare one first, and note that a plan is "+
				"single-use and expires after %s", jobID, sanitizePlanTTL)
	}
	delete(sealedPlans, jobID)

	if time.Since(plan.PreparedAt) > sanitizePlanTTL {
		return nil, refuse(RefuseInvalidOptions,
			"the prepared sanitization expired after %s; the filesystem may have changed since, "+
				"so prepare it again", sanitizePlanTTL)
	}
	return plan, nil
}

// PendingSanitizePlans reports how many prepared plans are waiting, for health.
func PendingSanitizePlans() int {
	sealedMu.Lock()
	defer sealedMu.Unlock()
	return len(sealedPlans)
}

// RunSanitize executes a prepared plan.
//
// The plan is revalidated immediately before the first destructive step and
// before every subsequent pass. Cancellation, abort and failure all take the
// same cleanup path, because "we stopped early" must never mean "the disk stays
// full".
func RunSanitize(ctx context.Context, jobID string) (*SanitizeResult, error) {
	plan, err := consumePlan(jobID)
	if err != nil {
		return nil, err
	}

	// RE-DERIVE the safety bound from the live filesystem rather than trusting
	// the document. The reserve is what stops this filling the disk, and a
	// value carried in a caller-held struct is not a safety bound — it is a
	// suggestion the caller can set to zero.
	if st, sErr := sanitizeStatfs(plan.Root); sErr == nil {
		live := sanitizeReserveBytes(st.TotalBytes, plan.Options.ReserveBytes)
		if live > plan.ReserveBytes {
			plan.ReserveBytes = live
		}
		plan.AvailableBytes = st.AvailBytes
		if tgt := sanitizeTargetBytes(st.AvailBytes, plan.ReserveBytes); tgt < plan.TargetBytesPerPass {
			plan.TargetBytesPerPass = tgt
		}
	} else {
		return nil, refuse(RefuseInvalidOptions,
			"cannot re-measure %s before writing to it: %v", plan.Root, sErr)
	}
	lockKey := plan.lockKey()
	if err := AcquireDevice(lockKey, plan.JobID); err != nil {
		return nil, &SanitizeRefusal{Code: RefuseDeviceBusy, Detail: err.Error()}
	}
	defer ReleaseDevice(lockKey, plan.JobID)

	started := time.Now()
	res := &SanitizeResult{
		JobID: plan.JobID, Method: plan.Method, Root: plan.Root,
		FreeBytesAtStart: plan.AvailableBytes, ReserveBytes: plan.ReserveBytes,
		Semantics: plan.Semantics, Warnings: plan.Warnings, StartedAt: started,
	}

	rec := SanitizeJobRecord{
		JobID: plan.JobID, State: JobRunning, Method: plan.Method, Root: plan.Root,
		Filesystem: plan.Filesystem, DeviceName: plan.DeviceName,
		DeviceFingerprint: plan.Fingerprint, DeviceModel: plan.Device.Model,
		DeviceSerial: plan.Device.Serial, ArtifactDir: plan.ArtifactDir(),
		BootID: currentBootID(), PID: os.Getpid(),
		PassesPlanned: len(plan.Passes), FreeBytesAtStart: plan.AvailableBytes,
		ReserveBytes: plan.ReserveBytes, StartedAt: started, PrincipalID: plan.PrincipalID,
	}
	// Persisted BEFORE anything large exists. A crash between here and the
	// first byte still leaves a record recovery can act on.
	if err := writeSanitizeJobRecord(rec); err != nil {
		return nil, fmt.Errorf("persist sanitization job state: %w", err)
	}
	markJobLive(plan.JobID, true)
	defer markJobLive(plan.JobID, false)

	// RULE 4: revalidate immediately before the first destructive step.
	if err := revalidateSanitizePlan(plan); err != nil {
		finishSanitizeRecord(&rec, JobFailed, err.Error())
		auditSanitize(plan, "refused", "revalidate", err.Error())
		res.Outcome = OutcomeAborted
		res.Detail = err.Error()
		res.FinishedAt = time.Now()
		res.Duration = time.Since(started)
		return res, err
	}

	var runErr error
	if plan.Method == SanitizeTrim {
		runErr = runSanitizeTrim(ctx, plan, res)
	} else {
		runErr = runSanitizeOverwrite(ctx, plan, res, &rec)
	}

	// Cleanup and verification happen on EVERY path, including failure.
	if plan.Method != SanitizeTrim {
		cl, cerr := cleanupSanitizeArtifacts(plan.Root, sanitizeArtifactDirName(plan.JobID))
		res.Unexplained = append(res.Unexplained, cl.Unexplained...)
		res.ArtifactsRemoved = cl.DirRemoved
		if cerr != nil && !errors.Is(cerr, errUnprovenArtifact) && runErr == nil {
			runErr = cerr
		}
		verifySanitizeReclaim(plan, res)
	}

	res.FinishedAt = time.Now()
	res.Duration = res.FinishedAt.Sub(started)

	switch {
	case runErr == nil:
		res.Outcome = OutcomeCompleted
		finishSanitizeRecord(&rec, JobCompleted, "")
		auditSanitize(plan, "completed", "run", sanitizeResultDetail(res))
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		res.Outcome = OutcomeCancelled
		res.Detail = runErr.Error()
		finishSanitizeRecord(&rec, JobCancelled, runErr.Error())
		auditSanitize(plan, "cancelled", "run", runErr.Error())
	default:
		var r *SanitizeRefusal
		if errors.As(runErr, &r) {
			res.Outcome = OutcomeAborted
		} else {
			res.Outcome = OutcomeFailed
		}
		res.Detail = runErr.Error()
		finishSanitizeRecord(&rec, JobFailed, runErr.Error())
		auditSanitize(plan, string(res.Outcome), "run", runErr.Error())
	}
	rec.BytesWritten = res.Final.BytesWrittenTotal
	rec.BytesDiscarded = res.Final.BytesDiscarded
	rec.PassesCompleted = res.Final.PassesCompleted
	rec.FreeBytesAtEnd = res.FreeBytesAtEnd
	_ = writeSanitizeJobRecord(rec)

	// The error is returned even for a cancellation: a caller that ignores the
	// error must not be able to read "no error" as "the free space was
	// sanitized". The outcome field says which kind of stop it was.
	return res, runErr
}

func sanitizeResultDetail(res *SanitizeResult) string {
	return fmt.Sprintf("wrote %s across %d pass(es), discarded %s, free space %s -> %s",
		humanBytes(res.Final.BytesWrittenTotal), res.Final.PassesCompleted,
		humanBytes(res.Final.BytesDiscarded), humanBytes(res.FreeBytesAtStart), humanBytes(res.FreeBytesAtEnd))
}

func finishSanitizeRecord(rec *SanitizeJobRecord, state SanitizeJobState, detail string) {
	now := time.Now()
	rec.State = state
	rec.FinishedAt = &now
	rec.Detail = detail
}

func auditSanitize(plan *SanitizePlan, result, stage, detail string) {
	_ = Audit(AuditEntry{
		PrincipalID: plan.PrincipalID,
		Operation:   "sanitize_free_space",
		Fingerprint: plan.Fingerprint,
		Model:       plan.Device.Model,
		Serial:      plan.Device.Serial,
		SizeBytes:   plan.FilesystemBytes,
		Requested:   fmt.Sprintf("%s root=%s fs=%s passes=%d", plan.Method, plan.Root, plan.Filesystem, len(plan.Passes)),
		Result:      result, Stage: stage, Detail: detail,
	})
}

// verifySanitizeReclaim checks the PROPERTY that matters after cleanup: the
// free space came back. Checking that the directory is gone is not enough —
// an unlinked file held open by a leaked descriptor keeps its blocks.
func verifySanitizeReclaim(plan *SanitizePlan, res *SanitizeResult) {
	st, err := sanitizeStatfs(plan.Root)
	if err != nil {
		res.Detail = firstNonEmptyStr(res.Detail, "free space after cleanup could not be verified: "+err.Error())
		return
	}
	res.FreeBytesAtEnd = st.AvailBytes
	res.Final.FreeBytesNow = st.AvailBytes
	// Tolerance, because a filesystem legitimately spends a little space on
	// metadata and journal during the run, and another writer may have used
	// some. The check is "the artifacts' space came back", not "the byte count
	// is identical".
	tolerance := plan.AvailableBytes / 100
	if tolerance < 16<<20 {
		tolerance = 16 << 20
	}
	res.SpaceReclaimed = st.AvailBytes+tolerance >= plan.AvailableBytes
}

// ---------------------------------------------------------------- overwriting

type sanitizeWriter struct {
	plan   *SanitizePlan
	res    *SanitizeResult
	rec    *SanitizeJobRecord
	opts   SanitizeOptions
	buf    []byte
	rng    *mrand.ChaCha8
	rootFD int

	startedAt    time.Time
	lastProgress time.Time
	sinceSync    uint64
	partIndex    int
}

func runSanitizeOverwrite(ctx context.Context, plan *SanitizePlan, res *SanitizeResult, rec *SanitizeJobRecord) error {
	if err := createSanitizeArtifacts(plan); err != nil {
		return err
	}

	w := &sanitizeWriter{
		plan: plan, res: res, rec: rec, opts: plan.Options,
		startedAt: time.Now(),
	}
	// ONE buffer for the WHOLE run, bounded by maxSanitizeChunkBytes. This is
	// the entire memory footprint of the writer regardless of whether the
	// filesystem has 8 GB or 8 TB of free space.
	buf, err := newSanitizeBuffer(plan.Options.ChunkBytes)
	if err != nil {
		return err
	}
	w.buf = buf

	res.Final = SanitizeProgress{
		JobID: plan.JobID, Method: plan.Method, Phase: PhasePreparing,
		FilesystemBytes: plan.FilesystemBytes, FreeBytesAtStart: plan.AvailableBytes,
		FreeBytesNow: plan.AvailableBytes, ReserveBytes: plan.ReserveBytes,
		TargetBytesPerPass: plan.TargetBytesPerPass, PassesPlanned: len(plan.Passes),
		Determinate: true,
	}
	w.emit()

	rootF, err := openSanitizeRoot(plan.Root, sanitizeMutateOps)
	if err != nil {
		return err
	}
	defer rootF.Close()
	w.rootFD = int(rootF.Fd())

	for i, spec := range plan.Passes {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// RULE 4 again: the disk may have been unplugged between passes.
		if err := revalidateSanitizePlan(plan); err != nil {
			return err
		}
		if spec.Kind == passRandom && w.rng == nil {
			var seed [32]byte
			if _, err := crand.Read(seed[:]); err != nil {
				return fmt.Errorf("seed the pattern generator: %w", err)
			}
			w.rng = mrand.NewChaCha8(seed)
		}
		res.Final.CurrentPass = i + 1
		res.Final.CurrentPassSpec = spec
		res.Final.CurrentPassLabel = spec.Label
		res.Final.BytesWrittenThisPass = 0
		res.Final.Phase = PhaseWriting

		if err := w.runPass(ctx, spec); err != nil {
			return err
		}
		res.Final.PassesCompleted = i + 1

		// Free the space between passes: the next pass has nothing to write
		// into otherwise, and leaving a filesystem full between passes is the
		// outage this design refuses to cause.
		res.Final.Phase = PhaseCleaningUp
		w.emit()
		cl, cerr := cleanupSanitizeArtifacts(plan.Root, sanitizeArtifactDirName(plan.JobID))
		res.Unexplained = append(res.Unexplained, cl.Unexplained...)
		if cerr != nil {
			return cerr
		}
		if i < len(plan.Passes)-1 {
			// Recreate the workspace for the next pass, manifest first again.
			if err := createSanitizeArtifacts(plan); err != nil {
				return err
			}
		}
		rec.PassesCompleted = i + 1
		rec.BytesWritten = res.Final.BytesWrittenTotal
		_ = writeSanitizeJobRecord(*rec)
	}

	res.Final.Phase = PhaseVerifying
	w.emit()
	return nil
}

func (w *sanitizeWriter) runPass(ctx context.Context, spec SanitizePassSpec) error {
	// Fill the buffer for the deterministic patterns once per pass. The random
	// pass refills per chunk from the stream.
	if spec.Kind != passRandom {
		for i := range w.buf {
			w.buf[i] = spec.Byte
		}
	}

	var written uint64
	for written < w.plan.TargetBytesPerPass {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// A new artifact per MaxPartBytes. On FAT32 this is what keeps every
		// file below the 4 GiB limit instead of hitting EFBIG mid-run.
		w.partIndex++
		name := sanitizePartName(w.plan.JobID, w.partIndex)
		n, stop, err := w.writePart(ctx, name, spec, w.plan.TargetBytesPerPass-written)
		written += n
		if err != nil {
			return err
		}
		if stop {
			break
		}
	}
	return nil
}

// writePart streams one artifact file. It returns the bytes written, whether
// the pass should stop, and an error.
func (w *sanitizeWriter) writePart(ctx context.Context, name string, spec SanitizePassSpec, budget uint64) (uint64, bool, error) {
	dirFD, err := openBeneath(w.rootFD, sanitizeArtifactDirName(w.plan.JobID), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return 0, true, fmt.Errorf("open sanitization workspace: %w", err)
	}
	defer unix.Close(dirFD)

	// O_EXCL: never reuse or truncate an existing file. If the name is taken,
	// something is wrong and the writer stops rather than overwriting an object
	// it did not create.
	fd, err := openBeneath(dirFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return 0, true, fmt.Errorf("create sanitization artifact: %w", err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	w.res.Final.PartsCreated++

	limit := w.plan.MaxPartBytes
	if budget < limit {
		limit = budget
	}

	var written uint64
	for written < limit {
		select {
		case <-ctx.Done():
			_ = f.Sync()
			return written, true, ctx.Err()
		default:
		}

		// THE SAFETY BOUND, checked every chunk. statfs is a cheap syscall and
		// this is the invariant the whole feature rests on: the writer stops
		// while there is still a reserve, it never discovers the limit by
		// filling the filesystem.
		st, serr := sanitizeStatfs(w.plan.Root)
		if serr != nil {
			return written, true, fmt.Errorf("free space could not be re-checked: %w", serr)
		}
		w.res.Final.FreeBytesNow = st.AvailBytes
		if st.AvailBytes <= w.plan.ReserveBytes {
			w.res.StoppedAtReserve = true
			_ = f.Sync()
			return written, true, nil
		}
		headroom := st.AvailBytes - w.plan.ReserveBytes

		chunk := uint64(len(w.buf))
		if remaining := limit - written; remaining < chunk {
			chunk = remaining
		}
		if headroom < chunk {
			chunk = headroom
		}
		if chunk == 0 {
			w.res.StoppedAtReserve = true
			_ = f.Sync()
			return written, true, nil
		}

		if spec.Kind == passRandom {
			if _, err := w.rng.Read(w.buf[:chunk]); err != nil {
				return written, true, fmt.Errorf("generate pattern: %w", err)
			}
		}

		n, werr := sanitizeWriteChunk(f, w.buf[:chunk])
		if n > 0 {
			written += uint64(n)
			w.res.Final.BytesWrittenThisPass += uint64(n)
			w.res.Final.BytesWrittenTotal += uint64(n)
			w.sinceSync += uint64(n)
		}
		if werr != nil {
			if isTerminalSpaceError(werr) {
				// EXPECTED TERMINAL CONDITION, not a catastrophe: the
				// filesystem is telling the algorithm it is finished. The
				// reserve should normally stop the writer first; this is the
				// backstop for the case where another writer consumed the
				// headroom concurrently.
				w.res.HitENOSPC = true
				_ = f.Sync()
				return written, true, nil
			}
			return written, true, fmt.Errorf("write sanitization artifact: %w", werr)
		}

		if w.sinceSync >= w.opts.SyncEveryBytes {
			// Bounded dirty page cache: without this the kernel accumulates
			// gigabytes of dirty pages and the appliance stalls when it finally
			// flushes them.
			if err := f.Sync(); err != nil {
				if isTerminalSpaceError(err) {
					w.res.HitENOSPC = true
					return written, true, nil
				}
				return written, true, err
			}
			w.sinceSync = 0
		}

		w.pace(ctx)
		w.emitThrottled()
	}

	if err := f.Sync(); err != nil {
		if isTerminalSpaceError(err) {
			w.res.HitENOSPC = true
			return written, true, nil
		}
		return written, true, err
	}
	return written, false, nil
}

// isTerminalSpaceError recognizes the filesystem saying "full". EFBIG is
// included because a per-file limit reached despite the cap is the same class
// of expected boundary, not a fault.
func isTerminalSpaceError(err error) bool {
	return errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) || errors.Is(err, unix.EFBIG)
}

// pace holds the configured throughput ceiling so a maintenance job does not
// take the responsiveness of the appliance with it.
func (w *sanitizeWriter) pace(ctx context.Context) {
	if w.opts.MaxBytesPerSecond == 0 {
		return
	}
	want := time.Duration(float64(w.res.Final.BytesWrittenTotal) / float64(w.opts.MaxBytesPerSecond) * float64(time.Second))
	elapsed := time.Since(w.startedAt)
	if want <= elapsed {
		return
	}
	t := time.NewTimer(want - elapsed)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (w *sanitizeWriter) emitThrottled() {
	if time.Since(w.lastProgress) < w.opts.ProgressInterval {
		return
	}
	w.emit()
}

func (w *sanitizeWriter) emit() {
	w.lastProgress = time.Now()
	p := w.res.Final
	p.Elapsed = time.Since(w.startedAt)
	if p.Elapsed > 0 {
		p.ThroughputBytesPerSec = float64(p.BytesWrittenTotal) / p.Elapsed.Seconds()
	}
	total := p.TargetBytesPerPass * uint64(p.PassesPlanned)
	if total > p.BytesWrittenTotal {
		p.EstimatedRemainingBytes = total - p.BytesWrittenTotal
		if p.ThroughputBytesPerSec > 1 {
			p.EstimatedRemaining = time.Duration(float64(p.EstimatedRemainingBytes) / p.ThroughputBytesPerSec * float64(time.Second))
		}
	}
	w.res.Final.ThroughputBytesPerSec = p.ThroughputBytesPerSec
	w.res.Final.Elapsed = p.Elapsed
	w.res.Final.EstimatedRemainingBytes = p.EstimatedRemainingBytes
	w.res.Final.EstimatedRemaining = p.EstimatedRemaining
	if w.opts.Progress != nil {
		w.opts.Progress(p)
	}
}

// ---------------------------------------------------------------- trim

var trimBytesRe = regexp.MustCompile(`\((\d+) bytes\)`)

// runSanitizeTrim issues discard for the filesystem's unused blocks.
//
// Progress is INDETERMINATE and says so. The kernel's FITRIM ioctl reports
// nothing until it returns, and the device reports nothing at all, so an ETA
// would be invented. "Submitted" then "completed" is the honest model.
func runSanitizeTrim(ctx context.Context, plan *SanitizePlan, res *SanitizeResult) error {
	caps := CurrentCapabilities(ctx)
	var fstrimPath string
	for _, s := range caps.Tools {
		if s.Tool == ToolFstrim && s.Available {
			fstrimPath = s.Path
		}
	}
	if fstrimPath == "" {
		return refuse(RefuseMethodUnavailable, "the fstrim helper is not available")
	}

	res.Final = SanitizeProgress{
		JobID: plan.JobID, Method: SanitizeTrim, Phase: PhaseDiscardSent,
		FilesystemBytes: plan.FilesystemBytes, FreeBytesAtStart: plan.AvailableBytes,
		FreeBytesNow: plan.AvailableBytes, ReserveBytes: plan.ReserveBytes,
		Determinate: false,
		Note: "Discard has been submitted. Neither the kernel nor the device reports how far it has " +
			"progressed, so no percentage or estimated time is shown. This operation writes nothing " +
			"and is not a secure erase.",
	}
	if plan.Options.Progress != nil {
		plan.Options.Progress(res.Final)
	}

	// RULE 4: the last thing before the tool runs.
	if err := revalidateSanitizePlan(plan); err != nil {
		return err
	}

	tctx, cancel := context.WithTimeout(ctx, plan.Options.TrimTimeout)
	defer cancel()

	// Structured arguments and "--" before the derived value. Never a shell
	// string. The mount point comes from the revalidated plan, not from a
	// caller-supplied path.
	cmd := exec.CommandContext(tctx, fstrimPath, "-v", "--", plan.MountPoint)
	started := time.Now()

	// A heartbeat so a UI can show liveness without pretending to know a
	// percentage.
	//
	// The WaitGroup is not decoration: the heartbeat reads res.Final while this
	// function writes it after the command returns, and without joining the
	// goroutine first that is a genuine data race on the result a caller reads.
	done := make(chan struct{})
	var beat sync.WaitGroup
	if plan.Options.Progress != nil {
		beat.Add(1)
		go func() {
			defer beat.Done()
			t := time.NewTicker(plan.Options.ProgressInterval)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					p := res.Final
					p.Elapsed = time.Since(started)
					plan.Options.Progress(p)
				}
			}
		}()
	}
	out, err := cmd.CombinedOutput()
	close(done)
	beat.Wait()

	res.Final.Elapsed = time.Since(started)
	if err != nil {
		if tctx.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("fstrim did not finish within %s on %s", plan.Options.TrimTimeout, plan.MountPoint)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("fstrim on %s failed: %v: %s", plan.MountPoint, err, strings.TrimSpace(string(out)))
	}
	if m := trimBytesRe.FindStringSubmatch(string(out)); len(m) == 2 {
		if v, perr := strconv.ParseUint(m[1], 10, 64); perr == nil {
			// The RANGE the kernel issued discard for — not a measurement of
			// anything erased. Named accordingly everywhere it surfaces.
			res.Final.BytesDiscarded = v
		}
	}
	res.Final.Phase = PhaseDone
	res.Final.Note = "Discard completed. The device was told which blocks are unused; what it does with " +
		"the previous contents is firmware-specific and cannot be verified from here."
	if st, serr := sanitizeStatfs(plan.Root); serr == nil {
		res.FreeBytesAtEnd = st.AvailBytes
		res.Final.FreeBytesNow = st.AvailBytes
	}
	// TRIM creates nothing, so there is nothing to reclaim and nothing to
	// remove. Saying so explicitly keeps the result honest instead of leaving
	// two false-looking booleans.
	res.ArtifactsRemoved = true
	res.SpaceReclaimed = true
	if plan.Options.Progress != nil {
		plan.Options.Progress(res.Final)
	}
	return nil
}

// ---------------------------------------------------------------- recovery

// SanitizeRecoveryReport is what startup recovery did.
type SanitizeRecoveryReport struct {
	InterruptedJobs int      `json:"interrupted_jobs"`
	ArtifactsRemove int      `json:"artifacts_removed"`
	BytesReclaimed  uint64   `json:"bytes_reclaimed"`
	Roots           []string `json:"roots_scanned,omitempty"`
	// Unexplained lists paths that LOOK like sanitization artifacts but could
	// not be proven to be. They are reported, never deleted.
	Unexplained []string `json:"unexplained_artifacts,omitempty"`
	Errors      []string `json:"errors,omitempty"`
}

// RecoverSanitizeJobs finds jobs a crash left running, removes their artifacts
// and verifies the space came back.
//
// Idempotent: running it twice is a no-op the second time. Automatic: it is
// called by EnsureSanitizeSubsystem at startup, so no operator has to notice a
// disk is mysteriously full and go looking with ssh.
func RecoverSanitizeJobs(ctx context.Context) SanitizeRecoveryReport {
	rep := SanitizeRecoveryReport{}
	records, err := ReadSanitizeJobs()
	if err != nil {
		rep.Errors = append(rep.Errors, "job records unreadable: "+err.Error())
		return rep
	}

	// The context is HONOURED, not merely accepted.
	//
	// It was taken as a parameter and never read, which mattered little while
	// this ran behind a capability check. It now runs on every boot, from
	// InstallStorageMaintenanceEngine, and every iteration touches the
	// filesystem: statfs and directory walks against a root recorded on disk.
	// A root on a remote mount that has gone away blocks in the kernel, and an
	// unbounded loop of those would hold service startup indefinitely — the
	// service that has to come up for an operator to see WHY.
	//
	// Abandoning recovery is safe in a way abandoning most work is not: an
	// artifact left behind is found again on the next boot, and the report says
	// the pass was cut short rather than claiming it finished.
	stopped := func() bool {
		if ctx.Err() == nil {
			return false
		}
		rep.Errors = append(rep.Errors, "recovery stopped early: "+ctx.Err().Error()+
			"; remaining artifacts are untouched and the roots still owed a sweep "+
			"were recorded for the next start")
		return true
	}

	seenRoots := map[string]bool{}
	for _, rec := range records {
		// BREAK, not return. Returning here skipped the hand-off below, so any
		// record this loop had already retired left its root unrecorded — and a
		// retired record is never re-derived, so those artifacts were stranded
		// exactly as if the hand-off did not exist. The early exit has to fall
		// through to the bookkeeping, not jump over it.
		if stopped() {
			break
		}
		if rec.State != JobRunning || jobIsLive(rec.JobID) {
			continue
		}
		rep.InterruptedJobs++
		seenRoots[rec.Root] = true

		// The root is re-authorized: a path read back from disk is exactly the
		// place a previous vulnerable release could have planted something.
		d := storageauth.AuthorizeSystem(sanitizeMutateOps, rec.Root)
		if !d.Allowed {
			rep.Errors = append(rep.Errors, fmt.Sprintf(
				"job %s: %s is no longer authorized storage (%s); artifacts left in place",
				rec.JobID, rec.Root, d.Reason))
			markRecordInterrupted(rec, "root no longer authorized; artifacts left in place")
			continue
		}
		before, _ := sanitizeStatfs(d.Path)
		cl, cerr := cleanupSanitizeArtifacts(d.Path, sanitizeArtifactDirName(rec.JobID))
		rep.Unexplained = append(rep.Unexplained, cl.Unexplained...)
		rep.ArtifactsRemove += cl.Removed
		rep.BytesReclaimed += cl.BytesFreed
		detail := fmt.Sprintf("interrupted; removed %d artifact(s), %s reclaimed",
			cl.Removed, humanBytes(cl.BytesFreed))
		cleanupFailed := cerr != nil && !errors.Is(cerr, errUnprovenArtifact)
		if cleanupFailed {
			rep.Errors = append(rep.Errors, fmt.Sprintf("job %s: %v", rec.JobID, cerr))
			detail = "interrupted; cleanup failed: " + cerr.Error()
		}
		if after, serr := sanitizeStatfs(d.Path); serr == nil {
			rec.FreeBytesAtEnd = after.AvailBytes
			if after.AvailBytes < before.AvailBytes {
				rep.Errors = append(rep.Errors, fmt.Sprintf(
					"job %s: free space on %s did not increase after cleanup", rec.JobID, d.Path))
			}
		}
		// Only retire the record once the workspace it points at is actually
		// gone. Retiring it while the artifacts are still there would leave
		// them with nothing pointing at them: the next pass skips non-running
		// records, so this root would never be swept again and the space would
		// be lost until somebody found the directory by hand. That holds for
		// an unproven workspace too — it is deliberately left in place, and
		// leaving it AND forgetting it is how it stops being reported. The
		// cost of keeping the record is a repeated attempt and a health check
		// that stays degraded: a report that keeps complaining, rather than a
		// disk that quietly stays full.
		if cleanupFailed || !cl.DirRemoved {
			_ = Audit(AuditEntry{
				PrincipalID: rec.PrincipalID, Operation: "sanitize_free_space",
				Fingerprint: rec.DeviceFingerprint, Model: rec.DeviceModel, Serial: rec.DeviceSerial,
				Requested: fmt.Sprintf("%s root=%s", rec.Method, rec.Root),
				Result:    "interrupted", Stage: "recovery",
				Detail: detail + "; the record is kept so the next recovery retries",
			})
			continue
		}
		markRecordInterrupted(rec, detail)
		_ = Audit(AuditEntry{
			PrincipalID: rec.PrincipalID, Operation: "sanitize_free_space",
			Fingerprint: rec.DeviceFingerprint, Model: rec.DeviceModel, Serial: rec.DeviceSerial,
			Requested: fmt.Sprintf("%s root=%s", rec.Method, rec.Root),
			Result:    "interrupted", Stage: "recovery", Detail: detail,
		})
	}

	// A ROOT OWED A SWEEP MUST OUTLIVE THE RECORD THAT NAMED IT.
	//
	// seenRoots is derived from records that were RUNNING. The loop above has
	// just retired them, so on the next start they are no longer running and
	// their roots are never re-derived. Stopping here without writing them down
	// would strand their artifacts permanently — which is the opposite of what
	// the stop message promises, and worse than the unbounded hang the deadline
	// was added to prevent. Losing disk space silently is not an acceptable
	// price for bounding startup.
	pending := loadPendingSweepRoots()
	for r := range seenRoots {
		pending[r] = true
	}
	swept := map[string]bool{}
	for r := range pending {
		if stopped() {
			break
		}
		rep.Roots = append(rep.Roots, r)
		orphans, errs := sweepSanitizeOrphans(r)
		rep.Unexplained = append(rep.Unexplained, orphans...)
		rep.Errors = append(rep.Errors, errs...)
		swept[r] = true
	}
	remaining := make([]string, 0, len(pending))
	for r := range pending {
		if !swept[r] {
			remaining = append(remaining, r)
		}
	}
	sort.Strings(remaining)
	if err := savePendingSweepRoots(remaining); err != nil {
		rep.Errors = append(rep.Errors,
			"the roots still owed an orphan sweep could not be recorded, so they "+
				"will not be retried automatically: "+err.Error())
	}
	sort.Strings(rep.Roots)
	sort.Strings(rep.Unexplained)
	return rep
}

// pendingSweepPath holds roots whose orphan sweep is still owed. It is the
// hand-off between one bounded recovery pass and the next: without it, a root
// is only ever sweepable while the record that named it is still RUNNING, and
// recovery retires those records before it sweeps.
func pendingSweepPath() string {
	return filepath.Join(StateDir(), "sanitize-pending-sweep.json")
}

// loadPendingSweepRoots never fails loudly: an unreadable or corrupt hand-off
// file must not stop a boot. It returns what it can, and the roots it cannot
// read are recovered the ordinary way the next time a job runs on them.
func loadPendingSweepRoots() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(pendingSweepPath())
	if err != nil {
		return out
	}
	var roots []string
	if json.Unmarshal(b, &roots) != nil {
		return out
	}
	for _, r := range roots {
		if r = strings.TrimSpace(r); r != "" {
			out[r] = true
		}
	}
	return out
}

// savePendingSweepRoots writes the hand-off atomically, and REMOVES the file
// when nothing is owed — so a stale list can never make later boots sweep roots
// that no longer exist.
func savePendingSweepRoots(roots []string) error {
	if len(roots) == 0 {
		if err := os.Remove(pendingSweepPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(StateDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(roots)
	if err != nil {
		return err
	}
	tmp := pendingSweepPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, pendingSweepPath())
}

func markRecordInterrupted(rec SanitizeJobRecord, detail string) {
	finishSanitizeRecord(&rec, JobInterrupted, detail)
	_ = writeSanitizeJobRecord(rec)
}

// RecoverSanitizeAtRoot cleans provable artifacts under one filesystem root,
// for the case where the state directory itself was lost.
func RecoverSanitizeAtRoot(root string) (SanitizeRecoveryReport, error) {
	rep := SanitizeRecoveryReport{Roots: []string{root}}
	d := storageauth.AuthorizeSystem(sanitizeMutateOps, root)
	if !d.Allowed {
		return rep, refuse(RefuseUnauthorizedPath, "%s is not authorized storage (%s)", root, d.Reason)
	}
	before, _ := sanitizeStatfs(d.Path)

	names, err := listSanitizeArtifactDirs(d.Path)
	if err != nil {
		return rep, err
	}
	for _, name := range names {
		cl, cerr := cleanupSanitizeArtifacts(d.Path, name)
		rep.Unexplained = append(rep.Unexplained, cl.Unexplained...)
		rep.ArtifactsRemove += cl.Removed
		rep.BytesReclaimed += cl.BytesFreed
		if cerr != nil && !errors.Is(cerr, errUnprovenArtifact) {
			rep.Errors = append(rep.Errors, cerr.Error())
		}
	}
	if after, serr := sanitizeStatfs(d.Path); serr == nil && after.AvailBytes < before.AvailBytes {
		rep.Errors = append(rep.Errors, "free space did not increase after cleanup of "+d.Path)
	}
	sort.Strings(rep.Unexplained)
	return rep, nil
}

func sweepSanitizeOrphans(root string) ([]string, []string) {
	d := storageauth.AuthorizeSystem(sanitizeMutateOps, root)
	if !d.Allowed {
		return nil, []string{root + ": " + d.Detail}
	}
	names, err := listSanitizeArtifactDirs(d.Path)
	if err != nil {
		return nil, []string{root + ": " + err.Error()}
	}
	var unexplained, errs []string
	for _, name := range names {
		cl, cerr := cleanupSanitizeArtifacts(d.Path, name)
		unexplained = append(unexplained, cl.Unexplained...)
		if cerr != nil && !errors.Is(cerr, errUnprovenArtifact) {
			errs = append(errs, cerr.Error())
		}
	}
	return unexplained, errs
}

func listSanitizeArtifactDirs(root string) ([]string, error) {
	f, err := openSanitizeRoot(root, storageauth.OpRead)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, sanitizeArtifactPrefix) &&
			sanitizeJobIDRe.MatchString(strings.TrimPrefix(n, sanitizeArtifactPrefix)) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------- health

// SanitizeHealth is machine-readable subsystem health that NAMES the invariant
// that failed rather than reporting a bare "degraded".
type SanitizeHealth struct {
	Status string `json:"status"` // healthy | degraded | failed | unprovisioned
	// Enforcing reports whether sanitization may run at all. When false, every
	// plan is refused — the fail-closed direction.
	Enforcing bool `json:"enforcing"`
	// Invariant names the property that failed, in words an operator can act on.
	Invariant string `json:"invariant,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// SelfTest is "passed" or "failed" — the outcome of proving the safety
	// properties hold, not merely that the code is present.
	SelfTest             string    `json:"self_test"`
	ConfinementAvailable bool      `json:"kernel_path_confinement"`
	StateDir             string    `json:"state_dir"`
	InterruptedRecovered int       `json:"interrupted_jobs_recovered"`
	BytesReclaimed       uint64    `json:"bytes_reclaimed_at_startup"`
	UnexplainedArtifacts []string  `json:"unexplained_artifacts,omitempty"`
	RecoveryErrors       []string  `json:"recovery_errors,omitempty"`
	CheckedAt            time.Time `json:"checked_at"`
}

var (
	sanitizeHealthMu sync.RWMutex
	sanitizeHealth   = SanitizeHealth{
		Status: "unprovisioned", SelfTest: "not run",
		Invariant: "the free-space sanitization subsystem has not been provisioned",
	}
)

// SanitizeHealthNow reports the cached health.
func SanitizeHealthNow() SanitizeHealth {
	sanitizeHealthMu.RLock()
	defer sanitizeHealthMu.RUnlock()
	return sanitizeHealth
}

// confinementAvailable is indirected so a test can present a kernel without
// openat2 without needing one. It is only the health verdict that is
// simulated: the confined syscalls themselves are not stubbed anywhere.
var confinementAvailable = storageauth.ConfinementAvailable

// EnsureSanitizeSubsystem provisions, self-validates and recovers.
//
// Idempotent and automatic: it creates its own state directory, proves its
// safety properties, and cleans up after a crash without anybody being asked to
// run something over ssh. It is safe to call on every start.
func EnsureSanitizeSubsystem(ctx context.Context) SanitizeHealth {
	h := SanitizeHealth{StateDir: sanitizeJobsDir(), CheckedAt: time.Now()}
	confined, why := confinementAvailable()
	h.ConfinementAvailable = confined

	if err := os.MkdirAll(sanitizeJobsDir(), 0o700); err != nil {
		h.Status, h.SelfTest, h.Enforcing = "failed", "failed", false
		h.Invariant = "the job state directory must exist and be writable"
		h.Detail = err.Error()
		publishSanitizeHealth(h)
		return h
	}

	// PROPERTY validation, not existence: the reserve and pass-sequence
	// invariants are proven against a table of inputs including the degenerate
	// ones. If they do not hold, sanitization is disabled entirely.
	if err := sanitizeSelfTest(); err != nil {
		h.Status, h.SelfTest, h.Enforcing = "failed", "failed", false
		h.Invariant = "the safety-reserve and pass-sequence invariants must hold"
		h.Detail = err.Error()
		publishSanitizeHealth(h)
		return h
	}
	h.SelfTest = "passed"

	// Recovery runs BEFORE the confinement verdict is allowed to end this
	// function. Refusing to start new sanitizations on a kernel without
	// openat2 is correct; skipping recovery there is not, because the disk
	// space held by an interrupted job is then never even looked at and the
	// operator sees a full filesystem with no explanation. What recovery can
	// actually free without confinement is limited — its cleanup is built on
	// openat2 and will report errors instead of deleting — so this makes the
	// situation VISIBLE rather than silently skipped. It does not delete
	// anything unconfined.
	rep := RecoverSanitizeJobs(ctx)
	h.InterruptedRecovered = rep.InterruptedJobs
	h.BytesReclaimed = rep.BytesReclaimed
	h.UnexplainedArtifacts = rep.Unexplained
	h.RecoveryErrors = rep.Errors

	if !confined {
		h.Status, h.Enforcing = "degraded", false
		h.Invariant = "the kernel must be able to confine path resolution (openat2/RESOLVE_BENEATH)"
		h.Detail = why
		publishSanitizeHealth(h)
		return h
	}

	h.Enforcing = true
	h.Status = "healthy"
	if len(rep.Errors) > 0 || len(rep.Unexplained) > 0 {
		// Degraded, not failed: sanitization still runs. Something on a data
		// disk needs a human's eyes, and it is named here rather than deleted.
		h.Status = "degraded"
		h.Invariant = "every artifact left by an interrupted job must be provable and removable"
		h.Detail = "some artifacts could not be proven to belong to CasaNova and were left untouched"
	}
	publishSanitizeHealth(h)
	return h
}

func publishSanitizeHealth(h SanitizeHealth) {
	sanitizeHealthMu.Lock()
	sanitizeHealth = h
	sanitizeHealthMu.Unlock()
}

// sanitizeSelfTest proves the properties this subsystem's safety rests on.
//
// It checks BEHAVIOUR over a table of inputs, including degenerate ones, rather
// than checking that a function exists. A regression that removes the reserve
// stops the whole feature at startup instead of filling a disk at 3am.
func sanitizeSelfTest() error {
	sizes := []uint64{
		0, 1, 1 << 20, 100 << 20, 1 << 30, 64 << 30, 1 << 40, 8 << 40, 1 << 50,
	}
	for _, total := range sizes {
		for _, override := range []uint64{0, 1, 1 << 30} {
			r := sanitizeReserveBytes(total, override)
			if total > 0 && r == 0 {
				return fmt.Errorf("reserve is zero for a %s filesystem: the writer would be allowed "+
					"to consume 100%% of it", humanBytes(total))
			}
			if total >= minSanitizeReserveBytes && r < minSanitizeReserveBytes {
				return fmt.Errorf("reserve %s for a %s filesystem is below the absolute floor %s",
					humanBytes(r), humanBytes(total), humanBytes(minSanitizeReserveBytes))
			}
			if total > 0 && r > total {
				return fmt.Errorf("reserve %s exceeds the %s filesystem it protects",
					humanBytes(r), humanBytes(total))
			}
			for _, avail := range []uint64{0, total / 2, total} {
				t := sanitizeTargetBytes(avail, r)
				if t > avail {
					return fmt.Errorf("target %s exceeds the %s available", humanBytes(t), humanBytes(avail))
				}
				if t > 0 && avail-t < r {
					return fmt.Errorf("writing %s of %s available would leave less than the %s reserve",
						humanBytes(t), humanBytes(avail), humanBytes(r))
				}
			}
		}
	}

	for n, seq := range multiSanitizePassSequences {
		if n < MinSanitizePasses || n > MaxSanitizePasses {
			return fmt.Errorf("pass sequence table contains an out-of-range entry %d", n)
		}
		if len(seq) != n {
			return fmt.Errorf("the %d-pass sequence has %d passes", n, len(seq))
		}
		last := seq[len(seq)-1]
		if last.Kind != passZero {
			// A run that ends on a random pass leaves the media full of
			// high-entropy data, which is indistinguishable from corruption.
			return fmt.Errorf("the %d-pass sequence does not end with a zero clean pass", n)
		}
	}

	// The semantics table must never claim more than the code does.
	for _, s := range SanitizeMethodCatalog() {
		if s.CryptographicErase {
			return fmt.Errorf("method %s claims a cryptographic erase, which this package does not perform", s.Method)
		}
		if s.Method == SanitizeTrim && s.HostOverwrites {
			return errors.New("TRIM is described as overwriting data, which it does not do")
		}
		if s.Method == SanitizeMultiPass && !s.RotationalOnly {
			return errors.New("multi-pass is not restricted to rotational media")
		}
		if s.Method == SanitizeDeviceErase && s.Implemented {
			return errors.New("device sanitize is described as implemented, which it is not")
		}
	}
	return nil
}
