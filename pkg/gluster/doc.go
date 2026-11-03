// Package gluster runs gluster management commands on the GlusterFS servers of
// a cluster.
//
// The driver does not ship a gluster CLI and does not run it locally: quota,
// volume and (later) snapshot operations are executed on one of the GlusterFS
// servers over SSH, because every glusterd can run cluster wide management
// commands (docs/architecture.md section 5.2, D-34). This package is the transport
// only:
//
//   - [Executor] is the interface: run an argv on one host and return stdout.
//   - [NewSSHExecutor] is the MVP implementation, built on
//     golang.org/x/crypto/ssh. It opens one connection per call, verifies the
//     host key against the known_hosts of the credentials and authenticates
//     with a single unencrypted private key.
//   - [Failover] runs a command on the first reachable server of a cluster and
//     remembers which one worked last.
//   - [ParseCredentials] turns the data of an SSH Secret into [Credentials].
//
// The gluster subcommands and their XML output belong to the next layer, which
// only ever calls [Executor.Run] with an argv like
// {"gluster", "--mode=script", "--xml", "volume", "info"}.
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
// that is rejected before any connection is made. A remote command that ran and
// failed is reported as an [ExitError], which never moves to another host.
// Cancellation and timeouts of the caller's context are reported through
// errors.Is with ctx.Err().
package gluster
