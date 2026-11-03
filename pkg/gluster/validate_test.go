package gluster

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
)

// TestValidateVolumeName checks the volume name whitelist.
func TestValidateVolumeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "simple name", value: "t16vol"},
		{name: "with dots and dashes", value: "pvc-1a2b.gfs"},
		{name: "with an underscore", value: "vol_1"},
		{name: "one character", value: "v"},
		{name: "at the length limit", value: strings.Repeat("a", 128)},
		{name: "a UUID", value: "d8d9c6f9-87d2-4a5d-9dc7-a231593b0779"},

		{name: "empty", value: "", wantErr: true},
		{name: "a leading dash", value: "-x", wantErr: true},
		{name: "a dash only", value: "-", wantErr: true},
		{name: "a relative path element", value: "..", wantErr: true},
		{name: "the current directory", value: ".", wantErr: true},
		{name: "a slash", value: "a/b", wantErr: true},
		{name: "a space", value: "a b", wantErr: true},
		{name: "a newline", value: "a\nb", wantErr: true},
		{name: "a semicolon", value: "a;b", wantErr: true},
		{name: "a dollar sign", value: "a$b", wantErr: true},
		{name: "a backtick", value: "a`b", wantErr: true},
		{name: "a colon", value: "a:b", wantErr: true},
		{name: "non ASCII", value: "tötest", wantErr: true},
		{name: "one byte above the limit", value: strings.Repeat("a", 129), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateVolumeName("volume name", tc.value)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateVolumeName(%q) = %v, want nil", tc.value, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("validateVolumeName(%q) = %v, want ErrInvalidCommand", tc.value, err)
			}
		})
	}
}

// longPath builds a clean absolute path of at most size bytes whose segments are all
// legal: a single segment longer than 128 bytes would be rejected on its own.
func longPath(size int) string {
	const segment = 64
	var b strings.Builder
	for {
		room := size - b.Len() - 1
		if room < 1 {
			return b.String()
		}
		b.WriteByte('/')
		b.WriteString(strings.Repeat("a", min(segment, room)))
	}
}

// TestValidateDirPath checks the directory path rules for bricks and quota
// directories.
func TestValidateDirPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     string
		allowRoot bool
		wantErr   bool
	}{
		{name: "one segment", value: "/d1"},
		{name: "several segments", value: "/bricks/b1/t16vol"},
		{name: "a dot inside a segment", value: "/a.b/c.d"},
		{name: "an underscore and a dash", value: "/a_b/c-d"},
		{name: "at the length limit", value: longPath(maxPathLength)},
		{name: "the root of a quota directory", value: "/", allowRoot: true},

		{name: "the root of a brick", value: "/", wantErr: true},
		{name: "empty", value: "", wantErr: true},
		{name: "relative", value: "relative", wantErr: true},
		{name: "relative with a slash", value: "a/b", wantErr: true},
		{name: "a parent segment", value: "/a/../b", wantErr: true},
		{name: "an empty segment", value: "/a//b", wantErr: true},
		{name: "a trailing slash", value: "/a/", wantErr: true},
		{name: "a current directory segment", value: "/a/.", wantErr: true},
		{name: "a segment starting with a dash", value: "/a/-b", wantErr: true},
		{name: "a space", value: "/a b", wantErr: true},
		{name: "a newline", value: "/a\nb", wantErr: true},
		{name: "a semicolon", value: "/a;b", wantErr: true},
		{name: "a quote", value: `/a'b`, wantErr: true},
		{name: "a colon", value: "/a:b", wantErr: true},
		{name: "above the length limit", value: longPath(maxPathLength + 1), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateDirPath("quota directory", tc.value, tc.allowRoot)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateDirPath(%q) = %v, want nil", tc.value, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("validateDirPath(%q) = %v, want ErrInvalidCommand", tc.value, err)
			}
		})
	}
}

// TestValidateDirPathNeverEchoesALongValue checks that an oversized value is not
// copied into the message.
func TestValidateDirPathNeverEchoesALongValue(t *testing.T) {
	t.Parallel()

	value := longPath(maxPathLength + 1)
	err := validateDirPath("quota directory", value, false)
	if err == nil {
		t.Fatal("validateDirPath = nil, want an error")
	}
	if strings.Contains(err.Error(), value) {
		t.Errorf("the message contains the value: %d bytes", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "limit is 4096") {
		t.Errorf("the message = %q, want the limit", err)
	}
}

// TestValidateBrick checks the brick rules.
func TestValidateBrick(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		brick   Brick
		wantErr bool
	}{
		{name: "a host name", brick: Brick{Host: "gfs-golden", Path: "/bricks/b1/t16vol"}},
		{name: "an IPv4 literal", brick: Brick{Host: "10.0.0.13", Path: "/data/brick1"}},
		{name: "a qualified host name", brick: Brick{Host: "gfs-1.example.com", Path: "/b"}},
		{name: "a dotted host name", brick: Brick{Host: "gfs_1", Path: "/b"}},

		{name: "a leading dash in the host", brick: Brick{Host: "-h", Path: "/b"}, wantErr: true},
		{name: "a colon in the host", brick: Brick{Host: "h:1", Path: "/b"}, wantErr: true},
		{name: "an IPv6 literal", brick: Brick{Host: "::1", Path: "/b"}, wantErr: true},
		{name: "a space in the host", brick: Brick{Host: "h h", Path: "/b"}, wantErr: true},
		{name: "an empty host", brick: Brick{Host: "", Path: "/b"}, wantErr: true},
		{name: "a slash in the host", brick: Brick{Host: "a/b", Path: "/b"}, wantErr: true},

		{name: "a relative path", brick: Brick{Host: "h", Path: "relative"}, wantErr: true},
		{name: "the root", brick: Brick{Host: "h", Path: "/"}, wantErr: true},
		{name: "a parent segment", brick: Brick{Host: "h", Path: "/a/../b"}, wantErr: true},
		{name: "an empty segment", brick: Brick{Host: "h", Path: "/a//b"}, wantErr: true},
		{name: "a trailing slash", brick: Brick{Host: "h", Path: "/a/"}, wantErr: true},
		{name: "a segment starting with a dash", brick: Brick{Host: "h", Path: "/a/-b"}, wantErr: true},
		{name: "a current directory segment", brick: Brick{Host: "h", Path: "/a/."}, wantErr: true},
		{name: "an empty path", brick: Brick{Host: "h", Path: ""}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateBrick(tc.brick)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateBrick(%v) = %v, want nil", tc.brick, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("validateBrick(%v) = %v, want ErrInvalidCommand", tc.brick, err)
			}
			if errors.Is(err, ErrConnect) {
				t.Errorf("validateBrick(%v) = %v, must not wrap ErrConnect", tc.brick, err)
			}
		})
	}
}

// TestValidateOption checks the volume option rules.
func TestValidateOption(t *testing.T) {
	t.Parallel()

	keys := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "cache size", value: "performance.cache-size"},
		{name: "granular entry heal", value: "cluster.granular-entry-heal"},
		{name: "nfs disable", value: "nfs.disable"},
		{name: "three segments", value: "a.b-c.d0"},
		{name: "digits in a segment", value: "a1.b2"},

		{name: "empty", value: "", wantErr: true},
		{name: "no dot", value: "nodot", wantErr: true},
		{name: "an empty segment", value: "a..b", wantErr: true},
		{name: "an upper case domain", value: "Performance.x", wantErr: true},
		{name: "upper case inside", value: "a.B", wantErr: true},
		{name: "a leading number", value: "1a.b", wantErr: true},
		{name: "a trailing dot", value: "a.b.", wantErr: true},
		{name: "an underscore", value: "a_b.c", wantErr: true},
		{name: "a space", value: "a. b", wantErr: true},
		{name: "a leading dash", value: "-a.b", wantErr: true},
		{name: "a slash", value: "a/b.c", wantErr: true},
		{name: "a segment starting with a dash", value: "a.-b", wantErr: true},
	}
	for _, tc := range keys {
		t.Run("key/"+tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateOptionKey(tc.value)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateOptionKey(%q) = %v, want nil", tc.value, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("validateOptionKey(%q) = %v, want ErrInvalidCommand", tc.value, err)
			}
		})
	}

	values := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "a size with a unit", value: "64MB"},
		{name: "on", value: "on"},
		{name: "off", value: "off"},
		{name: "a number", value: "1024"},
		{name: "a list", value: "10.0.0.1,10.0.0.2"},
		{name: "a percentage", value: "80%"},
		{name: "a path", value: "/var/log/glusterfs"},
		{name: "an equals sign", value: "a=b"},
		{name: "a plus sign", value: "a+b"},
		{name: "an at sign", value: "a@b"},
		{name: "a star", value: "*"},
		{name: "at the length limit", value: strings.Repeat("a", maxOptionValueLength)},

		{name: "empty", value: "", wantErr: true},
		{name: "a leading dash", value: "-x", wantErr: true},
		{name: "a space", value: "a b", wantErr: true},
		{name: "a semicolon", value: "a;b", wantErr: true},
		{name: "a pipe", value: "a|b", wantErr: true},
		{name: "a dollar sign", value: "$HOME", wantErr: true},
		{name: "a backtick", value: "a`b`", wantErr: true},
		{name: "a quote", value: `a"b`, wantErr: true},
		{name: "a single quote", value: "a'b", wantErr: true},
		{name: "a newline", value: "a\nb", wantErr: true},
		{name: "a backslash", value: `a\b`, wantErr: true},
		{name: "one byte above the limit", value: strings.Repeat("a", maxOptionValueLength+1), wantErr: true},
	}
	for _, tc := range values {
		t.Run("value/"+tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateOptionValue(tc.value)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateOptionValue(%q) = %v, want nil", tc.value, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("validateOptionValue(%q) = %v, want ErrInvalidCommand", tc.value, err)
			}
		})
	}
}

// TestValidateOptionValueDoesNotEchoALongValue checks that an oversized option value
// is reported by its length.
func TestValidateOptionValueDoesNotEchoALongValue(t *testing.T) {
	t.Parallel()

	value := strings.Repeat("a", maxOptionValueLength+1)
	err := validateOptionValue(value)
	if err == nil {
		t.Fatal("validateOptionValue = nil, want an error")
	}
	if strings.Contains(err.Error(), value) {
		t.Errorf("the message contains the value: %d bytes", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "limit is 1024") {
		t.Errorf("the message = %q, want the limit", err)
	}
}

// TestQuotaSizeArg checks the quota size rules and the decimal formatting.
func TestQuotaSizeArg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		bytes   uint64
		want    string
		wantErr bool
	}{
		{name: "one byte", bytes: 1, want: "1"},
		{name: "one gibibyte", bytes: 1 << 30, want: "1073741824"},
		{name: "a gigabyte in decimal", bytes: 1_000_000_000, want: "1000000000"},
		{name: "the largest allowed value", bytes: math.MaxInt64, want: "9223372036854775807"},

		{name: "zero", bytes: 0, wantErr: true},
		{name: "one above the largest allowed value", bytes: math.MaxInt64 + 1, wantErr: true},
		{name: "the largest uint64", bytes: math.MaxUint64, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := quotaSizeArg(tc.bytes)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidCommand) {
					t.Fatalf("quotaSizeArg(%d) = %q, %v, want ErrInvalidCommand", tc.bytes, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("quotaSizeArg(%d) = %v, want nil", tc.bytes, err)
			}
			if got != tc.want {
				t.Errorf("quotaSizeArg(%d) = %q, want %q", tc.bytes, got, tc.want)
			}
			if strings.ContainsAny(got, "KMGTPEkmgtpe") {
				t.Errorf("quotaSizeArg(%d) = %q, want plain decimal bytes", tc.bytes, got)
			}
		})
	}
}

// TestValidateCreateOptions checks the rules of `volume create`.
func TestValidateCreateOptions(t *testing.T) {
	t.Parallel()

	brick := func(host, path string) Brick { return Brick{Host: host, Path: path} }
	bricks := func(n int) []Brick {
		result := make([]Brick, n)
		for i := 0; i < n; i++ {
			result[i] = brick("h"+string(rune('1'+i)), "/b"+string(rune('1'+i)))
		}
		return result
	}

	tests := []struct {
		name    string
		opts    CreateOptions
		wantErr bool
	}{
		{name: "one brick", opts: CreateOptions{Name: "v", Bricks: bricks(1)}},
		{name: "replica 3 with 3 bricks", opts: CreateOptions{Name: "v", Replica: 3, Bricks: bricks(3)}},
		{name: "replica 2 with 4 bricks", opts: CreateOptions{Name: "v", Replica: 2, Bricks: bricks(4)}},
		{name: "replica 3 with 6 bricks", opts: CreateOptions{Name: "v", Replica: 3, Bricks: bricks(6)}},
		{name: "force", opts: CreateOptions{Name: "v", Bricks: bricks(1), Force: true}},

		{name: "an empty name", opts: CreateOptions{Bricks: bricks(1)}, wantErr: true},
		{name: "a name with a slash", opts: CreateOptions{Name: "a/b", Bricks: bricks(1)}, wantErr: true},
		{name: "no brick", opts: CreateOptions{Name: "v"}, wantErr: true},
		{name: "replica 1", opts: CreateOptions{Name: "v", Replica: 1, Bricks: bricks(1)}, wantErr: true},
		{name: "replica 3 with 4 bricks", opts: CreateOptions{Name: "v", Replica: 3, Bricks: bricks(4)}, wantErr: true},
		{name: "replica 4 with 3 bricks", opts: CreateOptions{Name: "v", Replica: 4, Bricks: bricks(3)}, wantErr: true},
		{
			name:    "a repeated brick",
			opts:    CreateOptions{Name: "v", Bricks: []Brick{brick("h1", "/b"), brick("h1", "/b")}},
			wantErr: true,
		},
		{
			name:    "an invalid brick",
			opts:    CreateOptions{Name: "v", Bricks: []Brick{brick("h1", "relative")}},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateCreateOptions(tc.opts)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateCreateOptions(%q) = %v, want nil", tc.opts.Name, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("validateCreateOptions(%q) = %v, want ErrInvalidCommand", tc.opts.Name, err)
			}
		})
	}
}

// TestBrickString checks the form gluster expects in `volume create`.
func TestBrickString(t *testing.T) {
	t.Parallel()

	if got, want := (Brick{Host: "gfs-golden", Path: "/bricks/b1"}).String(), "gfs-golden:/bricks/b1"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestInvalidInputNeverReachesTheExecutor is the central validation test: every
// rejected value has to be refused before a command is sent.
func TestInvalidInputNeverReachesTheExecutor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*Client) error
	}{
		{"VolumeInfo with a bad name", func(c *Client) error {
			_, err := c.VolumeInfo(context.Background(), "-x")
			return err
		}},
		{"VolumeInfo with a name that is too long", func(c *Client) error {
			_, err := c.VolumeInfo(context.Background(), strings.Repeat("a", 129))
			return err
		}},
		{"VolumeStatus with a bad name", func(c *Client) error {
			_, err := c.VolumeStatus(context.Background(), "a/b")
			return err
		}},
		{"VolumeStart with a bad name", func(c *Client) error { return c.VolumeStart(context.Background(), "..") }},
		{"VolumeStop with a bad name", func(c *Client) error { return c.VolumeStop(context.Background(), "a b") }},
		{"VolumeDelete with a bad name", func(c *Client) error { return c.VolumeDelete(context.Background(), "") }},
		{"VolumeSet with a bad option key", func(c *Client) error {
			return c.VolumeSet(context.Background(), "t16vol", "nodot", "on")
		}},
		{"VolumeSet with a bad option value", func(c *Client) error {
			return c.VolumeSet(context.Background(), "t16vol", "nfs.disable", "a b")
		}},
		{"VolumeCreate with a bad brick", func(c *Client) error {
			return c.VolumeCreate(context.Background(), CreateOptions{
				Name: "v", Bricks: []Brick{{Host: "h", Path: "/a/../b"}},
			})
		}},
		{"QuotaEnable with a bad name", func(c *Client) error { return c.QuotaEnable(context.Background(), "-v") }},
		{"VolumeSet with a bad name", func(c *Client) error {
			return c.VolumeSet(context.Background(), "-v", "nfs.disable", "on")
		}},
		{"QuotaLimitUsage with a bad name", func(c *Client) error {
			return c.QuotaLimitUsage(context.Background(), "a/b", "/d1", 1)
		}},
		{"QuotaRemove with a bad name", func(c *Client) error {
			return c.QuotaRemove(context.Background(), "", "/d1")
		}},
		{"QuotaList with a bad name", func(c *Client) error {
			_, err := c.QuotaList(context.Background(), "..")
			return err
		}},
		{"QuotaLimitUsage with a zero size", func(c *Client) error {
			return c.QuotaLimitUsage(context.Background(), "t16vol", "/d1", 0)
		}},
		{"QuotaLimitUsage with a bad directory", func(c *Client) error {
			return c.QuotaLimitUsage(context.Background(), "t16vol", "/a/../b", 1)
		}},
		{"QuotaLimitUsage with a size above the limit", func(c *Client) error {
			return c.QuotaLimitUsage(context.Background(), "t16vol", "/d1", math.MaxInt64+1)
		}},
		{"QuotaRemove with a bad directory", func(c *Client) error {
			return c.QuotaRemove(context.Background(), "t16vol", "relative")
		}},
		{"QuotaList with a bad directory", func(c *Client) error {
			_, err := c.QuotaList(context.Background(), "t16vol", "/a/")
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The answer is a success, so a call that reaches the executor would
			// return nil and the case would fail on the missing error.
			client, executor := newStaticClient(t, okAnswer(), nil)
			err := tc.call(client)
			if err == nil {
				t.Fatal("call = nil, want an error")
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("call = %v, want ErrInvalidCommand", err)
			}
			if hosts, _ := executor.Calls(); len(hosts) != 0 {
				t.Errorf("the executor was called %v, want no call", hosts)
			}
		})
	}
}

// TestValidInputReachesTheExecutor checks the other direction of the validation: a
// value that is legal is not blocked.
func TestValidInputReachesTheExecutor(t *testing.T) {
	t.Parallel()

	client, executor := newStaticClient(t, okAnswer(), nil)
	if err := client.QuotaLimitUsage(context.Background(), "t16vol", "/", 1); err != nil {
		t.Fatalf("QuotaLimitUsage = %v, want nil", err)
	}
	if err := client.VolumeSet(context.Background(), "t16vol", "performance.cache-size", "64MB"); err != nil {
		t.Fatalf("VolumeSet = %v, want nil", err)
	}
	_, argvs := executor.Calls()
	if len(argvs) != 2 {
		t.Fatalf("the executor was called %d times, want 2", len(argvs))
	}
	if got, want := argvs[0], wantArgv("volume", "quota", "t16vol", "limit-usage", "/", "1"); !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}
