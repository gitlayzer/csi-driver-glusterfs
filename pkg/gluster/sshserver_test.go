package gluster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// serverMode selects how the test SSH server answers an exec request.
type serverMode int

const (
	// modeHandler calls the configured handler and returns its answer.
	modeHandler serverMode = iota
	// modeShell runs the received command line with /bin/sh -c, which is what an
	// OpenSSH server configured with a POSIX login shell does. It is the mode of
	// the injection test.
	modeShell
	// modeReject rejects the exec request.
	modeReject
	// modeNoExit writes output and closes the channel without an exit-status.
	modeNoExit
	// modeHang reads from the channel until the client closes the connection.
	modeHang
	// modeRejectSession rejects the session channel itself.
	modeRejectSession
	// modeBadExitStatus sends an exit-status request with a payload that is too
	// short for the protocol.
	modeBadExitStatus
)

// testSSHServer is an in-process SSH server. It is a test helper, not code under
// test: it accepts exactly the client keys it was created with and answers exec
// requests according to its mode.
type testSSHServer struct {
	t        *testing.T
	listener net.Listener
	hostKeys []ssh.Signer
	client   ssh.PublicKey
	mode     serverMode
	// handler answers an exec request in modeHandler.
	handler func(command string) (stdout, stderr string, code int)
	// shellEnv and shellDir are used in modeShell.
	shellEnv []string
	shellDir string

	mu       sync.Mutex
	commands []string
	// hangClosed is closed when modeHang observed that the client hung up.
	hangClosed chan struct{}
	hangOnce   sync.Once
	// accepted counts the connections the server accepted, so a test can assert
	// that a rejected command never reached the network.
	accepted int
	wg       sync.WaitGroup
}

// newTestSSHServer starts a server on 127.0.0.1:0 that accepts clientKey and
// closes itself when the test ends.
func newTestSSHServer(t *testing.T, hostKey ssh.Signer, clientKey ssh.PublicKey, mode serverMode) *testSSHServer {
	t.Helper()
	return newTestSSHServerMultiHostKey(t, []ssh.Signer{hostKey}, clientKey, mode)
}

// newTestSSHServerMultiHostKey is [newTestSSHServer] with several host keys, which
// is what an OpenSSH server with more than one HostKey line offers.
func newTestSSHServerMultiHostKey(t *testing.T, hostKeys []ssh.Signer, clientKey ssh.PublicKey, mode ...serverMode) *testSSHServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &testSSHServer{
		t:          t,
		listener:   listener,
		hostKeys:   hostKeys,
		client:     clientKey,
		mode:       modeHandler,
		hangClosed: make(chan struct{}),
	}
	if len(mode) > 0 {
		s.mode = mode[0]
	}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.Close)
	return s
}

// Addr is the address the server listens on.
func (s *testSSHServer) Addr() string { return s.listener.Addr().String() }

// Host is the host name and the port of the listener, as known_hosts writes it.
func (s *testSSHServer) Host() string {
	host, port, err := net.SplitHostPort(s.Addr())
	if err != nil {
		s.t.Fatalf("split %q: %v", s.Addr(), err)
	}
	return net.JoinHostPort(host, port)
}

// Port is the port the server listens on.
func (s *testSSHServer) Port() int {
	_, port, err := net.SplitHostPort(s.Addr())
	if err != nil {
		s.t.Fatalf("split %q: %v", s.Addr(), err)
	}
	n, err := net.LookupPort("tcp", port)
	if err != nil {
		s.t.Fatalf("port %q: %v", port, err)
	}
	return n
}

// KnownHostsLine returns a known_hosts record for the given host name and the
// real port of the listener.
func (s *testSSHServer) KnownHostsLine(host string) string {
	return knownhosts.Line([]string{net.JoinHostPort(host, fmt.Sprintf("%d", s.Port()))}, s.hostKeys[0].PublicKey())
}

// Commands returns the command lines the server received, in order.
func (s *testSSHServer) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// Accepted returns the number of accepted connections.
func (s *testSSHServer) Accepted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

// HangClosed is closed when the server noticed that the client hung up while the
// command was still running.
func (s *testSSHServer) HangClosed() <-chan struct{} { return s.hangClosed }

// Close stops the listener and waits for every connection goroutine.
func (s *testSSHServer) Close() {
	_ = s.listener.Close()
	s.wg.Wait()
}

// accept runs the accept loop until the listener is closed.
func (s *testSSHServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.accepted++
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// handleConn performs the handshake and serves the session channels of one
// connection.
func (s *testSSHServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), s.client.Marshal()) {
				return nil, errors.New("unknown client key")
			}
			return nil, nil
		},
	}
	config.AddHostKey(s.hostKeys[0])
	for _, hostKey := range s.hostKeys[1:] {
		config.AddHostKey(hostKey)
	}

	serverConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = serverConn.Close() }()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if s.mode == modeRejectSession {
			_ = newChannel.Reject(ssh.Prohibited, "no sessions")
			continue
		}
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handleSession(channel, requests)
	}
}

// handleSession answers the exec request of one session channel.
func (s *testSSHServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer s.wg.Done()
	defer func() { _ = channel.Close() }()

	for req := range requests {
		if req.Type != "exec" {
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			req.Reply(false, nil)
			return
		}
		s.record(payload.Command)
		switch s.mode {
		case modeBadExitStatus:
			req.Reply(true, nil)
			_, _ = channel.Write([]byte("output before the broken exit status"))
			_, _ = channel.SendRequest("exit-status", false, []byte{0})
			return
		case modeReject:
			req.Reply(false, nil)
			return
		case modeNoExit:
			req.Reply(true, nil)
			_, _ = channel.Write([]byte("partial output"))
			_, _ = channel.Stderr().Write([]byte("partial error"))
			return
		case modeHang:
			req.Reply(true, nil)
			// The command runs until the client hangs up: the requests channel of
			// the channel is closed with it. Reading the data stream would not do,
			// because the client sends EOF on stdin right after the exec request.
			for range requests {
			}
			s.hangOnce.Do(func() { close(s.hangClosed) })
			return
		case modeShell:
			req.Reply(true, nil)
			s.serveShell(channel, payload.Command)
			return
		case modeHandler:
			req.Reply(true, nil)
			s.serveHandler(channel, payload.Command)
			return
		}
	}
}

// record notes the received command line.
func (s *testSSHServer) record(command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, command)
}

// serveHandler writes the configured answer and the exit status.
func (s *testSSHServer) serveHandler(channel ssh.Channel, command string) {
	stdout, stderr, code := "ok", "", 0
	if s.handler != nil {
		stdout, stderr, code = s.handler(command)
	}
	if stdout != "" {
		_, _ = channel.Write([]byte(stdout))
	}
	if stderr != "" {
		_, _ = channel.Stderr().Write([]byte(stderr))
	}
	sendExitStatus(channel, code)
}

// serveShell executes the command line with /bin/sh -c, which is what an SSH
// server with a POSIX login shell does.
func (s *testSSHServer) serveShell(channel ssh.Channel, command string) {
	// #nosec G204 -- running a shell command line is the point of this test
	// helper: the test asserts that the client quoted every argument.
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = s.shellDir
	cmd.Env = s.shellEnv
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = 127
			fmt.Fprintf(&stderr, "cannot run the command: %v", err)
		}
	}
	if stdout.Len() > 0 {
		_, _ = channel.Write(stdout.Bytes())
	}
	if stderr.Len() > 0 {
		_, _ = channel.Stderr().Write(stderr.Bytes())
	}
	sendExitStatus(channel, code)
}

// sendExitStatus sends the exit status of a finished command.
func sendExitStatus(channel ssh.Channel, code int) {
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
}

// marshalPrivateKey returns the PEM encoding of an ed25519 private key, which is
// what the ssh-privatekey key of a Secret holds.
func marshalPrivateKey(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

// dialFunc maps the fake host names of the tests to the addresses of the test
// servers.
func dialFunc(mapping map[string]string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialer := &net.Dialer{}
		if target, ok := mapping[addr]; ok {
			return dialer.DialContext(ctx, network, target)
		}
		return dialer.DialContext(ctx, network, addr)
	}
}
