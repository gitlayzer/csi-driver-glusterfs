package gluster

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

const (
	// maxPathLength bounds a brick path or a quota directory. The Linux limit for
	// a path is PATH_MAX, 4096 bytes including the terminating NUL.
	maxPathLength = 4096

	// maxOptionValueLength bounds the value of a volume option. Every value gluster
	// documents is far shorter; the limit only keeps a StorageClass parameter from
	// turning into a huge argv.
	maxOptionValueLength = 1024
)

// optionKeyRE is the whitelist for a volume option name. Every option of gluster
// is "<domain>.<name>", lower case, with at least one dot, for example
// "performance.cache-size" or "cluster.granular-entry-heal".
var optionKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)+$`)

// optionValueRE is the whitelist for a volume option value. It covers what gluster
// accepts in practice: sizes with a unit ("64MB"), "on"/"off", comma separated
// lists, percentages, paths and IPv4 addresses. Characters that could change the
// meaning of the argv or of a shell word are not in the class. The length is checked
// separately because the repetition limit of a Go regular expression is 1000.
var optionValueRE = regexp.MustCompile(`^[A-Za-z0-9._,:/*@+=%-]+$`)

// Brick is one brick of a volume: the host that stores it and the absolute path of
// its directory on that host.
type Brick struct {
	Host string
	Path string
}

// String returns "host:path", the form gluster expects in `volume create`.
func (b Brick) String() string { return b.Host + ":" + b.Path }

// validateVolumeName checks a Gluster volume name with the volume_id whitelist:
// ^[A-Za-z0-9._-]{1,128}$, no "." or "..", no leading "-". A leading "-" would be
// read as a flag by gluster instead of as a volume name.
func validateVolumeName(field, name string) error {
	if err := volumeid.ValidateName(field, name); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidCommand, err)
	}
	return nil
}

// validateDirPath checks a directory path inside a Gluster volume: it has to be
// absolute, clean and built from whitelisted segments. The result is safe to pass
// as a single argv element and cannot address anything outside the volume.
//
// allowRoot is true for a quota directory, where "/" is the whole volume, and false
// for a brick, which may not be the file system root.
func validateDirPath(field, dir string, allowRoot bool) error {
	switch {
	case dir == "":
		return fmt.Errorf("%w: empty %s", ErrInvalidCommand, field)
	case len(dir) > maxPathLength:
		// Never echo an oversized value; report its length instead.
		return fmt.Errorf("%w: %s is %d bytes, limit is %d",
			ErrInvalidCommand, field, len(dir), maxPathLength)
	case !strings.HasPrefix(dir, "/"):
		return fmt.Errorf("%w: %s %s is not absolute", ErrInvalidCommand, field, quoteValue(dir))
	case path.Clean(dir) != dir:
		return fmt.Errorf("%w: %s %s is not a clean path", ErrInvalidCommand, field, quoteValue(dir))
	}
	if dir == "/" {
		if allowRoot {
			return nil
		}
		return fmt.Errorf("%w: %s must not be %q", ErrInvalidCommand, field, "/")
	}
	for _, segment := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		// ValidateName rejects ".", "..", an empty segment, a leading "-" and
		// everything outside the whitelist.
		if err := volumeid.ValidateName(field+" segment", segment); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidCommand, err)
		}
	}
	return nil
}

// validateBrick checks the host and the path of a brick. The host additionally may
// not start with "-" and may not contain ":", because a brick is handed to gluster
// as the single argument "host:path" and an IPv6 literal would be ambiguous there.
func validateBrick(brick Brick) error {
	if err := validateHost(brick.Host); err != nil {
		return err
	}
	if strings.HasPrefix(brick.Host, "-") {
		return fmt.Errorf("%w: brick host %s must not start with %q",
			ErrInvalidCommand, quoteValue(brick.Host), "-")
	}
	if strings.Contains(brick.Host, ":") {
		return fmt.Errorf("%w: brick host %s must not contain %q (IPv6 literals are not supported)",
			ErrInvalidCommand, quoteValue(brick.Host), ":")
	}
	return validateDirPath("brick path", brick.Path, false)
}

// validateOptionKey checks a volume option name such as "performance.cache-size".
func validateOptionKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty volume option name", ErrInvalidCommand)
	}
	if !optionKeyRE.MatchString(key) {
		return fmt.Errorf("%w: volume option %s does not match %s",
			ErrInvalidCommand, quoteValue(key), optionKeyRE.String())
	}
	return nil
}

// validateOptionValue checks a volume option value such as "64MB" or "on".
func validateOptionValue(value string) error {
	switch {
	case value == "":
		return fmt.Errorf("%w: empty volume option value", ErrInvalidCommand)
	case len(value) > maxOptionValueLength:
		// Never echo an oversized value; report its length instead.
		return fmt.Errorf("%w: volume option value is %d bytes, limit is %d",
			ErrInvalidCommand, len(value), maxOptionValueLength)
	case strings.HasPrefix(value, "-"):
		return fmt.Errorf("%w: volume option value %s must not start with %q",
			ErrInvalidCommand, quoteValue(value), "-")
	case !optionValueRE.MatchString(value):
		return fmt.Errorf("%w: volume option value %s does not match %s",
			ErrInvalidCommand, quoteValue(value), optionValueRE.String())
	}
	return nil
}

// quotaSizeArg validates a quota size and returns it as a plain decimal byte count.
//
// Only bytes are passed, never a value with a unit: gluster parses "2GB" as 2*1024^3
// and a malformed value makes it print the usage without any XML, which this
// package could then not tell apart from a transport failure.
func quotaSizeArg(bytes uint64) (string, error) {
	if bytes == 0 {
		return "", fmt.Errorf("%w: quota size must be at least 1 byte", ErrInvalidCommand)
	}
	if bytes > math.MaxInt64 {
		return "", fmt.Errorf("%w: quota size %d exceeds %d", ErrInvalidCommand, bytes, int64(math.MaxInt64))
	}
	return strconv.FormatUint(bytes, 10), nil
}

// validateCreateOptions checks everything that `volume create` puts into its argv
// before the command runs.
func validateCreateOptions(opts CreateOptions) error {
	if err := validateVolumeName("volume name", opts.Name); err != nil {
		return err
	}
	if len(opts.Bricks) == 0 {
		return fmt.Errorf("%w: volume %s has no brick", ErrInvalidCommand, opts.Name)
	}
	seen := make(map[string]bool, len(opts.Bricks))
	for _, brick := range opts.Bricks {
		if err := validateBrick(brick); err != nil {
			return err
		}
		key := brick.String()
		if seen[key] {
			return fmt.Errorf("%w: brick %s is listed twice", ErrInvalidCommand, quoteValue(key))
		}
		seen[key] = true
	}
	if opts.Replica == 0 {
		return nil
	}
	if opts.Replica < 2 {
		return fmt.Errorf("%w: replica count %d must be at least 2", ErrInvalidCommand, opts.Replica)
	}
	if len(opts.Bricks)%opts.Replica != 0 {
		return fmt.Errorf("%w: %d bricks are not a multiple of the replica count %d",
			ErrInvalidCommand, len(opts.Bricks), opts.Replica)
	}
	return nil
}
