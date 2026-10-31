package mounter

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/config"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

const (
	// FSType is passed to mount(8). /proc/mounts reports the result as
	// "fuse.glusterfs", so both spellings are accepted when an existing mount
	// entry is compared with a spec (see sameGlusterfsMount).
	FSType = "glusterfs"

	// DefaultLogLevel is used unless Spec.Options sets log-level. WARNING keeps
	// the driver from writing a line per request into the mount log while still
	// reporting real failures.
	DefaultLogLevel = "WARNING"
)

// Bounds of the validated input. They exist to keep a malformed StorageClass or
// PV from producing a multi-kilobyte mount option or error message.
const (
	// maxServerLen is the DNS name limit, which also bounds an IPv4 address
	// with room to spare.
	maxServerLen = 253
	// maxOptionLen is the longest mount option fragment that is accepted. Any
	// longer value is certainly not one of the allowlisted options.
	maxOptionLen = 256
	// maxEchoLen is the longest user supplied value echoed verbatim in an error
	// message. Longer values are reported by their length only, so that a huge
	// StorageClass parameter never ends up in a log line.
	maxEchoLen = 253
)

var (
	// ErrInvalidSpec is the sentinel wrapped by every error about the spec
	// itself: the servers, the volume, the subdirectory or the log directory.
	// The Node service maps it to codes.InvalidArgument.
	ErrInvalidSpec = errors.New("invalid mount spec")

	// ErrInvalidOption is the sentinel wrapped by every error about a mount
	// option: an option outside the allowlist, a value of the wrong type, a
	// duplicate key or an empty fragment. The Node service maps it to
	// codes.InvalidArgument.
	ErrInvalidOption = errors.New("invalid mount option")
)

// decimalSecondsRE matches the value of a GlusterFS timeout option, which is a
// number of seconds with at most three fractional digits.
var decimalSecondsRE = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,3})?$`)

// unsignedIntRE matches the value of a GlusterFS counter option.
var unsignedIntRE = regexp.MustCompile(`^[0-9]{1,9}$`)

// logLevels are the accepted values of log-level, from most to least severe.
var logLevels = []string{"CRITICAL", "ERROR", "WARNING", "INFO", "DEBUG", "TRACE", "NONE"}

// yesNo are the accepted values of the boolean-ish options that mount.glusterfs
// spells "yes"/"no" instead of using a bare flag.
var yesNo = []string{"yes", "no"}

// optionRule describes how a single allowlisted mount option is validated.
// Exactly one of the three value fields is set; switchOpt means "no value at
// all".
type optionRule struct {
	// switchOpt marks a bare flag: the fragment must not contain "=".
	switchOpt bool
	// values is the closed set of accepted values of an enumerated option.
	values []string
	// valueRE is the shape of the value of a numeric option.
	valueRE *regexp.Regexp
}

// allowedOptions is the mountOptions allowlist. The keys are case sensitive,
// exactly as mount.glusterfs spells them; anything not listed here is rejected
// by validateOption. Only options that are safe for a tenant controlled
// StorageClass to set are here: they tune caching, ACLs and logging, they never
// change which volume is mounted or how the process is identified.
var allowedOptions = map[string]optionRule{
	// Bare flags.
	"ro":               {switchOpt: true},
	"acl":              {switchOpt: true},
	"selinux":          {switchOpt: true},
	"auto_unmount":     {switchOpt: true},
	"fopen-keep-cache": {switchOpt: true},
	"enable-ino32":     {switchOpt: true},

	// Enumerated values.
	"log-level":              {values: logLevels},
	"use-readdirp":           {values: yesNo},
	"kernel-writeback-cache": {values: yesNo},
	"direct-io-mode":         {values: []string{"enable", "disable"}},

	// Numeric values.
	"attribute-timeout":    {valueRE: decimalSecondsRE},
	"entry-timeout":        {valueRE: decimalSecondsRE},
	"negative-timeout":     {valueRE: decimalSecondsRE},
	"gid-timeout":          {valueRE: decimalSecondsRE},
	"background-qlen":      {valueRE: unsignedIntRE},
	"congestion-threshold": {valueRE: unsignedIntRE},
	"reader-thread-count":  {valueRE: unsignedIntRE},
	"lru-limit":            {valueRE: unsignedIntRE},
	"invalidate-limit":     {valueRE: unsignedIntRE},
}

// forbiddenOptions are the options the driver itself sets, or that let a user
// escape the validated mount setup. They are rejected with a message that says
// why, instead of the generic "not in the allowlist", because a StorageClass
// that tried one of them is likely reused by many PVCs and the reason matters.
var forbiddenOptions = map[string]string{
	"log-file":               "controlled by the driver",
	"backup-volfile-servers": "controlled by the driver",
	"backupvolfile-server":   "controlled by the driver",
	"volfile-server":         "controlled by the driver",
	"volfile-id":             "controlled by the driver",
	"xlator-option":          "not allowed",
	"fuse-mountopts":         "not allowed",
	"process-name":           "not allowed",
	"transport":              "not allowed",
	"rw":                     "not allowed",
}

// Spec describes one GlusterFS FUSE mount.
type Spec struct {
	// Servers are the GlusterFS servers in failover order; at least one is
	// required. Servers[0] becomes the volfile server of the mount source, the
	// rest are passed as backup-volfile-servers.
	Servers []string
	// Volume is the name of the GlusterFS volume.
	Volume string
	// Subdir optionally mounts a single directory of the volume with the native
	// GlusterFS subdirectory mount (server:/volume/subdir).
	Subdir string
	// ReadOnly mounts the volume read only.
	ReadOnly bool
	// Options are the StorageClass or PV mountOptions as they are, one entry per
	// list element; each entry may itself be a comma separated list.
	Options []string
}

// quotedValue renders a user supplied value for an error message. Short values
// are quoted so that whitespace and empty strings stay visible; oversized ones
// are replaced by their length and never echoed at all.
func quotedValue(v string) string {
	if len(v) > maxEchoLen {
		return fmt.Sprintf("value of length %d", len(v))
	}
	return fmt.Sprintf("%q", v)
}

// specErrf builds an error that wraps [ErrInvalidSpec] and names the field.
func specErrf(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidSpec, field, fmt.Sprintf(format, args...))
}

// optionErrf builds an error that wraps [ErrInvalidOption].
func optionErrf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOption, fmt.Sprintf(format, args...))
}

// BuildMountArgs validates spec and returns the mount(8) source and the option
// list of the glusterfs mount. An empty logDir means "no log-file option": the
// mount then logs wherever mount.glusterfs defaults to.
//
// The result is deterministic: the same spec and logDir always produce the same
// source and the same option slice in the same order. That is what allows
// MountGlusterfs to compare an existing mount with a requested one and to treat
// a repeated NodeStage as success.
func BuildMountArgs(spec Spec, logDir string) (source string, options []string, err error) {
	if err = validateServers(spec.Servers); err != nil {
		return "", nil, err
	}
	if err = volumeid.ValidateName("volume", spec.Volume); err != nil {
		return "", nil, specErrf("volume", "%v", err)
	}
	if spec.Subdir != "" {
		if err = volumeid.ValidateName("subdir", spec.Subdir); err != nil {
			return "", nil, specErrf("subdir", "%v", err)
		}
	}
	if err = validateLogDir(logDir); err != nil {
		return "", nil, err
	}

	// The allowlisted user options keep their original order, which makes the
	// resulting slice easy to compare in tests and in logs.
	userOptions, seen, err := splitUserOptions(spec.Options)
	if err != nil {
		return "", nil, err
	}
	options = userOptions

	if spec.ReadOnly && !seen["ro"] {
		options = append(options, "ro")
	}
	if !seen["log-level"] {
		options = append(options, "log-level="+DefaultLogLevel)
	}
	if len(spec.Servers) > 1 {
		options = append(options, "backup-volfile-servers="+strings.Join(spec.Servers[1:], ":"))
	}
	if logDir != "" {
		options = append(options, "log-file="+filepath.Join(logDir, logFileName(spec)))
	}

	source = spec.Servers[0] + ":/" + spec.Volume
	if spec.Subdir != "" {
		source += "/" + spec.Subdir
	}
	return source, options, nil
}

// logFileName is the name of the mount log inside the log directory. The
// subdirectory is joined with "_", so two mounts of different subdirectories
// share a log file only in the rare case where "<volume>_<subdir>" collides
// with another "<volume>" name. That is acceptable: the log file is used for
// debugging a failing mount, and a shared file is still readable.
func logFileName(spec Spec) string {
	if spec.Subdir == "" {
		return spec.Volume + ".log"
	}
	return spec.Volume + "_" + spec.Subdir + ".log"
}

// validateServers validates the server list: at least one server, no more than
// [config.MaxServers], each usable inside a colon separated mount option and no
// host name twice.
func validateServers(servers []string) error {
	switch {
	case len(servers) == 0:
		return specErrf("servers", "at least 1 server is required")
	case len(servers) > config.MaxServers:
		return specErrf("servers", "%d servers exceed the maximum of %d", len(servers), config.MaxServers)
	}

	for i, server := range servers {
		if err := validateServer(server); err != nil {
			return specErrf(fmt.Sprintf("servers[%d]", i), "%v", err)
		}
	}

	// Host names and IPv4 addresses are case insensitive, so "G1" and "g1" are
	// the same server; a duplicate would only produce a redundant (and for
	// backup-volfile-servers, ambiguous) failover target.
	seen := make(map[string]int, len(servers))
	for i, server := range servers {
		key := strings.ToLower(server)
		if first, dup := seen[key]; dup {
			return specErrf(fmt.Sprintf("servers[%d]", i),
				"%s is a duplicate of servers[%d], host names are case insensitive", quotedValue(server), first)
		}
		seen[key] = i
	}
	return nil
}

// validateServer validates a single server. Every server ends up in the
// backup-volfile-servers option, which mount.glusterfs splits on ':' and ',',
// and in the mount source before the first ':', so the characters that carry a
// meaning there are rejected.
func validateServer(server string) error {
	switch {
	case len(server) > maxServerLen:
		return fmt.Errorf("%s exceeds the maximum of %d bytes", quotedValue(server), maxServerLen)
	case server == "":
		return errors.New("must not be empty")
	case strings.ContainsAny(server, ":,/"):
		// An IPv6 address is the important case: it cannot be expressed in
		// "server:/volume" nor in a ':' separated backup-volfile-servers list.
		return fmt.Errorf("%s must not contain ':', ',' or '/'; IPv6 addresses are not supported", quotedValue(server))
	case strings.ContainsAny(server, " \t\n\r\v\f"):
		return fmt.Errorf("%s must not contain whitespace", quotedValue(server))
	case strings.Contains(server, "="):
		return fmt.Errorf("%s must not contain '='", quotedValue(server))
	case strings.HasPrefix(server, "-"):
		// A leading '-' would be parsed as a flag by mount(8).
		return fmt.Errorf("%s must not start with '-'", quotedValue(server))
	}
	return nil
}

// validateLogDir validates the directory the mount log is written to. It is
// used in a path, so a non-clean path (".", "..", a trailing slash, a double
// slash) is rejected instead of being silently normalized.
func validateLogDir(logDir string) error {
	switch {
	case logDir == "":
		// The caller decided not to log to a file.
		return nil
	case !filepath.IsAbs(logDir):
		return specErrf("logDir", "%s must be an absolute path", quotedValue(logDir))
	case filepath.Clean(logDir) != logDir:
		return specErrf("logDir", "%s must be a clean path", quotedValue(logDir))
	}
	return nil
}

// splitUserOptions splits every entry of options on ',' and validates each
// fragment. It returns the fragments in their original order and the set of
// option keys that were seen, which the caller uses to avoid emitting a default
// for an option the user already set.
func splitUserOptions(options []string) (fragments []string, seen map[string]bool, err error) {
	seen = make(map[string]bool, len(options))
	fragments = make([]string, 0, len(options))
	for _, entry := range options {
		for _, fragment := range strings.Split(entry, ",") {
			key, err := validateOption(fragment)
			if err != nil {
				return nil, nil, err
			}
			if seen[key] {
				return nil, nil, optionErrf("option %q is set more than once", key)
			}
			seen[key] = true
			fragments = append(fragments, fragment)
		}
	}
	return fragments, seen, nil
}

// validateOption validates one fragment of a mount option and returns its key.
// The key is the part before '='; for a bare flag it is the whole fragment.
func validateOption(fragment string) (key string, err error) {
	if len(fragment) > maxOptionLen {
		// Never echo an oversized fragment: it is user input of unknown size.
		return "", optionErrf("option of length %d exceeds the maximum of %d bytes",
			len(fragment), maxOptionLen)
	}
	if fragment == "" {
		return "", optionErrf("mount option list contains an empty option")
	}

	key, value, hasValue := strings.Cut(fragment, "=")
	if key == "" {
		return "", optionErrf("option %s has an empty key", quotedValue(fragment))
	}
	if reason, forbidden := forbiddenOptions[key]; forbidden {
		return "", optionErrf("option %q is %s", key, reason)
	}
	rule, ok := allowedOptions[key]
	if !ok {
		return "", optionErrf("option %q is not in the allowlist of mount options", key)
	}

	switch {
	case rule.switchOpt && hasValue:
		return "", optionErrf("option %q is a flag and must not have a value", key)
	case rule.switchOpt:
		return key, nil
	case !hasValue:
		return "", optionErrf("option %q requires a value", key)
	case value == "":
		return "", optionErrf("option %q must not have an empty value", key)
	}

	if rule.values != nil && !slices.Contains(rule.values, value) {
		return "", optionErrf("option %q: %s must be one of %s",
			key, quotedValue(value), strings.Join(rule.values, ", "))
	}
	if rule.valueRE != nil && !rule.valueRE.MatchString(value) {
		return "", optionErrf("option %q: %s is not a valid value", key, quotedValue(value))
	}
	return key, nil
}
