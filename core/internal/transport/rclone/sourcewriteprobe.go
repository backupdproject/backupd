package rclone

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"

	"github.com/backupdproject/backupd/core/internal/transport"
)

// The compile-time assertion that this adapter really is the capability
// the connection check type-asserts for. Nothing else forces it: the
// service asks a transport.Transport whether it also implements
// transport.SourceWriteProbe, and a method whose signature drifted would
// turn that assertion false at runtime and quietly report every source in
// the deployment as not writable.
var _ transport.SourceWriteProbe = (*Adapter)(nil)

// probeBody is what a probe object contains while it briefly exists. It is
// fixed, tiny and says what it is, so an operator who finds one left
// behind by a probe whose removal failed (or whose process was killed
// between the two halves) knows immediately what wrote it and that
// deleting it is safe. internal/mediumcheck's probeBody is the same
// decision on the medium side.
var probeBody = []byte("backupd write-permission probe. This file is written and deleted by a connection check and is safe to remove.\n")

// The three refusals ProbeSourceWrite reports, as this package's own
// errors, and the reason they are all it reports.
//
// This is the one call in the product that writes a path of its own making
// into somebody else's directory, so the error it gets back is the one
// most likely to name a remote path, a chroot's home directory and the
// probe's own generated filename all in one string — and every one of
// those travels: internal/sourcecheck hands a failed step's cause to its
// Observe hook, which is the operator's log (and, through obs, the
// activity feed). Issue #852's fourth requirement is that nothing from
// this probe leaks there, and the way that is kept true is STRUCTURAL
// rather than careful: the underlying error is classified and then
// dropped, so there is no string here for a path to be in.
//
// What survives is the half a caller can act on: the FR-22 category
// (transport.PermissionDenied is "your account is read-only there",
// everything else is "this could not be established") and, for the
// removal, transport.ErrProbeNotRemoved, which is the one outcome that
// leaves litter behind.
var (
	errProbeNotWritten   = errors.New("rclone: the write probe could not be created under the source's remote path")
	errProbeNotDeleted   = errors.New("rclone: the write probe was created and could not be deleted again")
	errProbeStillThere   = errors.New("rclone: the write probe was deleted and is still listed at its own name")
	errProbeNoRemotePath = errors.New("rclone: a write probe needs a source with a remote path")
)

// ProbeSourceWrite proves this source's credentials may both create and
// remove an object under its root, by doing exactly that (issue #852).
//
// # What it proves, and why it has to be a real round trip
//
// FR-16's delete-from-source is the one thing this manager does to a
// producer's own machine, and the permission it needs is not the one a
// backup needs: an account that can read every byte under a directory may
// still be unable to unlink a single file in it, which is the recommended
// posture in plenty of deployments and the default in some. Nothing short
// of writing and removing an object establishes the difference. A stat of
// the directory answers a different question (the mode bits of a chroot
// say nothing about what the SFTP server will permit for this account),
// and asking the far side for its opinion is not something SFTP offers.
//
// So: one object, at a name nothing else can produce, created, removed,
// and its absence confirmed. The confirmation is not ceremony. An SFTP
// server that accepts a remove and does nothing (a union mount with a
// read-only lower layer is the real case) would otherwise be reported as a
// source this manager may delete from, and the first thing to find out
// otherwise would be FR-16 silently retaining everything, forever, while
// the journal said it had deleted.
//
// # What it never leaves behind, and the one case where it can
//
// The remove runs whatever the write did, and the error it returns says
// which half failed. Only one shape leaves an object behind — a create
// that succeeded and a remove that did not — and that one is reported
// through transport.ErrProbeNotRemoved rather than folded into "not
// writable", because an operator has a file of ours to delete and needs to
// be told so.
//
// # What it never says
//
// See the errors above: the underlying error is classified and then
// dropped, so no remote path, no chroot home and no probe filename can
// reach a log, a feed or an API response through this call.
func (a *Adapter) ProbeSourceWrite(ctx context.Context, src transport.Source) error {
	if src.Root == "" {
		return transport.NewError(transport.Configuration, "probe_source_write", errProbeNoRemotePath)
	}
	ctx = oneConnectionAtATime(ctx)
	f, err := a.fsFor(ctx, src)
	if err != nil {
		// A source whose Fs cannot even be built is not a read-only
		// source, it is an unreachable one, and the caller's earlier
		// steps have already said so in their own words. It still comes
		// back as "not proven writable", which is the fail-safe answer.
		return WrapCtx(ctx, "probe_source_write", err)
	}
	defer shutdownFs(ctx, f)

	name, err := probeObjectName()
	if err != nil {
		return transport.NewError(transport.Configuration, "probe_source_write", err)
	}

	info := object.NewStaticObjectInfo(name, time.Now(), int64(len(probeBody)), true, nil, f)
	written, err := f.Put(ctx, bytes.NewReader(probeBody), info)
	if err != nil {
		return transport.NewError(ClassifyCtx(ctx, err), "probe_source_write", errProbeNotWritten)
	}

	if err := written.Remove(ctx); err != nil {
		return transport.NewError(ClassifyCtx(ctx, err), "probe_source_write",
			errors.Join(transport.ErrProbeNotRemoved, errProbeNotDeleted))
	}

	// The delete is CONFIRMED, for the reason the doc above gives: a
	// backend that accepts a remove and keeps the object would otherwise
	// be reported as one this manager may delete from. fs.ErrorObjectNotFound
	// is the success condition here, so an error that is anything else —
	// including a connection that dropped between the two calls — is
	// unconfirmed rather than confirmed, and unconfirmed is not writable.
	if _, err := f.NewObject(ctx, name); !errors.Is(err, fs.ErrorObjectNotFound) {
		if err == nil {
			return transport.NewError(transport.Permanent, "probe_source_write",
				errors.Join(transport.ErrProbeNotRemoved, errProbeStillThere))
		}
		return transport.NewError(ClassifyCtx(ctx, err), "probe_source_write",
			errors.Join(transport.ErrProbeNotRemoved, errProbeNotDeleted))
	}
	return nil
}

// probeObjectName builds the name this probe's object lives at, under the
// source's own root.
//
// Random rather than derived from a clock, a hostname or a fixed word, and
// 16 bytes of it, for internal/mediumcheck.probeKey's reason: two checks
// running at once must not share an object, and a probe left behind by a
// run that was killed must never be mistaken for this one's, because the
// confirmation step's whole verdict is "the thing I wrote is gone".
//
// A leftover is kept out of the artifact pipeline by internal/discovery,
// which skips any basename carrying transport.ProbeObjectPrefix, and not
// by the prefix being a dotfile: this comment used to claim an FR-8
// include pattern could not match a dotfile, and it can (no patterns at
// all matches everything, and path.Match gives a dot no special meaning).
// The dot buys only that an operator reading a plain directory listing is
// not shown one.
func probeObjectName() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return transport.ProbeObjectPrefix + hex.EncodeToString(raw[:]), nil
}
