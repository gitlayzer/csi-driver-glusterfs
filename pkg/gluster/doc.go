// Package gluster runs gluster management commands on the GlusterFS servers of
// a cluster.
//
// The driver does not ship a gluster CLI and does not run it locally: quota,
// volume and (later) snapshot operations are executed on one of the GlusterFS
// servers over SSH, because every glusterd can run cluster wide management
// commands (docs/architecture.md section 5.2, D-34). The package has two layers:
//
//   - [Executor] is the interface: run an argv on one host and return stdout.
//   - [NewSSHExecutor] is the MVP implementation, built on
//     golang.org/x/crypto/ssh. It opens one connection per call, verifies the
//     host key against the known_hosts of the credentials and authenticates
//     with a single unencrypted private key.
//   - [Failover] runs a command on the first reachable server of a cluster and
//     remembers which one worked last.
//   - [ParseCredentials] turns the data of an SSH Secret into [Credentials].
//   - [Client] wraps the gluster subcommands the driver needs (volume info,
//     status, create, start, stop, delete, set and the quota commands) on top of
//     [Failover] and parses their --xml answers.
//
// # The gluster client
//
// [Client] always runs `gluster --mode=script --xml <subcommand> ...` and decides
// success from the <opRet> of the answer, never from the exit status of the
// process: gluster reports almost every failure with exit status 0 and opRet -1
// (or -2), while `volume quota <vol> list` exits with status 2 when quota is
// disabled and still prints a complete answer. Failures are classified only by the
// text of <opErrstr>, with patterns that are anchored at both ends, because the
// errno is the generic 30800 for nearly everything. The result is a sentinel such
// as [ErrVolumeNotFound] or [ErrVolumeExists], or a [CommandError] that carries the
// op, the volume, the host, the opRet, the errno and the cleaned errstr.
//
// The operations that only move a volume into a state are idempotent: starting a
// started volume, stopping a stopped one, enabling quota twice and removing a limit
// that is already gone all return nil (D-35). Everything else reports the state it
// found, and mapping it to a CSI error code is the job of the RPC layer.
//
// When the CLI answers that it cannot reach the local glusterd (see [daemonCheck]),
// the command never reached the cluster, so the error wraps [ErrConnect] and
// [Failover] moves on to the next server.
//
// # Security properties
//
// The remote command line is built by [shellQuote]: every argument is a single
// quoted POSIX shell word, so no argument can be interpreted by the remote
// shell. The program name is checked against a small allowlist ("gluster"),
// arguments may not contain control characters and the quoted command line is
// bounded. The driver never adds a prefix such as sudo, env or exec: raising
// privileges for a non root account is the job of a wrapper script plugged in
// through the authorized_keys command= option (T-19).
//
// Host keys are always verified against the known_hosts of the credentials.
// There is no way to skip the check, to trust the key on first use, to
// authenticate with a password or to use ssh-agent. Because the remote command
// starts in the login shell of the account, that shell has to be POSIX
// compatible (bash, dash, zsh, sh) unless the wrapper of T-19 is configured; the
// single quoting used here is understood by all of them.
//
// # Failures
//
// A command either never runs remotely or it runs once: [ErrConnect] marks the
// failures that happen before the remote command starts, and it is the only
// error [Failover] retries on another host. [ErrInvalidCommand] marks a command
// that is rejected before any connection is made, which is also where every value
// that [Client] puts into an argv (volume name, brick host and path, quota
// directory, option name and value, quota size) is validated. A remote command that
// ran and failed is reported as an [ExitError], which never moves to another host,
// or as a [CommandError] when it answered with a non-zero opRet. Cancellation and
// timeouts of the caller's context are reported through errors.Is with ctx.Err().
package gluster
