//go:build integration

// Package gluster integration test: it runs a real gluster command over SSH
// against the servers of a live cluster. It is compiled only with
// -tags integration and skipped unless every environment variable below is set.
//
//	GLUSTER_SSH_HOSTS            comma separated GlusterFS servers
//	GLUSTER_SSH_USER             login name on the servers
//	GLUSTER_SSH_KEY_FILE         path of the unencrypted private key
//	GLUSTER_SSH_KNOWN_HOSTS_FILE path of the known_hosts file (ssh-keyscan output)
//	GLUSTER_SSH_PORT             optional, 22 by default
//
// The test only reads: it runs "volume info" twice and never changes the cluster.
package gluster

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// integrationCredentials builds credentials from the environment. It skips the
// test when any of the files or values is missing.
func integrationCredentials(t *testing.T) (*Credentials, []string) {
	t.Helper()

	hostsValue := os.Getenv("GLUSTER_SSH_HOSTS")
	user := os.Getenv("GLUSTER_SSH_USER")
	keyFile := os.Getenv("GLUSTER_SSH_KEY_FILE")
	knownHostsFile := os.Getenv("GLUSTER_SSH_KNOWN_HOSTS_FILE")
	if hostsValue == "" || user == "" || keyFile == "" || knownHostsFile == "" {
		t.Skip("GLUSTER_SSH_HOSTS, GLUSTER_SSH_USER, GLUSTER_SSH_KEY_FILE and GLUSTER_SSH_KNOWN_HOSTS_FILE are required")
	}

	hosts := make([]string, 0, 4)
	for _, host := range strings.Split(hostsValue, ",") {
		if trimmed := strings.TrimSpace(host); trimmed != "" {
			hosts = append(hosts, trimmed)
		}
	}
	if len(hosts) == 0 {
		t.Skip("GLUSTER_SSH_HOSTS lists no host")
	}

	privateKey, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read %s: %v", keyFile, err)
	}
	knownHosts, err := os.ReadFile(knownHostsFile)
	if err != nil {
		t.Fatalf("read %s: %v", knownHostsFile, err)
	}
	port := ""
	if value := os.Getenv("GLUSTER_SSH_PORT"); value != "" {
		if _, convErr := strconv.Atoi(value); convErr != nil {
			t.Fatalf("GLUSTER_SSH_PORT %q is not a number", value)
		}
		port = value
	}

	creds, err := ParseCredentials(map[string]string{
		KeyUsername:   user,
		KeyPrivateKey: string(privateKey),
		KeyKnownHosts: string(knownHosts),
		KeyPort:       port,
	})
	if err != nil {
		t.Fatalf("ParseCredentials: %v", err)
	}
	return creds, hosts
}

func TestIntegrationGlusterVolumeInfo(t *testing.T) {
	creds, hosts := integrationCredentials(t)
	executor, err := NewSSHExecutor(creds, SSHOptions{})
	if err != nil {
		t.Fatalf("NewSSHExecutor: %v", err)
	}
	failover := &Failover{Executor: executor}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The unreachable host comes first, so a successful run proves the failover.
	hosts = append([]string{"nonexistent.invalid"}, hosts...)
	stdout, host, err := failover.Run(ctx, hosts, "gluster", "--mode=script", "--xml", "volume", "info")
	if err != nil {
		t.Fatalf("Run volume info = %v, want nil", err)
	}
	if host == hosts[0] {
		t.Fatalf("the command ran on %q, want any host but the unreachable one", host)
	}
	t.Logf("ran on %q, %d bytes of output", host, len(stdout))
	if !strings.Contains(string(stdout), "<opRet>0</opRet>") {
		t.Fatalf("output does not contain <opRet>0</opRet>: %s", stdout)
	}

	// A volume that does not exist: either the command exits non-zero or it answers
	// with a non-zero opRet. Which one happens depends on the gluster version.
	stdout, host, err = failover.Run(ctx, hosts, "gluster", "--mode=script", "--xml", "volume", "info", "no-such-volume-t15")
	var exitErr *ExitError
	switch {
	case errors.As(err, &exitErr):
		t.Logf("host %q exited with status %d: %s", host, exitErr.Code, exitErr.Stderr)
	case err == nil:
		if strings.Contains(string(stdout), "<opRet>0</opRet>") {
			t.Fatalf("volume info of a missing volume succeeded with opRet 0 on %q: %s", host, stdout)
		}
		t.Logf("host %q answered without an error but with a non-zero opRet: %s", host, stdout)
	default:
		t.Fatalf("volume info of a missing volume = %v, want an ExitError or a non-zero opRet", err)
	}
}
