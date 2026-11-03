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
//	GLUSTER_IT_BRICK_ROOT        optional, directory of the brick on every server,
//	                             enables the create/delete lifecycle test
//
// TestIntegrationGlusterVolumeInfo only reads. TestIntegrationClientLifecycle creates
// one volume whose name is unique, exercises every method of the Client against it and
// deletes the volume again; it needs GLUSTER_IT_BRICK_ROOT and exactly three servers and
// skips otherwise. gluster never removes the brick directories of a deleted volume and
// this package cannot remove directories, so the test logs the paths that have to be
// cleaned up by hand.
package gluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// integrationClientCredentials builds a Client for the servers of the environment. It
// skips the test unless the three servers and the brick root directory are configured.
func integrationClientCredentials(t *testing.T) (*Client, []string, string) {
	t.Helper()

	creds, hosts := integrationCredentials(t)
	brickRoot := strings.TrimSpace(os.Getenv("GLUSTER_IT_BRICK_ROOT"))
	if brickRoot == "" {
		t.Skip("GLUSTER_IT_BRICK_ROOT is required")
	}
	if len(hosts) != 3 {
		t.Skipf("GLUSTER_SSH_HOSTS lists %d servers, the lifecycle test needs exactly 3", len(hosts))
	}

	executor, err := NewSSHExecutor(creds, SSHOptions{})
	if err != nil {
		t.Fatalf("NewSSHExecutor: %v", err)
	}
	client, err := NewClient(executor, hosts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client, hosts, brickRoot
}

// randomVolumeName returns "t16-it-" followed by 8 hexadecimal digits.
func randomVolumeName(t *testing.T) string {
	t.Helper()

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("random volume name: %v", err)
	}
	return "t16-it-" + hex.EncodeToString(suffix)
}

// TestIntegrationClientLifecycle runs every method of the Client against a real
// three node cluster: it creates one volume, starts it, sets an option, enables quota,
// sets and removes a limit, stops it and deletes it again.
//
// The volume name is unique, so the test does not collide with another run, and the
// cleanup stops and deletes the volume even when an assertion fails. It has to be run
// from inside the cluster (the architect runs it in a temporary pod), not from a
// workstation: no test in this repository talks to a live cluster on its own.
func TestIntegrationClientLifecycle(t *testing.T) {
	client, hosts, brickRoot := integrationClientCredentials(t)
	name := randomVolumeName(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	bricks := make([]Brick, 0, len(hosts))
	brickPaths := make([]string, 0, len(hosts))
	for _, host := range hosts {
		brickPath := strings.TrimSuffix(brickRoot, "/") + "/" + name
		bricks = append(bricks, Brick{Host: host, Path: brickPath})
		brickPaths = append(brickPaths, host+":"+brickPath)
	}
	t.Logf("creating volume %q with bricks %v", name, brickPaths)

	// gluster never deletes the brick directories of a volume, and this package does
	// not create or remove directories, so they are reported for a manual cleanup.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if cleanupErr := client.VolumeStop(cleanupCtx, name); cleanupErr != nil {
			t.Logf("cleanup: VolumeStop(%q) = %v", name, cleanupErr)
		}
		if cleanupErr := client.VolumeDelete(cleanupCtx, name); cleanupErr != nil {
			t.Logf("cleanup: VolumeDelete(%q) = %v", name, cleanupErr)
		}
		for _, brick := range brickPaths {
			t.Logf("cleanup: the brick directory of %s has to be removed by hand", brick)
		}
	})

	// A fresh name: the volume has to be reported as missing.
	_, err := client.VolumeInfo(ctx, name)
	if !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("VolumeInfo of a new name = %v, want ErrVolumeNotFound", err)
	}

	// Create, then repeat: the volume exists now.
	if err = client.VolumeCreate(ctx, CreateOptions{Name: name, Replica: 3, Bricks: bricks}); err != nil {
		t.Fatalf("VolumeCreate = %v, want nil", err)
	}
	if err = client.VolumeCreate(ctx, CreateOptions{Name: name, Replica: 3, Bricks: bricks}); !errors.Is(err, ErrVolumeExists) {
		t.Fatalf("the second VolumeCreate = %v, want ErrVolumeExists", err)
	}

	volume, err := client.VolumeInfo(ctx, name)
	if err != nil {
		t.Fatalf("VolumeInfo = %v, want nil", err)
	}
	if volume.Status != VolumeCreated {
		t.Errorf("Status = %q, want %q", volume.Status, VolumeCreated)
	}
	if volume.ReplicaCount != 3 {
		t.Errorf("ReplicaCount = %d, want 3", volume.ReplicaCount)
	}
	if len(volume.Bricks) != 3 {
		t.Fatalf("Bricks = %d, want 3", len(volume.Bricks))
	}
	for i, brick := range volume.Bricks {
		if brick.Host != bricks[i].Host {
			t.Errorf("Bricks[%d].Host = %q, want %q", i, brick.Host, bricks[i].Host)
		}
		if brick.Path != bricks[i].Path {
			t.Errorf("Bricks[%d].Path = %q, want %q", i, brick.Path, bricks[i].Path)
		}
	}

	// A volume that is not started has no brick process.
	_, err = client.VolumeStatus(ctx, name)
	if !errors.Is(err, ErrVolumeNotStarted) {
		t.Fatalf("VolumeStatus before the start = %v, want ErrVolumeNotStarted", err)
	}

	// Start twice: the second call has to be idempotent.
	if err = client.VolumeStart(ctx, name); err != nil {
		t.Fatalf("VolumeStart = %v, want nil", err)
	}
	if err = client.VolumeStart(ctx, name); err != nil {
		t.Fatalf("the second VolumeStart = %v, want nil", err)
	}

	brickStatuses, err := client.VolumeStatus(ctx, name)
	if err != nil {
		t.Fatalf("VolumeStatus = %v, want nil", err)
	}
	if len(brickStatuses) != 3 {
		t.Fatalf("VolumeStatus = %d bricks, want 3", len(brickStatuses))
	}
	for i, brick := range brickStatuses {
		if !brick.Online {
			t.Errorf("bricks[%d].Online = false, want true", i)
		}
		if brick.Port <= 0 {
			t.Errorf("bricks[%d].Port = %d, want > 0", i, brick.Port)
		}
		if brick.SizeTotal == 0 {
			t.Errorf("bricks[%d].SizeTotal = 0, want > 0", i)
		}
	}

	// A known option is accepted and shows up in the volume info.
	if err = client.VolumeSet(ctx, name, "performance.cache-size", "64MB"); err != nil {
		t.Fatalf("VolumeSet = %v, want nil", err)
	}
	volume, err = client.VolumeInfo(ctx, name)
	if err != nil {
		t.Fatalf("VolumeInfo after VolumeSet = %v, want nil", err)
	}
	if cached := volume.Options["performance.cache-size"]; cached != "64MB" {
		t.Errorf("Options[performance.cache-size] = %q, want 64MB", cached)
	}

	// An option that does not exist in this release.
	if err = client.VolumeSet(ctx, name, "no.such.option", "x"); !errors.Is(err, ErrUnknownOption) {
		t.Fatalf("VolumeSet with an unknown option = %v, want ErrUnknownOption", err)
	}

	// Quota is disabled at this point. On a real cluster this is the path where the
	// CLI exits with status 2 and still prints a complete XML answer.
	if _, err = client.QuotaList(ctx, name); !errors.Is(err, ErrQuotaNotEnabled) {
		t.Fatalf("QuotaList before the enable = %v, want ErrQuotaNotEnabled", err)
	}
	if err = client.QuotaEnable(ctx, name); err != nil {
		t.Fatalf("QuotaEnable = %v, want nil", err)
	}
	if err = client.QuotaEnable(ctx, name); err != nil {
		t.Fatalf("the second QuotaEnable = %v, want nil", err)
	}
	limits, err := client.QuotaList(ctx, name)
	if err != nil {
		t.Fatalf("QuotaList after the enable = %v, want nil", err)
	}
	if len(limits) != 0 {
		t.Fatalf("QuotaList after the enable = %v, want an empty result", quotaPaths(limits))
	}

	// A limit on the root of the volume, then check it, then remove it twice.
	if err = client.QuotaLimitUsage(ctx, name, "/", 1<<30); err != nil {
		t.Fatalf("QuotaLimitUsage = %v, want nil", err)
	}
	limits, err = client.QuotaList(ctx, name, "/")
	if err != nil {
		t.Fatalf("QuotaList(/) = %v, want nil", err)
	}
	if len(limits) != 1 {
		t.Fatalf("QuotaList(/) = %v, want one entry", quotaPaths(limits))
	}
	if !limits[0].HasLimit || limits[0].HardLimit != 1<<30 {
		t.Errorf("the limit of / is %+v, want a hard limit of %d", limits[0], uint64(1<<30))
	}

	// A directory that does not exist: gluster answers both commands differently, but
	// both have to be recognised.
	if err = client.QuotaLimitUsage(ctx, name, "/nosuch", 1<<30); !errors.Is(err, ErrPathNotFound) {
		t.Fatalf("QuotaLimitUsage(/nosuch) = %v, want ErrPathNotFound", err)
	}
	if _, err = client.QuotaList(ctx, name, "/nosuch"); !errors.Is(err, ErrPathNotFound) {
		t.Fatalf("QuotaList(/nosuch) = %v, want ErrPathNotFound", err)
	}

	if err = client.QuotaRemove(ctx, name, "/"); err != nil {
		t.Fatalf("QuotaRemove = %v, want nil", err)
	}
	if err = client.QuotaRemove(ctx, name, "/"); err != nil {
		t.Fatalf("the second QuotaRemove = %v, want nil", err)
	}

	// A started volume cannot be deleted.
	if err = client.VolumeDelete(ctx, name); !errors.Is(err, ErrVolumeStarted) {
		t.Fatalf("VolumeDelete of a started volume = %v, want ErrVolumeStarted", err)
	}
	if err = client.VolumeStop(ctx, name); err != nil {
		t.Fatalf("VolumeStop = %v, want nil", err)
	}
	if err = client.VolumeStop(ctx, name); err != nil {
		t.Fatalf("the second VolumeStop = %v, want nil", err)
	}

	if err = client.VolumeDelete(ctx, name); err != nil {
		t.Fatalf("VolumeDelete = %v, want nil", err)
	}
	if err = client.VolumeDelete(ctx, name); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("the second VolumeDelete = %v, want ErrVolumeNotFound", err)
	}
}
