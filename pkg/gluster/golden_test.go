package gluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The golden tests replay the real --xml output of `gluster --mode=script --xml`
// that the architect captured on Debian bullseye (9.2), bookworm (10.3) and trixie
// (11.1). The samples are read-only input: every sample name has to appear in the
// case table below, which is checked by TestGoldenSamplesAreCovered, so a new
// sample cannot be added without a case that exercises it.
//
// Volume ids, peer ids, ports and pids differ between releases and between runs, so
// they are asserted on their shape and not on their value.

// goldenVersionDirPrefix is the prefix of the per release sample directories.
const goldenVersionDirPrefix = "testdata/gluster-"

// goldenSample is one captured command: its stdout, its stderr and its exit status.
type goldenSample struct {
	// XML is the content of <name>.xml, the stdout of the command.
	XML []byte
	// Stderr is the content of <name>.stderr.
	Stderr []byte
	// Exit is the content of <name>.exit.
	Exit int
}

// loadGoldenSample reads the three files of one sample of one release.
func loadGoldenSample(t *testing.T, version, name string) goldenSample {
	t.Helper()
	dir := filepath.Join(goldenVersionDirPrefix + version)
	read := func(suffix string) []byte {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(dir, name+suffix))
		if err != nil {
			t.Fatalf("read sample: %v", err)
		}
		return content
	}
	xmlContent := read(".xml")
	stderr := read(".stderr")
	exitContent := strings.TrimSpace(string(read(".exit")))
	exit, err := strconv.Atoi(exitContent)
	if err != nil {
		t.Fatalf("the exit status of %s/%s is %q, which is not a number", version, name, exitContent)
	}
	return goldenSample{XML: xmlContent, Stderr: stderr, Exit: exit}
}

// goldenHost is the host name of the machine that produced the samples.
const goldenHost = "gfs-golden"

// replayExecutor plays back captured samples in call order and records every call.
//
// Each call consumes the next sample. A sample with exit status 0 is returned as a
// success, a non-zero one as an *ExitError whose Stdout is the captured stdout: that
// is exactly what the SSHExecutor produces, and it is what makes the "exit status 2
// but complete XML" and the "glusterd is not running" paths testable.
type replayExecutor struct {
	t *testing.T

	mu      sync.Mutex
	samples []goldenSample
	hosts   []string
	argvs   [][]string
}

// Run replays the next sample for host.
func (r *replayExecutor) Run(_ context.Context, host string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := len(r.argvs)
	r.hosts = append(r.hosts, host)
	r.argvs = append(r.argvs, slices.Clone(args))
	if index >= len(r.samples) {
		r.t.Fatalf("call %d on %s (%v) has no sample left to replay", index, host, args)
	}
	sample := r.samples[index]
	if sample.Exit == 0 {
		return sample.XML, nil
	}
	return nil, &ExitError{
		Host:    host,
		Program: args[0],
		Code:    sample.Exit,
		Stdout:  sample.XML,
		Stderr:  sample.Stderr,
	}
}

// Calls returns the hosts and argvs of every call, in order.
func (r *replayExecutor) Calls() ([]string, [][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.hosts), cloneArgvs(r.argvs)
}

// cloneArgvs deep copies a list of argvs.
func cloneArgvs(argvs [][]string) [][]string {
	cloned := make([][]string, len(argvs))
	for i, argv := range argvs {
		cloned[i] = slices.Clone(argv)
	}
	return cloned
}

// newReplayClient returns a Client whose calls are answered by the named samples.
func newReplayClient(t *testing.T, version string, names ...string) (*Client, *replayExecutor) {
	t.Helper()
	samples := make([]goldenSample, len(names))
	for i, name := range names {
		samples[i] = loadGoldenSample(t, version, name)
	}
	executor := &replayExecutor{t: t, samples: samples}
	client, err := NewClient(executor, []string{goldenHost})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client, executor
}

// wantArgv is the expected argv of the single call of a case: the gluster prefix
// followed by the subcommand.
func wantArgv(args ...string) []string {
	return append([]string{"gluster", "--mode=script", "--xml"}, args...)
}

// goldenAssert is the check on the result of a case. It receives the value returned
// by the method under test (nil for an error-only method) and the error.
type goldenAssert func(t *testing.T, value any, err error)

// wantOK asserts that the call succeeded, with a check of the returned value.
func wantOK(check func(t *testing.T, value any)) goldenAssert {
	return func(t *testing.T, value any, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("call = %v, want nil", err)
		}
		if check != nil {
			check(t, value)
		}
	}
}

// wantSentinel asserts that the call failed with an error wrapping want.
func wantSentinel(want error) goldenAssert {
	return func(t *testing.T, _ any, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("call = nil, want an error wrapping %v", want)
		}
		if !errors.Is(err, want) {
			t.Fatalf("call = %v, want errors.Is(err, %v)", err, want)
		}
		if errors.Is(err, ErrUnexpectedOutput) {
			t.Fatalf("call = %v, must not be ErrUnexpectedOutput", err)
		}
	}
}

// goldenCase is one row of the table below.
type goldenCase struct {
	// name is the sample name; it has to exist in every release directory.
	name string
	// call runs the method under test.
	call func(t *testing.T, client *Client) (any, error)
	// argv is the argv the call has to send. It is not compared when nil, for the
	// samples that do not correspond to a legal call of this package.
	argv []string
	// want is the check on the result.
	want goldenAssert
	// skipArgv records the calls without an argv check, so that the coverage test
	// knows they are intentional.
	skipArgv bool
}

// goldenCases is the table of the golden test. Every sample of every release has to
// be used exactly by one case; see TestGoldenSamplesAreCovered.
func goldenCases() []goldenCase {
	return []goldenCase{
		// volume info
		{
			name: "volume-info-none",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.Volumes(context.Background())
			},
			argv: wantArgv("volume", "info"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				volumes := value.([]Volume)
				if len(volumes) != 0 {
					t.Fatalf("Volumes = %d volumes, want none", len(volumes))
				}
			}),
		},
		{
			name: "volume-info-all",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.Volumes(context.Background())
			},
			argv: wantArgv("volume", "info"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				volumes := value.([]Volume)
				if len(volumes) != 2 {
					t.Fatalf("Volumes = %d volumes, want 2", len(volumes))
				}
				if volumes[0].Name != "t16one" || volumes[1].Name != "t16vol" {
					t.Fatalf("volume names = %q, %q, want t16one, t16vol", volumes[0].Name, volumes[1].Name)
				}
				t16vol := volumes[1]
				if t16vol.Type != "Replicate" {
					t.Errorf("Type = %q, want Replicate", t16vol.Type)
				}
				if t16vol.ReplicaCount != 3 {
					t.Errorf("ReplicaCount = %d, want 3", t16vol.ReplicaCount)
				}
				if len(t16vol.Bricks) != 3 {
					t.Fatalf("Bricks = %d, want 3", len(t16vol.Bricks))
				}
				for i, brick := range t16vol.Bricks {
					if brick.Host != goldenHost {
						t.Errorf("Bricks[%d].Host = %q, want %q", i, brick.Host, goldenHost)
					}
					want := []string{"/bricks/b1/t16vol", "/bricks/b2/t16vol", "/bricks/b3/t16vol"}[i]
					if brick.Path != want {
						t.Errorf("Bricks[%d].Path = %q, want %q", i, brick.Path, want)
					}
					if !isUUID(brick.HostUUID) {
						t.Errorf("Bricks[%d].HostUUID = %q, want a UUID", i, brick.HostUUID)
					}
					if brick.Arbiter {
						t.Errorf("Bricks[%d].Arbiter = true, want false", i)
					}
				}
				if !isUUID(t16vol.ID) {
					t.Errorf("ID = %q, want a UUID", t16vol.ID)
				}
				if t16vol.BrickCount != 3 || t16vol.DistCount != 1 {
					t.Errorf("BrickCount, DistCount = %d, %d, want 3, 1", t16vol.BrickCount, t16vol.DistCount)
				}
			}),
		},
		{
			name: "volume-info-after-delete",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.Volumes(context.Background())
			},
			argv: wantArgv("volume", "info"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				// The sample was captured after t16vol was deleted; t16one is left.
				volumes := value.([]Volume)
				if len(volumes) != 1 || volumes[0].Name != "t16one" {
					t.Fatalf("Volumes = %v, want exactly t16one", volumeNames(volumes))
				}
			}),
		},
		{
			name: "volume-info-created",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.VolumeInfo(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "info", "t16vol"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				volume := value.(*Volume)
				if volume.Status != VolumeCreated {
					t.Errorf("Status = %q, want %q", volume.Status, VolumeCreated)
				}
				if volume.BrickCount != 3 {
					t.Errorf("BrickCount = %d, want 3", volume.BrickCount)
				}
				if len(volume.Bricks) != 3 {
					t.Fatalf("Bricks = %d, want 3", len(volume.Bricks))
				}
				if volume.Bricks[0].Host != goldenHost || volume.Bricks[0].Path != "/bricks/b1/t16vol" {
					t.Errorf("Bricks[0] = %q:%q, want %s:/bricks/b1/t16vol",
						volume.Bricks[0].Host, volume.Bricks[0].Path, goldenHost)
				}
				if value, ok := volume.Options["nfs.disable"]; !ok || value != "on" {
					t.Errorf("Options[nfs.disable] = %q, %v, want %q, true", value, ok, "on")
				}
			}),
		},
		{
			name: "volume-info-started",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.VolumeInfo(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "info", "t16vol"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				if status := value.(*Volume).Status; status != VolumeStarted {
					t.Fatalf("Status = %q, want %q", status, VolumeStarted)
				}
			}),
		},
		{
			name: "volume-info-missing",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.VolumeInfo(context.Background(), "nosuch")
			},
			argv: wantArgv("volume", "info", "nosuch"),
			want: wantSentinel(ErrVolumeNotFound),
		},

		// volume status
		{
			name: "volume-status-detail",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.VolumeStatus(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "status", "t16vol", "detail"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				bricks := value.([]BrickStatus)
				if len(bricks) != 3 {
					t.Fatalf("VolumeStatus = %d bricks, want 3", len(bricks))
				}
				for i, brick := range bricks {
					if !brick.Online {
						t.Errorf("bricks[%d].Online = false, want true", i)
					}
					if brick.Port <= 0 {
						t.Errorf("bricks[%d].Port = %d, want > 0", i, brick.Port)
					}
					if brick.PID <= 0 {
						t.Errorf("bricks[%d].PID = %d, want > 0", i, brick.PID)
					}
					if brick.SizeTotal == 0 || brick.SizeTotal < brick.SizeFree || brick.SizeFree == 0 {
						t.Errorf("bricks[%d] SizeTotal, SizeFree = %d, %d, want SizeTotal >= SizeFree > 0",
							i, brick.SizeTotal, brick.SizeFree)
					}
					if brick.FSName == "" {
						t.Errorf("bricks[%d].FSName is empty", i)
					}
					if brick.Host != goldenHost {
						t.Errorf("bricks[%d].Host = %q, want %q", i, brick.Host, goldenHost)
					}
					if !isUUID(brick.PeerID) {
						t.Errorf("bricks[%d].PeerID = %q, want a UUID", i, brick.PeerID)
					}
				}
			}),
		},
		{
			name: "volume-status",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.VolumeStatus(context.Background(), "t16vol")
			},
			// VolumeStatus asks for the detail form, which also returns the brick
			// host and the process id; the non-detail sample is the answer to the
			// same question about the same started volume and is parsed the same way.
			argv: wantArgv("volume", "status", "t16vol", "detail"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				bricks := value.([]BrickStatus)
				// The self-heal daemon in the sample has path "localhost" and is
				// skipped, so only the three bricks are left.
				if len(bricks) != 3 {
					t.Fatalf("VolumeStatus = %d bricks, want 3 (the Self-heal Daemon is not a brick)", len(bricks))
				}
				for i, brick := range bricks {
					if !brick.Online {
						t.Errorf("bricks[%d].Online = false, want true", i)
					}
				}
			}),
		},
		{
			name: "volume-status-not-started",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.VolumeStatus(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "status", "t16vol", "detail"),
			want: wantSentinel(ErrVolumeNotStarted),
		},
		{
			name: "volume-status-stopped",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.VolumeStatus(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "status", "t16vol", "detail"),
			want: wantSentinel(ErrVolumeNotStarted),
		},
		{
			name: "volume-status-missing",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.VolumeStatus(context.Background(), "nosuch")
			},
			argv: wantArgv("volume", "status", "nosuch", "detail"),
			want: wantSentinel(ErrVolumeNotFound),
		},

		// volume create
		{
			name: "volume-create-replica3",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeCreate(context.Background(), CreateOptions{
					Name:    "t16vol",
					Replica: 3,
					Bricks: []Brick{
						{Host: goldenHost, Path: "/bricks/b1/t16vol"},
						{Host: goldenHost, Path: "/bricks/b2/t16vol"},
						{Host: goldenHost, Path: "/bricks/b3/t16vol"},
					},
					Force: true,
				})
			},
			argv: wantArgv("volume", "create", "t16vol", "replica", "3",
				"gfs-golden:/bricks/b1/t16vol", "gfs-golden:/bricks/b2/t16vol",
				"gfs-golden:/bricks/b3/t16vol", "force"),
			want: wantOK(nil),
		},
		{
			name: "volume-create-single",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeCreate(context.Background(), CreateOptions{
					Name:    "t16one",
					Bricks:  []Brick{{Host: goldenHost, Path: "/bricks/one/t16one"}},
					Force:   true,
					Replica: 0,
				})
			},
			argv: wantArgv("volume", "create", "t16one", "gfs-golden:/bricks/one/t16one", "force"),
			want: wantOK(nil),
		},
		{
			name: "volume-create-exists",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeCreate(context.Background(), CreateOptions{
					Name:    "t16vol",
					Replica: 3,
					Bricks: []Brick{
						{Host: goldenHost, Path: "/bricks/b1/t16vol"},
						{Host: goldenHost, Path: "/bricks/b2/t16vol"},
						{Host: goldenHost, Path: "/bricks/b3/t16vol"},
					},
					Force: true,
				})
			},
			argv: wantArgv("volume", "create", "t16vol", "replica", "3",
				"gfs-golden:/bricks/b1/t16vol", "gfs-golden:/bricks/b2/t16vol",
				"gfs-golden:/bricks/b3/t16vol", "force"),
			want: wantSentinel(ErrVolumeExists),
		},
		{
			name: "volume-create-bad-brick",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeCreate(context.Background(), CreateOptions{
					Name:   "t16bad",
					Bricks: []Brick{{Host: goldenHost, Path: "/bricks/b1/t16vol"}},
					Force:  true,
				})
			},
			argv: wantArgv("volume", "create", "t16bad", "gfs-golden:/bricks/b1/t16vol", "force"),
			want: wantSentinel(ErrBrickInUse),
		},

		// volume start
		{
			name: "volume-start",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeStart(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "start", "t16vol"),
			want: wantOK(nil),
		},
		{
			name: "volume-start-single",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeStart(context.Background(), "t16one")
			},
			argv: wantArgv("volume", "start", "t16one"),
			want: wantOK(nil),
		},
		{
			name: "volume-start-again",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeStart(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "start", "t16vol"),
			// The state is already reached, so this is success (D-35).
			want: wantOK(nil),
		},
		{
			name: "volume-start-missing",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeStart(context.Background(), "nosuch")
			},
			argv: wantArgv("volume", "start", "nosuch"),
			want: wantSentinel(ErrVolumeNotFound),
		},

		// volume stop
		{
			name: "volume-stop",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeStop(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "stop", "t16vol"),
			want: wantOK(nil),
		},
		{
			name: "volume-stop-again",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeStop(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "stop", "t16vol"),
			want: wantOK(nil),
		},
		{
			name: "volume-stop-missing",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeStop(context.Background(), "nosuch")
			},
			argv: wantArgv("volume", "stop", "nosuch"),
			want: wantSentinel(ErrVolumeNotFound),
		},

		// volume delete
		{
			name: "volume-delete",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeDelete(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "delete", "t16vol"),
			want: wantOK(nil),
		},
		{
			name: "volume-delete-missing",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeDelete(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "delete", "t16vol"),
			want: wantSentinel(ErrVolumeNotFound),
		},
		{
			name: "volume-delete-started",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeDelete(context.Background(), "t16one")
			},
			argv: wantArgv("volume", "delete", "t16one"),
			want: wantSentinel(ErrVolumeStarted),
		},

		// volume set
		{
			name: "volume-set",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeSet(context.Background(), "t16vol", "performance.cache-size", "64MB")
			},
			argv: wantArgv("volume", "set", "t16vol", "performance.cache-size", "64MB"),
			want: wantOK(nil),
		},
		{
			name: "volume-set-bad-option",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.VolumeSet(context.Background(), "t16vol", "no.such.option", "x")
			},
			argv: wantArgv("volume", "set", "t16vol", "no.such.option", "x"),
			// The second line of the answer differs between releases ("Did you mean
			// dht.force-readdirp?" on 11.1), which must not affect the verdict.
			want: wantSentinel(ErrUnknownOption),
		},

		// volume quota enable
		{
			name: "quota-enable",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaEnable(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "quota", "t16vol", "enable"),
			want: wantOK(nil),
		},
		{
			name: "quota-enable-again",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaEnable(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "quota", "t16vol", "enable"),
			want: wantOK(nil),
		},
		{
			name: "quota-enable-missing",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaEnable(context.Background(), "nosuch")
			},
			argv: wantArgv("volume", "quota", "nosuch", "enable"),
			want: wantSentinel(ErrVolumeNotFound),
		},

		// volume quota limit-usage
		{
			name: "quota-limit",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaLimitUsage(context.Background(), "t16vol", "/d1", 1<<30)
			},
			argv: wantArgv("volume", "quota", "t16vol", "limit-usage", "/d1", "1073741824"),
			want: wantOK(nil),
		},
		{
			name: "quota-limit-again",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaLimitUsage(context.Background(), "t16vol", "/d1", 2<<30)
			},
			argv: wantArgv("volume", "quota", "t16vol", "limit-usage", "/d1", "2147483648"),
			want: wantOK(nil),
		},
		{
			name: "quota-limit-root",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaLimitUsage(context.Background(), "t16vol", "/", 10<<30)
			},
			argv: wantArgv("volume", "quota", "t16vol", "limit-usage", "/", "10737418240"),
			want: wantOK(nil),
		},
		{
			name: "quota-limit-not-enabled",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaLimitUsage(context.Background(), "t16vol", "/d1", 1<<30)
			},
			argv: wantArgv("volume", "quota", "t16vol", "limit-usage", "/d1", "1073741824"),
			want: wantSentinel(ErrQuotaNotEnabled),
		},
		{
			name: "quota-limit-missing-dir",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaLimitUsage(context.Background(), "t16vol", "/nosuch", 1<<30)
			},
			argv: wantArgv("volume", "quota", "t16vol", "limit-usage", "/nosuch", "1073741824"),
			// This failure answers with opRet -2 instead of -1, which has to be
			// treated as a failure as well.
			want: wantSentinel(ErrPathNotFound),
		},
		{
			name: "quota-limit-units",
			call: func(_ *testing.T, c *Client) (any, error) {
				// The sample was captured with "2GB", which no case of this package
				// sends (only decimal bytes are passed). The answer is a success, so
				// it only proves that a plain success is parsed as one.
				return nil, c.QuotaLimitUsage(context.Background(), "t16vol", "/d2", 2<<30)
			},
			skipArgv: true,
			want:     wantOK(nil),
		},
		{
			name: "quota-limit-bad-size",
			call: func(_ *testing.T, c *Client) (any, error) {
				// gluster rejects a non-numeric size with exit status 1 and prints
				// the usage text instead of XML. This package never sends a size
				// with a unit, but if the CLI ever fails like this the error has to
				// stay an *ExitError and must not be mistaken for a broken answer.
				return nil, c.QuotaLimitUsage(context.Background(), "t16vol", "/d3", 1)
			},
			skipArgv: true,
			want: func(t *testing.T, _ any, err error) {
				t.Helper()
				if err == nil {
					t.Fatal("call = nil, want an error")
				}
				var exitErr *ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("call = %v, want an error wrapping *ExitError", err)
				}
				if exitErr.Code != 1 {
					t.Errorf("exit status = %d, want 1", exitErr.Code)
				}
				if errors.Is(err, ErrUnexpectedOutput) {
					t.Error("call wraps ErrUnexpectedOutput, want the ExitError unchanged")
				}
			},
		},

		// volume quota remove
		{
			name: "quota-remove",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaRemove(context.Background(), "t16vol", "/d2")
			},
			argv: wantArgv("volume", "quota", "t16vol", "remove", "/d2"),
			want: wantOK(nil),
		},
		{
			name: "quota-remove-again",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaRemove(context.Background(), "t16vol", "/d2")
			},
			argv: wantArgv("volume", "quota", "t16vol", "remove", "/d2"),
			want: wantOK(nil),
		},
		{
			name: "quota-remove-missing-dir",
			call: func(_ *testing.T, c *Client) (any, error) {
				return nil, c.QuotaRemove(context.Background(), "t16vol", "/nosuch")
			},
			argv: wantArgv("volume", "quota", "t16vol", "remove", "/nosuch"),
			want: wantSentinel(ErrPathNotFound),
		},

		// volume quota list
		{
			name: "quota-list-all",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.QuotaList(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "quota", "t16vol", "list"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				limits := value.([]QuotaLimit)
				if len(limits) != 3 {
					t.Fatalf("QuotaList = %d limits, want 3", len(limits))
				}
				wantPaths := []string{"/", "/d1", "/d2"}
				for i, limit := range limits {
					if limit.Path != wantPaths[i] {
						t.Errorf("limits[%d].Path = %q, want %q", i, limit.Path, wantPaths[i])
					}
					if !limit.HasLimit {
						t.Errorf("limits[%d].HasLimit = false, want true", i)
					}
					if limit.HardLimit == 0 {
						t.Errorf("limits[%d].HardLimit = 0, want > 0", i)
					}
					if !limit.SoftLimitExceeded && limit.SoftLimitPercent != 80 {
						t.Errorf("limits[%d].SoftLimitPercent = %d, want 80", i, limit.SoftLimitPercent)
					}
				}
				d1 := limits[1]
				if d1.HardLimit != 2147483648 {
					t.Errorf("limits[1].HardLimit = %d, want 2147483648", d1.HardLimit)
				}
				if d1.SoftLimitPercent != 80 {
					t.Errorf("limits[1].SoftLimitPercent = %d, want 80", d1.SoftLimitPercent)
				}
				if d1.Used != 5242880 {
					t.Errorf("limits[1].Used = %d, want 5242880", d1.Used)
				}
				if d1.HardLimitExceeded || d1.SoftLimitExceeded {
					t.Errorf("limits[1] reports an exceeded limit, want none")
				}
				if d1.Available == 0 {
					t.Errorf("limits[1].Available = 0, want > 0")
				}
				if d1.SoftLimit != 1717986918 {
					t.Errorf("limits[1].SoftLimit = %d, want 1717986918", d1.SoftLimit)
				}
			}),
		},
		{
			name: "quota-list-path",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.QuotaList(context.Background(), "t16vol", "/d1")
			},
			argv: wantArgv("volume", "quota", "t16vol", "list", "/d1"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				limits := value.([]QuotaLimit)
				if len(limits) != 1 || limits[0].Path != "/d1" {
					t.Fatalf("QuotaList = %v, want exactly /d1", quotaPaths(limits))
				}
				if limits[0].HardLimit != 2147483648 {
					t.Errorf("HardLimit = %d, want 2147483648", limits[0].HardLimit)
				}
			}),
		},
		{
			name: "quota-list-paths",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.QuotaList(context.Background(), "t16vol", "/d1", "/d2")
			},
			argv: wantArgv("volume", "quota", "t16vol", "list", "/d1", "/d2"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				limits := value.([]QuotaLimit)
				if got, want := quotaPaths(limits), []string{"/d1", "/d2"}; !slices.Equal(got, want) {
					t.Fatalf("QuotaList = %v, want %v", got, want)
				}
			}),
		},
		{
			name: "quota-list-no-limits",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.QuotaList(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "quota", "t16vol", "list"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				limits := value.([]QuotaLimit)
				if len(limits) != 0 {
					t.Fatalf("QuotaList = %v, want an empty result", quotaPaths(limits))
				}
			}),
		},
		{
			name: "quota-list-not-enabled",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.QuotaList(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "quota", "t16vol", "list"),
			// This is the sample with exit status 2 and a complete XML answer: the
			// verdict must come from the opRet, not from the exit status.
			want: wantSentinel(ErrQuotaNotEnabled),
		},
		{
			name: "quota-list-missing-path",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.QuotaList(context.Background(), "t16vol", "/nosuch")
			},
			argv: wantArgv("volume", "quota", "t16vol", "list", "/nosuch"),
			// opRet is 0 and the answer is empty: only the comparison of the request
			// with the answer reveals that the directory does not exist.
			want: wantSentinel(ErrPathNotFound),
		},
		{
			name: "quota-list-no-limit-path",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.QuotaList(context.Background(), "t16vol", "/d3")
			},
			argv: wantArgv("volume", "quota", "t16vol", "list", "/d3"),
			want: wantOK(func(t *testing.T, value any) {
				t.Helper()
				limits := value.([]QuotaLimit)
				if len(limits) != 1 {
					t.Fatalf("QuotaList = %v, want one entry", quotaPaths(limits))
				}
				limit := limits[0]
				if limit.Path != "/d3" {
					t.Errorf("Path = %q, want /d3", limit.Path)
				}
				if limit.HasLimit {
					t.Error("HasLimit = true, want false (hard_limit is N/A)")
				}
				if limit.HardLimit != 0 {
					t.Errorf("HardLimit = %d, want 0", limit.HardLimit)
				}
				if limit.SoftLimitPercent != 0 || limit.SoftLimit != 0 || limit.Available != 0 {
					t.Errorf("SoftLimitPercent, SoftLimit, Available = %d, %d, %d, want 0, 0, 0",
						limit.SoftLimitPercent, limit.SoftLimit, limit.Available)
				}
				if limit.SoftLimitExceeded || limit.HardLimitExceeded {
					t.Error("a limit is reported as exceeded, want none")
				}
			}),
		},

		// glusterd is not running
		{
			name: "glusterd-down",
			call: func(_ *testing.T, c *Client) (any, error) {
				return c.VolumeInfo(context.Background(), "t16vol")
			},
			argv: wantArgv("volume", "info", "t16vol"),
			want: func(t *testing.T, _ any, err error) {
				t.Helper()
				// With a single host the command is not executed anywhere, so the
				// caller sees the AllHostsFailedError of the Failover. It wraps
				// ErrConnect and must not wrap the ExitError: that is what makes
				// Failover try the next host.
				if !errors.Is(err, ErrConnect) {
					t.Fatalf("call = %v, want errors.Is(err, ErrConnect)", err)
				}
				var exitErr *ExitError
				if errors.As(err, &exitErr) {
					t.Fatalf("call = %v, must not wrap *ExitError", err)
				}
			},
		},
	}
}

// goldenVersions returns the releases that have samples, in a stable order.
func goldenVersions(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix("testdata/"+entry.Name(), goldenVersionDirPrefix) {
			continue
		}
		versions = append(versions, strings.TrimPrefix("testdata/"+entry.Name(), goldenVersionDirPrefix))
	}
	if len(versions) == 0 {
		t.Fatal("testdata holds no golden samples")
	}
	slices.Sort(versions)
	return versions
}

// sampleNames returns the sorted names of the samples of one release.
func sampleNames(t *testing.T, version string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(goldenVersionDirPrefix + version))
	if err != nil {
		t.Fatalf("read the samples of %s: %v", version, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".xml") {
			continue
		}
		names = append(names, strings.TrimSuffix(entry.Name(), ".xml"))
	}
	slices.Sort(names)
	return names
}

// volumeNames returns the names of volumes, for a failure message.
func volumeNames(volumes []Volume) []string {
	names := make([]string, len(volumes))
	for i, volume := range volumes {
		names[i] = volume.Name
	}
	return names
}

// quotaPaths returns the paths of quota limits, for a failure message.
func quotaPaths(limits []QuotaLimit) []string {
	paths := make([]string, len(limits))
	for i, limit := range limits {
		paths[i] = limit.Path
	}
	return paths
}

// isUUID reports whether value has the shape of a UUID.
func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		switch i {
		case 8, 13, 18, 23:
			if value[i] != '-' {
				return false
			}
		default:
			if !isHexDigit(value[i]) {
				return false
			}
		}
	}
	return true
}

// isHexDigit reports whether b is a hexadecimal digit.
func isHexDigit(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

func TestGoldenClient(t *testing.T) {
	t.Parallel()

	for _, version := range goldenVersions(t) {
		for _, tc := range goldenCases() {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				client, executor := newReplayClient(t, version, tc.name)
				value, err := tc.call(t, client)
				tc.want(t, value, err)

				hosts, argvs := executor.Calls()
				if len(hosts) != 1 {
					t.Fatalf("the executor was called %d times, want 1: %v", len(hosts), hosts)
				}
				if hosts[0] != goldenHost {
					t.Errorf("the command ran on %q, want %q", hosts[0], goldenHost)
				}
				if tc.argv == nil {
					if !tc.skipArgv {
						t.Fatal("the case checks no argv, but does not say so with skipArgv")
					}
					return
				}
				if got := argvs[0]; !slices.Equal(got, tc.argv) {
					t.Errorf("argv = %v, want %v", got, tc.argv)
				}
			})
		}
	}
}

// TestGoldenNonexistentServerTriesTheNextHost checks the whole chain of
// daemonCheck and Failover: when the first server answers that its glusterd is not
// running, the command has not reached the cluster and the next server runs it.
func TestGoldenNonexistentServerTriesTheNextHost(t *testing.T) {
	t.Parallel()

	for _, version := range goldenVersions(t) {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			executor := &replayExecutor{t: t, samples: []goldenSample{
				loadGoldenSample(t, version, "glusterd-down"),
				loadGoldenSample(t, version, "volume-info-started"),
			}}
			client, err := NewClient(executor, []string{"gfs-down", "gfs-up"})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			volume, err := client.VolumeInfo(context.Background(), "t16vol")
			if err != nil {
				t.Fatalf("VolumeInfo = %v, want nil", err)
			}
			if volume.Status != VolumeStarted {
				t.Errorf("Status = %q, want %q", volume.Status, VolumeStarted)
			}
			hosts, _ := executor.Calls()
			if want := []string{"gfs-down", "gfs-up"}; !slices.Equal(hosts, want) {
				t.Fatalf("the command ran on %v, want %v", hosts, want)
			}
		})
	}
}

// TestGoldenAllServersDown checks that a cluster in which every glusterd is down is
// reported as unreachable and not as a failed command.
func TestGoldenAllServersDown(t *testing.T) {
	t.Parallel()

	for _, version := range goldenVersions(t) {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			executor := &replayExecutor{t: t, samples: []goldenSample{
				loadGoldenSample(t, version, "glusterd-down"),
				loadGoldenSample(t, version, "glusterd-down"),
			}}
			client, err := NewClient(executor, []string{"gfs-1", "gfs-2"})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = client.VolumeInfo(context.Background(), "t16vol")
			if !errors.Is(err, ErrConnect) {
				t.Fatalf("VolumeInfo = %v, want errors.Is(err, ErrConnect)", err)
			}
			var exitErr *ExitError
			if errors.As(err, &exitErr) {
				t.Fatalf("VolumeInfo = %v, must not wrap *ExitError", err)
			}
			var allErr *AllHostsFailedError
			if !errors.As(err, &allErr) {
				t.Fatalf("VolumeInfo = %v, want *AllHostsFailedError", err)
			}
			if len(allErr.Errors) != 2 {
				t.Errorf("the error holds %d causes, want 2", len(allErr.Errors))
			}
			hosts, _ := executor.Calls()
			if want := []string{"gfs-1", "gfs-2"}; !slices.Equal(hosts, want) {
				t.Fatalf("the command ran on %v, want %v", hosts, want)
			}
		})
	}
}

// TestGoldenSamplesAreCovered fails when a sample exists that no case of the table
// uses, or when the releases do not hold the same set of samples.
func TestGoldenSamplesAreCovered(t *testing.T) {
	t.Parallel()

	versions := goldenVersions(t)
	covered := make(map[string]bool, len(goldenCases()))
	for _, tc := range goldenCases() {
		if covered[tc.name] {
			t.Errorf("the sample %s is used by more than one case", tc.name)
		}
		covered[tc.name] = true
	}

	reference := sampleNames(t, versions[0])
	for _, name := range reference {
		if !covered[name] {
			t.Errorf("the sample %s of %s is not used by any case", name, versions[0])
		}
	}
	for name := range covered {
		if !slices.Contains(reference, name) {
			t.Errorf("the case %s has no sample in %s", name, versions[0])
		}
	}
	for _, version := range versions[1:] {
		if got := sampleNames(t, version); !slices.Equal(got, reference) {
			t.Errorf("the samples of %s differ from those of %s: %v vs %v", version, versions[0], got, reference)
		}
	}
}
