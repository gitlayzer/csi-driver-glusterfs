package driver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"k8s.io/klog/v2"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

// lockTable is the set of in-flight operation keys the Node service serializes
// its work with; [inflight] is the production implementation.
//
// It is an interface so that a test can observe the exact moment the recovery
// takes the stage lock of a volume, which is the window the second read of the
// staging record in [nodeService.recoverVolume] exists for.
type lockTable interface {
	// tryAcquire marks key as in progress and reports whether it was free.
	tryAcquire(key string) bool
	// release removes key from the set.
	release(key string)
}

// recoveryOutcome is what one pass over one staging record did. It is the unit of
// the log line and of the summary of a pass, so that an operator can tell an
// interrupted recovery from a broken one.
type recoveryOutcome int

const (
	// recoveryHealthy means nothing had to be done: the staging mount is fine and
	// no bind target is pending.
	recoveryHealthy recoveryOutcome = iota
	// recoveryBusy means the stage lock of the volume is held; the next pass
	// retries.
	recoveryBusy
	// recoveryStale means the record was removed because the volume is not
	// staged any more.
	recoveryStale
	// recoveryMismatch means something else is mounted at the staging path. It is
	// left alone: unmounting a mount the driver did not make could destroy data.
	recoveryMismatch
	// recoveryRecovered means the volume was mounted again and/or its bind
	// targets were refreshed, and nothing is pending.
	recoveryRecovered
	// recoveryPartial means some bind targets are still pending and the next pass
	// retries them.
	recoveryPartial
)

// recoveryOutcomeNames are the spellings the log lines use. They are stable, so
// that a log query can match them.
var recoveryOutcomeNames = map[recoveryOutcome]string{
	recoveryHealthy:   "healthy",
	recoveryBusy:      "busy",
	recoveryStale:     "stale",
	recoveryMismatch:  "mismatch",
	recoveryRecovered: "recovered",
	recoveryPartial:   "partial",
}

// String returns the name of the outcome, or "recoveryOutcome(<n>)" for a value
// this package does not define.
func (o recoveryOutcome) String() string {
	if name, ok := recoveryOutcomeNames[o]; ok {
		return name
	}
	return fmt.Sprintf("recoveryOutcome(%d)", int(o))
}

// recoverySummary counts how many records ended in each outcome during one pass.
type recoverySummary struct {
	// counts is indexed by recoveryOutcome.
	counts map[recoveryOutcome]int
	// failures is the number of records that could not be processed at all.
	failures int
	// processed is the number of records the pass looked at.
	processed int
}

// count records one outcome.
func (s *recoverySummary) count(outcome recoveryOutcome) {
	if s.counts == nil {
		s.counts = make(map[recoveryOutcome]int, len(recoveryOutcomeNames))
	}
	s.counts[outcome]++
}

// idle reports whether the pass had nothing to report: every record was healthy
// and none failed.
func (s *recoverySummary) idle() bool {
	if s.failures > 0 {
		return false
	}
	for outcome, n := range s.counts {
		if n > 0 && outcome != recoveryHealthy {
			return false
		}
	}
	return true
}

// fields renders the summary as klog key/value pairs. Zero entries are skipped,
// so that a quiet pass logs one short line, and the order is fixed, because the
// log lines of two passes are meant to be comparable.
func (s *recoverySummary) fields() []any {
	kvs := make([]any, 0, 2+2*len(recoveryOutcomeNames))
	kvs = append(kvs, "records", s.processed)
	if s.failures > 0 {
		kvs = append(kvs, "failures", s.failures)
	}
	for _, outcome := range []recoveryOutcome{
		recoveryRecovered, recoveryPartial, recoveryMismatch, recoveryStale, recoveryBusy, recoveryHealthy,
	} {
		if n := s.counts[outcome]; n > 0 {
			kvs = append(kvs, outcome.String(), n)
		}
	}
	return kvs
}

// runRecovery runs one pass at once and then one per interval until ctx is
// done. interval <= 0 runs the first pass only.
//
// It is the only goroutine the driver starts besides the gRPC server.
// [Driver.Serve] cancels the context it derives and waits for this function to
// return on every return path, so a stopped plugin never leaves a recovery pass
// behind.
func (s *nodeService) runRecovery(ctx context.Context, interval time.Duration) {
	klog.InfoS("starting mount recovery", "interval", interval.String())
	defer klog.InfoS("mount recovery stopped")

	if interval <= 0 {
		s.recoverAll(ctx)
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		s.recoverAll(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// recoverAll runs one pass over all records and returns how many records ended
// in each outcome, plus the number of failures.
//
// A cancelled ctx stops the pass before the next record: a shutdown must not wait
// for the remaining volumes.
func (s *nodeService) recoverAll(ctx context.Context) recoverySummary {
	var summary recoverySummary
	if s.records == nil {
		return summary
	}

	names, err := s.records.list()
	if err != nil {
		klog.ErrorS(err, "cannot list the staging records, skipping this recovery pass")
		return summary
	}
	if len(names) == 0 {
		klog.V(4).InfoS("no staging records, nothing to recover")
		return summary
	}

	for _, name := range names {
		if ctx.Err() != nil {
			klog.V(2).InfoS("stopping the recovery pass early", "reason", context.Cause(ctx),
				"remaining", len(names)-summary.processed)
			break
		}

		summary.processed++
		outcome, err := s.recoverVolume(ctx, name)
		if err != nil {
			summary.failures++
			klog.ErrorS(err, "mount recovery failed", "record", name)
			continue
		}
		summary.count(outcome)
		switch outcome {
		case recoveryRecovered, recoveryPartial, recoveryMismatch:
			klog.InfoS("mount recovery handled a record", "record", name, "outcome", outcome.String())
		case recoveryStale, recoveryBusy:
			klog.V(2).InfoS("mount recovery skipped a record", "record", name, "outcome", outcome.String())
		case recoveryHealthy:
			klog.V(4).InfoS("mount recovery found a record healthy", "record", name)
		}
	}

	if !summary.idle() {
		klog.InfoS("mount recovery pass finished", summary.fields()...)
	}
	return summary
}

// recoverVolume brings one staged volume back after the plugin restarted.
//
// The order of the steps is the whole point of the function and follows
// docs/architecture.md §5.3 with D-19 and D-20:
//
//  1. the bind targets are read from mountinfo and saved in the record while the
//     staging mount is still there, because detaching it removes its mountinfo
//     entry and with it every trace of its bind targets;
//  2. the corrupted staging mount is detached lazily and mounted again with the
//     servers of the live configuration;
//  3. every pending bind target is detached and bound again with its recorded
//     read-only flag.
//
// Every step that fails returns an error and leaves the record in a state the next
// pass can continue from.
func (s *nodeService) recoverVolume(ctx context.Context, name string) (recoveryOutcome, error) {
	rec, err := s.records.loadFile(name)
	if err != nil {
		// An unreadable record is kept: it is the only trace of a staged volume
		// and an operator has to look at it.
		return recoveryHealthy, fmt.Errorf("load staging record %q: %w", name, err)
	}
	volumeID := rec.VolumeID
	id, err := decodeVolumeID(volumeID)
	if err != nil {
		return recoveryHealthy, fmt.Errorf("staging record %q: %w", name, err)
	}

	// The same lock the Node RPCs use: the recovery must not race a stage or an
	// unstage of the same volume.
	key := nodeStageLockPrefix + volumeID
	if !s.locks.tryAcquire(key) {
		return recoveryBusy, nil
	}
	defer s.locks.release(key)

	// The record is read again under the lock: an unstage may have finished
	// between the first read and the lock, and then there is nothing to do.
	rec, err = s.records.load(volumeID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return recoveryHealthy, nil
	case err != nil:
		return recoveryHealthy, fmt.Errorf("reload staging record of volume %s: %w", volumeID, err)
	}

	staging := rec.StagingTargetPath
	state, err := s.mounter.VolumeMountState(staging, id.Volume, id.Subdir)
	if err != nil {
		return recoveryHealthy, fmt.Errorf("check the staging mount of volume %s: %w", volumeID, err)
	}

	switch state {
	case mounter.MountStateMismatch:
		// Something else is mounted there. Unmounting it would destroy the state
		// of another volume, so the record is left for an operator.
		klog.InfoS("staging path of a record is mounted from something else, leaving it alone",
			"volumeID", volumeID, "stagingTargetPath", staging)
		return recoveryMismatch, nil
	case mounter.MountStateNotMounted:
		// Without a corrupted target there is nothing left to repair: the node
		// restarted and lost the mounts, and kubelet stages the volume again
		// before it publishes it.
		if !s.hasCorruptedTarget(rec, id) {
			if rmErr := s.records.remove(volumeID); rmErr != nil {
				return recoveryHealthy, rmErr
			}
			klog.V(2).InfoS("removed a stale staging record, the volume is not mounted",
				"volumeID", volumeID, "stagingTargetPath", staging)
			return recoveryStale, nil
		}
	case mounter.MountStateCorrupted:
		// Find the bind targets *before* detaching the staging mount: after the
		// detach its mountinfo entry is gone and with it the device number and
		// root that identify its bind mounts.
		found, ferr := s.mounter.BindTargets(staging)
		if ferr != nil {
			return recoveryHealthy, fmt.Errorf("find the bind targets of %q: %w", staging, ferr)
		}
		rec.Targets = mergeTargets(rec.Targets, found)
		// Saved before the detach: a crash between the two must not lose the list
		// of targets that are about to be detached.
		if serr := s.records.save(rec); serr != nil {
			return recoveryHealthy, serr
		}
		if derr := s.mounter.Detach(staging); derr != nil {
			return recoveryHealthy, fmt.Errorf("detach the corrupted staging mount %q: %w", staging, derr)
		}
		klog.InfoS("detached a corrupted staging mount", "volumeID", volumeID,
			"stagingTargetPath", staging, "targets", len(rec.Targets))
	case mounter.MountStateMounted:
		if len(rec.Targets) == 0 {
			return recoveryHealthy, nil
		}
	}

	// Mount again: after a detach, and also when the staging mount is gone but a
	// bind target of an interrupted recovery still has to be refreshed.
	remounted := false
	if state != mounter.MountStateMounted {
		if err := s.remountVolume(ctx, rec, id); err != nil {
			return recoveryHealthy, err
		}
		remounted = true
	}

	pending, refreshed := s.refreshTargets(ctx, rec, id)
	if len(pending) != len(rec.Targets) {
		rec.Targets = pending
		if serr := s.records.save(rec); serr != nil {
			return recoveryHealthy, serr
		}
	}
	if len(pending) > 0 {
		return recoveryPartial, nil
	}
	if remounted || refreshed > 0 {
		klog.InfoS("recovered a staged volume", "volumeID", volumeID,
			"stagingTargetPath", staging, "remounted", remounted, "refreshedTargets", refreshed)
		return recoveryRecovered, nil
	}
	return recoveryHealthy, nil
}

// hasCorruptedTarget reports whether any bind target of rec is still a corrupted
// mount point.
//
// It is what tells an interrupted recovery (the staging mount is gone but the
// bind mounts of the Pods are still there and dead) from a node that simply lost
// all of its mounts: in the second case kubelet stages the volume again on its own
// and the record is stale.
func (s *nodeService) hasCorruptedTarget(rec stageRecord, id volumeid.ID) bool {
	for _, target := range rec.Targets {
		state, err := s.mounter.VolumeMountState(target.Path, id.Volume, id.Subdir)
		if err != nil {
			// Unknown is treated as "not corrupted": the record is then removed
			// like a stale one, which is the safe answer, because kubelet stages
			// the volume again before the next publish.
			klog.V(4).InfoS("cannot tell the state of a bind target", "targetPath", target.Path, "err", err)
			continue
		}
		if state == mounter.MountStateCorrupted {
			return true
		}
	}
	return false
}

// remountVolume mounts rec again at its staging path with the servers of the live
// configuration.
func (s *nodeService) remountVolume(ctx context.Context, rec stageRecord, id volumeid.ID) error {
	cluster, ok := s.cluster(id.ClusterID)
	if !ok {
		// The record is kept: a cluster that is temporarily missing from the
		// ConfigMap must not cost the node its mounts.
		return fmt.Errorf("cluster %q of staged volume %s is not configured", id.ClusterID, rec.VolumeID)
	}
	if err := s.ensureLogDir(); err != nil {
		return err
	}

	// The servers are taken from the configuration that is live now, not from the
	// record: a server list that changed while the plugin was down has to be used.
	spec := mounter.Spec{
		Servers:  cluster.Servers,
		Volume:   id.Volume,
		Subdir:   id.Subdir,
		ReadOnly: rec.ReadOnly,
		Options:  rec.MountFlags,
	}
	if err := s.mounter.MountGlusterfs(ctx, spec, rec.StagingTargetPath); err != nil {
		// Wrapped with mountErrorToStatus so that a cancelled ctx keeps its code;
		// the message carries a short cause and no secret.
		return fmt.Errorf("mount volume %s again: %w", rec.VolumeID, mountErrorToStatus("recover volume", err))
	}
	klog.InfoS("mounted a staged volume again", "volumeID", rec.VolumeID,
		"stagingTargetPath", rec.StagingTargetPath, "servers", cluster.Servers)
	return nil
}

// refreshTargets detaches and binds again every pending target of rec.
//
// It returns the targets that are still pending and how many were refreshed.
// A target is dropped from the list when it is mounted, not mounted at all (the
// Pod is gone) or mounted from something else (not ours): in all three cases the
// recovery has nothing left to do for it.
func (s *nodeService) refreshTargets(ctx context.Context, rec stageRecord, id volumeid.ID) (pending []recordTarget, refreshed int) {
	for _, target := range rec.Targets {
		key := nodePublishLockPrefix + target.Path
		if !s.locks.tryAcquire(key) {
			// A publish or unpublish of this target is in flight; the next pass
			// looks at it again.
			pending = append(pending, target)
			continue
		}

		wantMore, err := s.refreshTarget(ctx, rec, id, target)
		s.locks.release(key)
		if err != nil {
			// Logged, not returned: one target that cannot be refreshed must not
			// stop the others. It stays pending.
			klog.ErrorS(err, "cannot refresh a bind target, keeping it for the next pass",
				"volumeID", rec.VolumeID, "targetPath", target.Path)
			pending = append(pending, target)
			continue
		}
		if wantMore {
			refreshed++
		}
	}
	return pending, refreshed
}

// refreshTarget repairs one bind target of a staged volume.
//
// It reports whether the target was bound again. A target that is mounted, not
// mounted or mounted from something else needs no action.
func (s *nodeService) refreshTarget(ctx context.Context, rec stageRecord, id volumeid.ID, target recordTarget) (bool, error) {
	state, err := s.mounter.VolumeMountState(target.Path, id.Volume, id.Subdir)
	if err != nil {
		return false, fmt.Errorf("check the bind target %q: %w", target.Path, err)
	}
	if state != mounter.MountStateCorrupted {
		return false, nil
	}

	// The dead mount is detached first: bind mounting over it would leave the
	// broken mount in place and the Pod would keep seeing ENOTCONN.
	if derr := s.mounter.Detach(target.Path); derr != nil {
		return false, fmt.Errorf("detach the corrupted bind mount %q: %w", target.Path, derr)
	}
	// Bound again from the staging mount, with the read-only flag of the original
	// bind mount: a read-only Pod must not suddenly get a writable volume, and a
	// writable one must not lose write access.
	if berr := s.mounter.BindMount(ctx, rec.StagingTargetPath, target.Path, target.ReadOnly); berr != nil {
		return false, fmt.Errorf("bind mount %q again: %w", target.Path, berr)
	}
	klog.InfoS("bound a target again", "volumeID", rec.VolumeID,
		"stagingTargetPath", rec.StagingTargetPath, "targetPath", target.Path, "readOnly", target.ReadOnly)
	return true, nil
}

// mergeTargets merges the stored targets with the ones just found in mountinfo.
//
// The order of the stored list is kept, newly found targets are appended, and a
// path that appears twice keeps its position but takes the read-only flag of
// found: the mount table describes the mount that exists right now, the record
// may remember an older one.
func mergeTargets(stored []recordTarget, found []mounter.BindTarget) []recordTarget {
	merged := make([]recordTarget, 0, len(stored)+len(found))
	index := make(map[string]int, len(stored)+len(found))

	// add appends target or replaces the entry of an already known path.
	add := func(target recordTarget) {
		if i, dup := index[target.Path]; dup {
			merged[i] = target
			return
		}
		index[target.Path] = len(merged)
		merged = append(merged, target)
	}

	for _, target := range stored {
		add(target)
	}
	for _, target := range found {
		// The targets of mountinfo always replace the stored flag, also when the
		// path is already in the list.
		add(recordTarget{Path: target.Path, ReadOnly: target.ReadOnly})
	}
	return merged
}
