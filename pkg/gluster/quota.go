package gluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// QuotaLimit is one directory quota of a volume, as reported by
// `volume quota <vol> list`.
//
// A directory without a limit is reported as well: gluster lists it with a path, a
// used space and "N/A" everywhere else. HasLimit tells the two apart, everything
// else is 0 for such an entry.
type QuotaLimit struct {
	// Path is the directory inside the volume, starting with "/".
	Path string
	// HasLimit reports whether the directory has a hard limit.
	HasLimit bool
	// HardLimit is the hard limit in bytes; 0 when there is none.
	HardLimit uint64
	// SoftLimitPercent is the soft limit as a percentage of the hard limit; 0 when
	// it does not apply.
	SoftLimitPercent int
	// SoftLimit is the soft limit in bytes; 0 when it does not apply.
	SoftLimit uint64
	// Used is the space the directory uses in bytes.
	Used uint64
	// Available is the space left under the hard limit in bytes; 0 when there is no
	// limit.
	Available uint64
	// SoftLimitExceeded reports whether the soft limit is exceeded.
	SoftLimitExceeded bool
	// HardLimitExceeded reports whether the hard limit is exceeded.
	HardLimitExceeded bool
}

// QuotaEnable enables quota on the volume.
//
// Enabling quota that is already enabled is success (D-35). A missing volume wraps
// [ErrVolumeNotFound].
func (c *Client) QuotaEnable(ctx context.Context, volume string) error {
	const op = "volume quota enable"
	if err := validateVolumeName("volume name", volume); err != nil {
		return err
	}
	_, host, err := c.run(ctx, op, "volume", "quota", volume, "enable")
	if err != nil {
		if errors.Is(err, errQuotaAlreadyEnabled) {
			logSuccess(op, volume, host)
			return nil
		}
		return err
	}
	logSuccess(op, volume, host)
	return nil
}

// QuotaLimitUsage sets the hard limit of the directory dir to bytes.
//
// Setting a limit a second time, to the same or to another value, is success. A
// volume without quota wraps [ErrQuotaNotEnabled], a directory that does not exist
// wraps [ErrPathNotFound].
func (c *Client) QuotaLimitUsage(ctx context.Context, volume, dir string, bytes uint64) error {
	const op = "volume quota limit-usage"
	if err := validateVolumeName("volume name", volume); err != nil {
		return err
	}
	if err := validateDirPath("quota directory", dir, true); err != nil {
		return err
	}
	size, err := quotaSizeArg(bytes)
	if err != nil {
		return err
	}
	_, host, err := c.run(ctx, op, "volume", "quota", volume, "limit-usage", dir, size)
	if err != nil {
		return err
	}
	logSuccess(op, volume, host)
	return nil
}

// QuotaRemove removes the limit of the directory dir.
//
// Removing a limit that does not exist is success (D-35). A directory that does not
// exist wraps [ErrPathNotFound].
func (c *Client) QuotaRemove(ctx context.Context, volume, dir string) error {
	const op = "volume quota remove"
	if err := validateVolumeName("volume name", volume); err != nil {
		return err
	}
	if err := validateDirPath("quota directory", dir, true); err != nil {
		return err
	}
	_, host, err := c.run(ctx, op, "volume", "quota", volume, "remove", dir)
	if err != nil {
		if errors.Is(err, errNoQuotaLimit) {
			logSuccess(op, volume, host)
			return nil
		}
		return err
	}
	logSuccess(op, volume, host)
	return nil
}

// QuotaList returns the quota limits of the volume.
//
// Without dirs every limit of the volume is returned in the order gluster reported
// them; with dirs only those directories, and every requested directory has to
// appear in the answer. A volume without any limit returns an empty slice;
// a volume without quota wraps [ErrQuotaNotEnabled].
//
// A directory that does not exist inside the volume is not reported as a failure by
// gluster: the command succeeds with an empty answer and names the directory only
// on stderr. This method therefore compares the answer with the request and returns
// an error wrapping [ErrPathNotFound] for the first directory that is missing.
func (c *Client) QuotaList(ctx context.Context, volume string, dirs ...string) ([]QuotaLimit, error) {
	const op = "volume quota list"
	if err := validateVolumeName("volume name", volume); err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		if err := validateDirPath("quota directory", dir, true); err != nil {
			return nil, err
		}
	}
	args := make([]string, 0, 5+len(dirs))
	args = append(args, "volume", "quota", volume, "list")
	args = append(args, dirs...)
	output, host, err := c.run(ctx, op, args...)
	if err != nil {
		if errors.Is(err, errNoQuotaConfigured) {
			// Quota is enabled but no limit is configured at all (D-35): that is an
			// empty result, not a failure.
			return []QuotaLimit{}, nil
		}
		return nil, err
	}
	limits := []quotaLimitXML{}
	if output.VolQuota != nil {
		limits = output.VolQuota.Limits
	}
	result := make([]QuotaLimit, 0, len(limits))
	for _, limit := range limits {
		parsed, parseErr := parseQuotaLimit(op, host, limit)
		if parseErr != nil {
			return nil, parseErr
		}
		result = append(result, *parsed)
	}
	if err := checkQuotaPaths(op, host, dirs, result); err != nil {
		return nil, err
	}
	return result, nil
}

// checkQuotaPaths compares the limits gluster reported with the directories that
// were asked for: without a request every limit is returned as it came, with a
// request the answer has to name exactly those directories. A requested directory
// without an entry does not exist (D-35 notes 6 and 7), and an entry that was not
// requested means the answer does not belong to the request.
func checkQuotaPaths(op, host string, dirs []string, limits []QuotaLimit) error {
	if len(dirs) == 0 {
		return nil
	}
	reported := make(map[string]bool, len(limits))
	for _, limit := range limits {
		reported[limit.Path] = true
	}
	requested := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		requested[dir] = true
	}
	for _, dir := range dirs {
		if !reported[dir] {
			return fmt.Errorf("%w: gluster %s on %s: the directory %s has no quota entry",
				ErrPathNotFound, op, host, cleanValue(dir))
		}
	}
	for _, limit := range limits {
		if !requested[limit.Path] {
			return unexpectedf(op, host, "gluster reported the directory %s which was not requested",
				cleanValue(limit.Path))
		}
	}
	return nil
}

// parseQuotaLimit converts one <limit> element into a [QuotaLimit].
func parseQuotaLimit(op, host string, limit quotaLimitXML) (*QuotaLimit, error) {
	if limit.Path == "" {
		return nil, unexpectedf(op, host, "a limit has no path")
	}
	if err := checkText(op, host, "path", limit.Path); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(limit.Path, "/") {
		return nil, unexpectedf(op, host, "the path %s is not absolute", cleanValue(limit.Path))
	}
	used, err := uintField(op, host, "used_space", limit.UsedSpace)
	if err != nil {
		return nil, err
	}
	result := &QuotaLimit{Path: limit.Path, Used: used}
	// N/A means the directory has no limit at all: a directory that merely has no
	// quota yet is still listed, with only its used space filled in.
	result.HasLimit = strings.TrimSpace(limit.HardLimit) != naValue
	if !result.HasLimit {
		return result, nil
	}
	if result.HardLimit, err = uintField(op, host, "hard_limit", limit.HardLimit); err != nil {
		return nil, err
	}
	if result.SoftLimit, err = optionalUintField(op, host, "soft_limit_value", limit.SoftLimitValue); err != nil {
		return nil, err
	}
	if result.Available, err = optionalUintField(op, host, "avail_space", limit.AvailSpace); err != nil {
		return nil, err
	}
	if result.SoftLimitPercent, err = percentField(op, host, "soft_limit_percent", limit.SoftLimitPercent); err != nil {
		return nil, err
	}
	if result.SoftLimitExceeded, err = flagField(op, host, "sl_exceeded", limit.SLExceeded); err != nil {
		return nil, err
	}
	if result.HardLimitExceeded, err = flagField(op, host, "hl_exceeded", limit.HLExceeded); err != nil {
		return nil, err
	}
	return result, nil
}
