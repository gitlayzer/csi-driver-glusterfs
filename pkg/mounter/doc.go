// Package mounter mounts and unmounts GlusterFS volumes for the Node service.
//
// It has two responsibilities and nothing else (docs/architecture.md §5.3):
//
//   - [BuildMountArgs] turns a [Spec] plus a log directory into the source and
//     the option list of a glusterfs FUSE mount, validating every value and
//     passing only allowlisted StorageClass mountOptions through.
//   - [Mounter] performs the idempotent mount and unmount operations on top of
//     k8s.io/mount-utils: one FUSE mount per volume in the NodeStage path and a
//     bind mount per Pod in the NodePublish path.
//
// Two primitives exist only for the recovery that runs when the node plugin
// restarted and left dead FUSE mounts behind (docs/architecture.md §5.3, D-19,
// D-20): [Mounter.BindTargets] finds the bind mounts of a staging path by
// parsing mountinfo, which still works when the mount point is corrupted and
// mount-utils' GetMountRefs gives up, and [Mounter.Detach] removes a dead mount
// lazily, which succeeds although Pods still hold it. The [BindTarget] the first
// one returns carries the read-only flag so that a refreshed bind mount is made
// with the same options as the original one.
//
// The package knows nothing about CSI: it takes no context of the CSI request,
// never touches its Secrets and returns no gRPC status. Failures are expressed
// with the sentinels below, which the Node service (T-06) maps to CSI codes:
//
//   - [ErrInvalidSpec] and [ErrInvalidOption] -> codes.InvalidArgument: the
//     request itself is malformed, retrying it cannot help.
//   - [ErrMountConflict] -> codes.AlreadyExists: the target is already used by a
//     different volume or with different flags.
//   - [ErrCorruptedMount] and every other error -> codes.Internal: the FUSE
//     process died, the backend failed or the node is in an unexpected state.
//     A corrupted mount is reported so that the caller (Node service, or the
//     startup recovery of T-08) can decide to unmount and mount again.
//
// The package never creates directories: creating the staging and target
// directories belongs to the Node service, because it also owns their lifetime.
// Lazy unmounting uses syscall.Unmount with MNT_DETACH on Linux only, so that the
// package needs no dependency beyond k8s.io/mount-utils; off Linux both
// [Mounter.BindTargets] and [Mounter.Detach] report errors.ErrUnsupported.
// M5 adds a hostMounter that implements the same [Mounter] interface with
// nsenter into the host mount namespace; callers only ever see the interface.
package mounter
