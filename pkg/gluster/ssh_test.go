package gluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testSigner is a signer together with the PEM of its private key, so that a test
// credentials set can be built from one key pair.
type testSigner struct {
	ssh.Signer
	pem string
}

// newTestSigner returns a fresh ed25519 signer that remembers its PEM encoding.
func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return &testSigner{Signer: signer, pem: marshalPrivateKey(t, priv)}
}

// knownhostsLineForPort builds a known_hosts record for host and port.
func knownhostsLineForPort(host string, port int, key ssh.PublicKey) string {
	return knownHostsLine(net.JoinHostPort(host, fmt.Sprintf("%d", port)), key)
}

// newTestExecutor returns an executor that maps the fake host names to the
// addresses in mapping.
func newTestExecutor(t *testing.T, creds *Credentials, opts SSHOptions, mapping map[string]string) *SSHExecutor {
	t.Helper()
	opts.Dial = dialFunc(mapping)
	executor, err := NewSSHExecutor(creds, opts)
	if err != nil {
		t.Fatalf("NewSSHExecutor: %v", err)
	}
	return executor
}

// testCredentials builds credentials that authenticate with clientKey, connect to
// port and pin hostKey as the host key of host.
func testCredentials(t *testing.T, hostKey, clientKey *testSigner, host string, port int) *Credentials {
	t.Helper()
	data := map[string]string{
		KeyUsername:   "gluster",
		KeyPrivateKey: clientKey.pem,
		KeyKnownHosts: knownhostsLineForPort(host, port, hostKey.PublicKey()),
		KeyPort:       strconv.Itoa(port),
	}
	creds, err := ParseCredentials(data)
	if err != nil {
		t.Fatalf("ParseCredentials: %v", err)
	}
	return creds
}

func TestNewSSHExecutor(t *testing.T) {
	t.Parallel()

	if _, err := NewSSHExecutor(nil, SSHOptions{}); err == nil {
		t.Fatal("NewSSHExecutor(nil) = nil error, want an error")
	}
	creds := &Credentials{User: "gluster", Port: 22}
	if _, err := NewSSHExecutor(creds, SSHOptions{}); err == nil {
		t.Fatal("NewSSHExecutor(incomplete credentials) = nil error, want an error")
	}
	client := newTestSigner(t)
	data := map[string]string{
		KeyUsername:   "gluster",
		KeyPrivateKey: client.pem,
		KeyKnownHosts: knownhostsLineForPort("gluster-1", 22, client.PublicKey()),
	}
	parsed, err := ParseCredentials(data)
	if err != nil {
		t.Fatalf("ParseCredentials = %v", err)
	}
	executor, err := NewSSHExecutor(parsed, SSHOptions{})
	if err != nil {
		t.Fatalf("NewSSHExecutor = %v, want nil", err)
	}
	if executor.dialTimeout != DefaultDialTimeout {
		t.Errorf("dialTimeout = %v, want %v", executor.dialTimeout, DefaultDialTimeout)
	}
	if executor.maxOutput != DefaultMaxOutput {
		t.Errorf("maxOutput = %d, want %d", executor.maxOutput, DefaultMaxOutput)
	}
	if executor.dial == nil {
		t.Error("dial = nil, want the default dialer")
	}
}

func TestSSHExecutorRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		host       string
		args       []string
		handler    func(string) (string, string, int)
		wantStdout string
		wantExit   int
	}{
		{
			name: "successful command returns stdout verbatim",
			host: "gluster-1",
			args: []string{"gluster", "--mode=script", "--xml", "volume", "info"},
			handler: func(string) (string, string, int) {
				return "<cliOutput><opRet>0</opRet></cliOutput>\n", "", 0
			},
			wantStdout: "<cliOutput><opRet>0</opRet></cliOutput>\n",
		},
		{
			name: "empty stdout",
			host: "gluster-1",
			args: []string{"gluster", "volume", "list"},
			handler: func(string) (string, string, int) {
				return "", "", 0
			},
			wantStdout: "",
		},
		{
			name: "non-zero exit becomes an ExitError",
			host: "gluster-1",
			args: []string{"gluster", "volume", "info", "missing"},
			handler: func(string) (string, string, int) {
				return "", "volume missing not found\n", 1
			},
			wantExit: 1,
		},
		{
			name: "large exit code",
			host: "gluster-1",
			args: []string{"gluster"},
			handler: func(string) (string, string, int) {
				return "", "boom", 42
			},
			wantExit: 42,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hostKey := newTestSigner(t)
			clientKey := newTestSigner(t)
			server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
			server.handler = tc.handler
			creds := testCredentials(t, hostKey, clientKey, tc.host, server.Port())
			executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
				net.JoinHostPort(tc.host, fmt.Sprintf("%d", server.Port())): server.Addr(),
			})

			stdout, err := executor.Run(context.Background(), tc.host, tc.args...)
			if tc.wantExit != 0 {
				var exitErr *ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("Run = %v, want *ExitError", err)
				}
				if exitErr.Code != tc.wantExit {
					t.Errorf("exit code = %d, want %d", exitErr.Code, tc.wantExit)
				}
				if exitErr.Host != tc.host {
					t.Errorf("Host = %q, want %q", exitErr.Host, tc.host)
				}
				if exitErr.Program != tc.args[0] {
					t.Errorf("Program = %q, want %q", exitErr.Program, tc.args[0])
				}
				if errors.Is(err, ErrConnect) {
					t.Error("an ExitError must not wrap ErrConnect")
				}
				return
			}
			if err != nil {
				t.Fatalf("Run = %v, want nil", err)
			}
			if string(stdout) != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, tc.wantStdout)
			}
			if got := len(server.Commands()); got != 1 {
				t.Fatalf("the server received %d commands, want 1", got)
			}
			if want := shellQuote(tc.args); server.Commands()[0] != want {
				t.Errorf("received command = %q, want %q", server.Commands()[0], want)
			}
		})
	}
}

func TestSSHExecutorExitErrorStderr(t *testing.T) {
	t.Parallel()

	const stderr = "cannot connect to the glusterd on this host\n"
	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
	server.handler = func(string) (string, string, int) { return "", stderr, 1 }
	creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
	})

	_, err := executor.Run(context.Background(), "gluster-1", "gluster", "volume", "info")
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run = %v, want *ExitError", err)
	}
	if string(exitErr.Stderr) != stderr {
		t.Errorf("Stderr = %q, want %q", exitErr.Stderr, stderr)
	}
	if want := "gluster on gluster-1 exited with status 1: cannot connect to the glusterd on this host"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

// TestSSHExecutorInjection is the central security test: the test server runs the
// received command line with /bin/sh -c, exactly like an SSH server with a POSIX
// login shell, and a fake gluster in PATH prints every argument it received
// separated by NUL bytes.
func TestSSHExecutorInjection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the injection test needs /bin/sh")
	}

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("create %s: %v", binDir, err)
	}
	fakeGluster := filepath.Join(binDir, "gluster")
	// #nosec G306 -- the script has to be executable, it lives in a temp dir.
	if err := os.WriteFile(fakeGluster, []byte("#!/bin/sh\nfor a; do printf '%s\\0' \"$a\"; done\n"), 0o755); err != nil {
		t.Fatalf("write the fake gluster: %v", err)
	}

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeShell)
	server.shellDir = dir
	server.shellEnv = []string{"PATH=" + binDir + ":/usr/bin:/bin", "HOME=" + dir}

	creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
	})

	injections := []string{
		"a b",
		"'",
		"''",
		`"`,
		"$(touch PWNED)",
		"`touch PWNED`",
		";touch PWNED",
		"|touch PWNED",
		"&touch PWNED",
		">out",
		"*",
		"~",
		`\`,
		"!",
		"$HOME",
		"${IFS}",
		"",
		"卷",
		"$(id > PWNED)",
		"'; touch PWNED; '",
	}
	args := append([]string{"gluster", "--mode=script", "--xml"}, injections...)

	stdout, err := executor.Run(context.Background(), "gluster-1", args...)
	if err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}

	got := strings.Split(string(stdout), "\x00")
	// A trailing NUL byte produces one empty field at the end.
	if len(got) > 0 && got[len(got)-1] == "" {
		got = got[:len(got)-1]
	}
	want := args[1:]
	if len(got) != len(want) {
		t.Fatalf("the fake gluster received %d arguments %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argument %d = %q, want %q", i, got[i], want[i])
		}
	}

	for _, name := range []string{"PWNED", "out"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was created in the working directory: an argument was interpreted by the shell", name)
		}
	}
}

func TestSSHExecutorRejectsInvalidCommandsWithoutConnecting(t *testing.T) {
	t.Parallel()

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
	creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
	})

	tests := []struct {
		name string
		host string
		args []string
	}{
		{name: "empty host", host: "", args: []string{"gluster"}},
		{name: "host with a space", host: "gluster 1", args: []string{"gluster"}},
		{name: "host with at sign", host: "root@gluster-1", args: []string{"gluster"}},
		{name: "host with a newline", host: "gluster-1\n", args: []string{"gluster"}},
		{name: "no arguments", host: "gluster-1", args: nil},
		{name: "program sh", host: "gluster-1", args: []string{"sh", "-c", "id"}},
		{name: "program with an absolute path", host: "gluster-1", args: []string{"/usr/sbin/gluster"}},
		{name: "program with a separator", host: "gluster-1", args: []string{"gluster;id"}},
		{name: "argument with a newline", host: "gluster-1", args: []string{"gluster", "a\nb"}},
		{name: "argument with a carriage return", host: "gluster-1", args: []string{"gluster", "a\rb"}},
		{name: "argument with a NUL byte", host: "gluster-1", args: []string{"gluster", "a\x00b"}},
		{name: "argument with an escape character", host: "gluster-1", args: []string{"gluster", "\x1b[31m"}},
		{name: "argument that is not valid UTF-8", host: "gluster-1", args: []string{"gluster", "\xff"}},
		{name: "command above the length limit", host: "gluster-1",
			args: []string{"gluster", strings.Repeat("a", MaxCommandLength)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := executor.Run(context.Background(), tc.host, tc.args...)
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("Run(%q, %q) = %v, want ErrInvalidCommand", tc.host, tc.args, err)
			}
			if errors.Is(err, ErrConnect) {
				t.Fatalf("Run(%q, %q) wraps ErrConnect", tc.host, tc.args)
			}
			if accepted := server.Accepted(); accepted != 0 {
				t.Fatalf("the server accepted %d connections, want 0", accepted)
			}
			if commands := server.Commands(); len(commands) != 0 {
				t.Fatalf("the server received %q, want nothing", commands)
			}
		})
	}
}

func TestSSHExecutorHostKeyVerification(t *testing.T) {
	t.Parallel()

	t.Run("matching host key succeeds", func(t *testing.T) {
		t.Parallel()
		hostKey := newTestSigner(t)
		clientKey := newTestSigner(t)
		server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
		creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
		executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
			net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
		})
		if _, err := executor.Run(context.Background(), "gluster-1", "gluster", "volume", "list"); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	})

	t.Run("unknown host is rejected", func(t *testing.T) {
		t.Parallel()
		hostKey := newTestSigner(t)
		clientKey := newTestSigner(t)
		server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
		// The known_hosts record is for another host name, so the address of the
		// connection is not in it.
		creds := testCredentials(t, hostKey, clientKey, "gluster-2", server.Port())
		executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
			net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
		})
		_, err := executor.Run(context.Background(), "gluster-1", "gluster")
		if !errors.Is(err, ErrConnect) {
			t.Fatalf("Run = %v, want ErrConnect", err)
		}
		if !strings.Contains(err.Error(), "not in known_hosts") {
			t.Errorf("error %q does not say that the host is unknown", err)
		}
		if accepted := server.Accepted(); accepted != 1 {
			t.Errorf("the server accepted %d connections, want 1", accepted)
		}
	})

	t.Run("mismatched host key is rejected", func(t *testing.T) {
		t.Parallel()
		hostKey := newTestSigner(t)
		clientKey := newTestSigner(t)
		otherKey := newTestSigner(t)
		server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
		// known_hosts pins another key for the same host and port.
		data := map[string]string{
			KeyUsername:   "gluster",
			KeyPrivateKey: clientKey.pem,
			KeyKnownHosts: knownhostsLineForPort("gluster-1", server.Port(), otherKey.PublicKey()),
			KeyPort:       strconv.Itoa(server.Port()),
		}
		creds, err := ParseCredentials(data)
		if err != nil {
			t.Fatalf("ParseCredentials = %v", err)
		}
		executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
			net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
		})
		_, err = executor.Run(context.Background(), "gluster-1", "gluster")
		if !errors.Is(err, ErrConnect) {
			t.Fatalf("Run = %v, want ErrConnect", err)
		}
		if !strings.Contains(err.Error(), "does not match known_hosts") {
			t.Errorf("error %q does not report the mismatch", err)
		}
		if !strings.Contains(err.Error(), "man-in-the-middle") {
			t.Errorf("error %q does not warn about a man-in-the-middle attack", err)
		}
	})

	t.Run("only the pinned key type is negotiated", func(t *testing.T) {
		t.Parallel()
		// The server offers an ecdsa key first and an ed25519 key second;
		// known_hosts only pins the ed25519 key, so the executor has to ask for
		// ed25519 and must not fail with a host key mismatch.
		ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate ecdsa key: %v", err)
		}
		ecdsaSigner, err := ssh.NewSignerFromKey(ecdsaKey)
		if err != nil {
			t.Fatalf("ecdsa signer: %v", err)
		}
		edSigner := newTestSigner(t)
		clientKey := newTestSigner(t)

		server := newTestSSHServerMultiHostKey(t, []ssh.Signer{ecdsaSigner, edSigner.Signer}, clientKey.PublicKey())
		data := map[string]string{
			KeyUsername:   "gluster",
			KeyPrivateKey: clientKey.pem,
			KeyKnownHosts: knownhostsLineForPort("gluster-1", server.Port(), edSigner.PublicKey()),
			KeyPort:       strconv.Itoa(server.Port()),
		}
		creds, err := ParseCredentials(data)
		if err != nil {
			t.Fatalf("ParseCredentials = %v", err)
		}
		if got := strings.Join(creds.hostKeyAlgorithms, ","); got != "ssh-ed25519" {
			t.Fatalf("hostKeyAlgorithms = %q, want ssh-ed25519", got)
		}
		executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
			net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
		})
		if _, err := executor.Run(context.Background(), "gluster-1", "gluster"); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	})
}

func TestSSHExecutorAuthenticationFailure(t *testing.T) {
	t.Parallel()

	hostKey := newTestSigner(t)
	acceptedKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, acceptedKey.PublicKey(), modeHandler)
	creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
	})

	_, err := executor.Run(context.Background(), "gluster-1", "gluster")
	if !errors.Is(err, ErrConnect) {
		t.Fatalf("Run = %v, want ErrConnect", err)
	}
	if !strings.Contains(err.Error(), "rejected the ssh key") {
		t.Errorf("error %q does not report the rejected key", err)
	}
	if commands := server.Commands(); len(commands) != 0 {
		t.Errorf("the server received %q, want nothing", commands)
	}
}

func TestSSHExecutorExecRejected(t *testing.T) {
	t.Parallel()

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeReject)
	creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
	})

	_, err := executor.Run(context.Background(), "gluster-1", "gluster")
	if !errors.Is(err, ErrConnect) {
		t.Fatalf("Run = %v, want ErrConnect", err)
	}
	if !strings.Contains(err.Error(), "rejected the exec request") {
		t.Errorf("error %q does not report the rejected exec request", err)
	}
}

func TestSSHExecutorConnectionClosedWithoutExitStatus(t *testing.T) {
	t.Parallel()

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeNoExit)
	creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
	})

	stdout, err := executor.Run(context.Background(), "gluster-1", "gluster")
	if err == nil {
		t.Fatalf("Run = %v, nil, want an error", stdout)
	}
	if errors.Is(err, ErrConnect) {
		t.Fatalf("Run = %v, must not wrap ErrConnect: the command did run", err)
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("Run = %v, must not be an ExitError", err)
	}
	if !strings.Contains(err.Error(), "closed before") {
		t.Errorf("error %q does not report the closed connection", err)
	}
}

func TestSSHExecutorOutputLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output int
	}{
		{name: "output within the ssh window", output: 4096},
		// More than the 2 MiB flow control window of the ssh package: the remote
		// command blocks in its write unless the executor closes the connection.
		{name: "output beyond the ssh window", output: 8 << 20},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hostKey := newTestSigner(t)
			clientKey := newTestSigner(t)
			server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
			server.handler = func(string) (string, string, int) {
				return strings.Repeat("x", tc.output), "", 0
			}
			creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
			executor := newTestExecutor(t, creds, SSHOptions{MaxOutput: 512}, map[string]string{
				net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
			})

			done := make(chan error, 1)
			go func() {
				// context.Background on purpose: the limit alone has to end the call.
				_, err := executor.Run(context.Background(), "gluster-1", "gluster")
				done <- err
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return after the output limit was exceeded")
			}
			if !errors.Is(err, errOutputLimit) {
				t.Fatalf("Run = %v, want an error about the output limit", err)
			}
			if errors.Is(err, ErrConnect) {
				t.Fatalf("Run = %v, must not wrap ErrConnect", err)
			}
			if !strings.Contains(err.Error(), "512") {
				t.Errorf("error %q does not mention the limit", err)
			}
		})
	}
}

func TestSSHExecutorContextCancellation(t *testing.T) {
	t.Parallel()

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHang)
	creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := executor.Run(ctx, "gluster-1", "gluster")
		done <- err
	}()

	// Give the command a moment to start, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want errors.Is(err, context.Canceled)", err)
		}
		if errors.Is(err, ErrConnect) {
			t.Fatalf("Run = %v, must not wrap ErrConnect", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of the cancellation")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Run took %v, want less than 2s", elapsed)
	}

	select {
	case <-server.HangClosed():
	case <-time.After(2 * time.Second):
		t.Fatal("the server did not observe that the channel was closed")
	}
}

// TestSSHExecutorCancellationJoinsTheWaitGoroutine checks that Run does not
// return before the goroutine that waits for the session has finished. The
// connection keeps its reader busy for a while after it was closed, so a Run that
// does not join the goroutine returns while the goroutine is still there. The
// test is not parallel on purpose: no other test runs an executor while it
// inspects the goroutines, so a goroutine in SSHExecutor.wait can only be one
// this test leaked.
func TestSSHExecutorCancellationJoinsTheWaitGoroutine(t *testing.T) {
	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHang)
	creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
	dial := dialFunc(map[string]string{
		net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
	})
	executor, err := NewSSHExecutor(creds, SSHOptions{
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &slowCloseConn{Conn: conn, closed: make(chan struct{})}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewSSHExecutor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for len(server.Commands()) == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := executor.Run(ctx, "gluster-1", "gluster"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want errors.Is(err, context.Canceled)", err)
	}
	if stacks := waitGoroutineStacks(); stacks != "" {
		t.Fatalf("a goroutine of SSHExecutor.wait outlived Run:\n%s", stacks)
	}
}

// slowCloseDelay is how long a [slowCloseConn] delays a failed read after Close.
const slowCloseDelay = 300 * time.Millisecond

// slowCloseConn is a connection whose reads fail only slowCloseDelay after it was
// closed, which keeps the ssh reader goroutine, and with it session.Wait, alive
// for that long.
type slowCloseConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *slowCloseConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *slowCloseConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		select {
		case <-c.closed:
			time.Sleep(slowCloseDelay)
		default:
		}
	}
	return n, err
}

// waitGoroutineStacks returns the stacks of the goroutines that are running the
// session.Wait closure of SSHExecutor.wait, or "" if there are none.
func waitGoroutineStacks() string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var leaked []string
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(stack, "(*SSHExecutor).wait.func") {
			leaked = append(leaked, stack)
		}
	}
	return strings.Join(leaked, "\n\n")
}

func TestSSHExecutorDialTimeout(t *testing.T) {
	t.Parallel()

	// A listener that accepts connections and never sends an SSH banner.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	var (
		wg       sync.WaitGroup
		accepted = make(chan net.Conn, 8)
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
		close(accepted)
		for conn := range accepted {
			_ = conn.Close()
		}
	})

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	host := "gluster-1"
	port := listener.Addr().(*net.TCPAddr).Port
	creds := testCredentials(t, hostKey, clientKey, host, port)
	executor := newTestExecutor(t, creds, SSHOptions{DialTimeout: 200 * time.Millisecond}, map[string]string{
		net.JoinHostPort(host, fmt.Sprintf("%d", port)): listener.Addr().String(),
	})

	start := time.Now()
	_, err = executor.Run(context.Background(), host, "gluster")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrConnect) {
		t.Fatalf("Run = %v, want ErrConnect", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v, want less than 2s", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("Run returned after %v, want at least the dial timeout", elapsed)
	}
}

func TestSSHExecutorCancelledBeforeTheCall(t *testing.T) {
	t.Parallel()

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
	creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := executor.Run(ctx, "gluster-1", "gluster")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want errors.Is(err, context.Canceled)", err)
	}
	if errors.Is(err, ErrConnect) {
		t.Fatalf("Run = %v, must not wrap ErrConnect", err)
	}
	if accepted := server.Accepted(); accepted != 0 {
		t.Errorf("the server accepted %d connections, want 0", accepted)
	}
}

func TestSSHExecutorCancelledDuringTheHandshake(t *testing.T) {
	t.Parallel()

	// A listener that never answers, so the handshake blocks until the context is
	// cancelled.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns []net.Conn
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	host := "gluster-1"
	port := listener.Addr().(*net.TCPAddr).Port
	creds := testCredentials(t, hostKey, clientKey, host, port)
	executor := newTestExecutor(t, creds, SSHOptions{DialTimeout: time.Minute}, map[string]string{
		net.JoinHostPort(host, fmt.Sprintf("%d", port)): listener.Addr().String(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = executor.Run(ctx, host, "gluster")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
	if errors.Is(err, ErrConnect) {
		t.Fatalf("Run = %v, must not wrap ErrConnect: a cancellation is not a host failure", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Run took %v, want less than 2s", elapsed)
	}
}

func TestSSHExecutorConcurrentRuns(t *testing.T) {
	t.Parallel()

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
	server.handler = func(command string) (string, string, int) {
		return command, "", 0
	}
	creds := testCredentials(t, hostKey, clientKey, "gluster-1", server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort("gluster-1", fmt.Sprintf("%d", server.Port())): server.Addr(),
	})

	const runs = 20
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args := []string{"gluster", "volume", "info", fmt.Sprintf("vol-%d", i)}
			stdout, err := executor.Run(context.Background(), "gluster-1", args...)
			if err != nil {
				errs <- fmt.Errorf("run %d: %w", i, err)
				return
			}
			if want := shellQuote(args); string(stdout) != want {
				errs <- fmt.Errorf("run %d: stdout = %q, want %q", i, stdout, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := len(server.Commands()); got != runs {
		t.Errorf("the server received %d commands, want %d", got, runs)
	}
}

func TestSSHExecutorDeadlineErrors(t *testing.T) {
	t.Parallel()

	clientKey := newTestSigner(t)
	hostKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
	const host = "gluster-1"
	creds := testCredentials(t, hostKey, clientKey, host, server.Port())

	// connect calls SetDeadline twice: once to bound the handshake and once to
	// clear the deadline afterwards. Both calls have to be handled.
	for _, tc := range []struct {
		name   string
		failAt int
		want   string
	}{
		{name: "setting the deadline fails", failAt: 1, want: "cannot set a deadline"},
		{name: "clearing the deadline fails", failAt: 2, want: "cannot clear the deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			executor, err := NewSSHExecutor(creds, SSHOptions{
				Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
					dialer := &net.Dialer{}
					conn, dialErr := dialer.DialContext(ctx, network, server.Addr())
					if dialErr != nil {
						return nil, dialErr
					}
					return &nthDeadlineConn{Conn: conn, failAt: tc.failAt, err: errors.New("deadline not supported")}, nil
				},
			})
			if err != nil {
				t.Fatalf("NewSSHExecutor: %v", err)
			}
			_, err = executor.Run(context.Background(), host, "gluster")
			if !errors.Is(err, ErrConnect) {
				t.Fatalf("Run = %v, want ErrConnect", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// nthDeadlineConn fails the SetDeadline call with the configured index.
type nthDeadlineConn struct {
	net.Conn
	failAt int
	calls  int
	err    error
}

func (c *nthDeadlineConn) SetDeadline(t time.Time) error {
	c.calls++
	if c.calls == c.failAt {
		return c.err
	}
	return c.Conn.SetDeadline(t)
}

// TestSSHExecutorSessionOpenRejected covers the failure to open a session channel,
// which happens before the remote command can start.
func TestSSHExecutorSessionOpenRejected(t *testing.T) {
	t.Parallel()

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeRejectSession)
	const host = "gluster-1"
	creds := testCredentials(t, hostKey, clientKey, host, server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort(host, strconv.Itoa(server.Port())): server.Addr(),
	})

	_, err := executor.Run(context.Background(), host, "gluster")
	if !errors.Is(err, ErrConnect) {
		t.Fatalf("Run = %v, want ErrConnect", err)
	}
	if !strings.Contains(err.Error(), "cannot open an ssh session") {
		t.Errorf("error %q does not report the session problem", err)
	}
}

func TestSSHExecutorBrokenExitStatus(t *testing.T) {
	t.Parallel()

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeBadExitStatus)
	const host = "gluster-1"
	creds := testCredentials(t, hostKey, clientKey, host, server.Port())
	executor := newTestExecutor(t, creds, SSHOptions{}, map[string]string{
		net.JoinHostPort(host, strconv.Itoa(server.Port())): server.Addr(),
	})

	stdout, err := executor.Run(context.Background(), host, "gluster")
	if err == nil {
		t.Fatalf("Run = %q, nil, want an error", stdout)
	}
	if errors.Is(err, ErrConnect) {
		t.Fatalf("Run = %v, must not wrap ErrConnect: the command did run", err)
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("Run = %v, must not be an ExitError", err)
	}
	if !strings.Contains(err.Error(), "failed after the command started") {
		t.Errorf("error %q does not report the failure after the start", err)
	}
}
