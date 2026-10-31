// Package volumeid encodes, decodes and validates the volume_id values used by
// the GlusterFS CSI driver.
//
// The controller is stateless: everything it needs to delete, expand, mount or
// stat a volume is carried inside the volume_id (docs/architecture.md §5.1).
// The canonical, case sensitive form is:
//
//	gfs:v1:<mode>:<clusterID>:<volume>[:<subdir>]
//
// Every field ends up in a gluster command line argument or in a mount path, so
// all values are validated against a strict whitelist before they are used.
//
// This package deliberately only distinguishes valid from invalid input: the
// returned errors are always wrapped around [ErrInvalid]. Mapping that sentinel
// to gRPC status codes is the job of the RPC layer, which follows the CSI
// conventions used by csi-sanity:
//
//   - NodeStageVolume/NodePublishVolume/NodeUnstageVolume/
//     NodeUnpublishVolume/NodeGetVolumeStats, ValidateVolumeCapabilities and
//     ControllerExpandVolume map ErrInvalid to codes.NotFound, because
//     csi-sanity probes those paths with ids that are not in this driver's
//     format (for example "some-vol-id").
//   - DeleteVolume on an invalid id succeeds, so that deleting an unknown or
//     already deleted volume is idempotent.
package volumeid

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Mode is the provisioning mode encoded in a volume_id.
type Mode string

const (
	// ModeStatic is an administrator managed Gluster volume, addressed directly.
	ModeStatic Mode = "static"
	// ModeSubdir is a subdirectory of a shared parent Gluster volume.
	ModeSubdir Mode = "subdir"
	// ModeVolume is a dedicated Gluster volume created for a single PVC.
	ModeVolume Mode = "volume"
)

const (
	// Prefix is the first segment of every volume_id.
	Prefix = "gfs"
	// Version is the format version of the volume_id layout.
	Version = "v1"
	// MaxLength is the maximum length of a volume_id in bytes (CSI spec: the
	// default limit for string fields is 128 bytes).
	MaxLength = 128
)

// ErrInvalid is the sentinel wrapped by every validation and parsing error
// returned by this package.
var ErrInvalid = errors.New("invalid volume id")

// nameRE is the whitelist for a single name field. It is compiled once; the
// repetition bound mirrors [MaxLength].
var nameRE = regexp.MustCompile(fmt.Sprintf(`^[A-Za-z0-9._-]{1,%d}$`, MaxLength))

// invalidError carries a human readable cause while unwrapping to [ErrInvalid].
type invalidError struct{ msg string }

func (e *invalidError) Error() string { return e.msg }

// Unwrap makes errors.Is(err, ErrInvalid) succeed for every error of this package.
func (e *invalidError) Unwrap() error { return ErrInvalid }

// invalidf builds an [invalidError].
func invalidf(format string, args ...any) error {
	return &invalidError{msg: fmt.Sprintf(format, args...)}
}

// ID is a decoded volume_id.
type ID struct {
	// Mode selects the provisioning mode.
	Mode Mode
	// ClusterID indexes the entry in the glusterfs-csi-config ConfigMap.
	ClusterID string
	// Volume is the Gluster volume name.
	Volume string
	// Subdir is an optional single level directory name; empty means the whole
	// volume.
	Subdir string
}

// ValidateName validates a single name field (clusterID, Gluster volume name,
// subdirectory name and, in the future, other StorageClass parameters). The
// field name is only used in error messages.
//
// A name must match ^[A-Za-z0-9._-]{1,128}$, must not be "." or ".." (path
// traversal) and must not start with "-" (it would be parsed as a flag by
// gluster or mount when placed in an argv slice).
func ValidateName(field, value string) error {
	switch {
	case len(value) > MaxLength:
		// Never echo an oversized value; report its length instead.
		return invalidf("invalid %s: length %d exceeds %d", field, len(value), MaxLength)
	case value == "":
		return invalidf("invalid %s %q: must not be empty", field, value)
	case value == "." || value == "..":
		return invalidf("invalid %s %q: must not be a relative path element", field, value)
	case strings.HasPrefix(value, "-"):
		return invalidf("invalid %s %q: must not start with %q", field, value, "-")
	case !nameRE.MatchString(value):
		return invalidf("invalid %s %q: must match %s", field, value, nameRE.String())
	}
	return nil
}

// validateMode rejects unknown modes, including the empty mode and modes that
// only differ from a known one by case.
func validateMode(mode Mode) error {
	switch mode {
	case ModeStatic, ModeSubdir, ModeVolume:
		return nil
	default:
		return invalidf("invalid mode %q: must be one of %q, %q or %q",
			string(mode), ModeStatic, ModeSubdir, ModeVolume)
	}
}

// Validate validates the whole ID: every field, the per-mode subdir rules and
// the length of the encoded form.
func (id ID) Validate() error {
	if err := validateMode(id.Mode); err != nil {
		return err
	}
	if err := ValidateName("clusterID", id.ClusterID); err != nil {
		return err
	}
	if err := ValidateName("volume", id.Volume); err != nil {
		return err
	}

	switch id.Mode {
	case ModeSubdir:
		if id.Subdir == "" {
			return invalidf("invalid subdir %q: mode %q requires a subdir", id.Subdir, ModeSubdir)
		}
	case ModeVolume:
		if id.Subdir != "" {
			return invalidf("invalid subdir %q: mode %q must not have a subdir", id.Subdir, ModeVolume)
		}
	case ModeStatic:
		// Subdir is optional for static volumes.
	}

	if id.Subdir != "" {
		if err := ValidateName("subdir", id.Subdir); err != nil {
			return err
		}
	}

	if encoded := id.format(); len(encoded) > MaxLength {
		return invalidf("invalid volume id: length %d exceeds %d", len(encoded), MaxLength)
	}
	return nil
}

// Encode validates id and returns its canonical string representation.
func Encode(id ID) (string, error) {
	if err := id.Validate(); err != nil {
		return "", err
	}
	return id.format(), nil
}

// Decode parses and validates s. On success Encode(Decode(s)) == s holds, so
// every volume has exactly one canonical id; that uniqueness is what makes the
// RPCs idempotent.
func Decode(s string) (ID, error) {
	if s == "" {
		return ID{}, invalidf("invalid volume id: input is empty")
	}
	if len(s) > MaxLength {
		return ID{}, invalidf("invalid volume id: length %d exceeds %d", len(s), MaxLength)
	}

	// Fields may not contain ':', so splitting is unambiguous.
	parts := strings.Split(s, ":")
	if len(parts) != 5 && len(parts) != 6 {
		return ID{}, invalidf("invalid volume id: expected 5 or 6 colon separated segments, got %d", len(parts))
	}
	if parts[0] != Prefix {
		return ID{}, invalidf("invalid volume id %q: unsupported prefix %q, want %q", s, parts[0], Prefix)
	}
	if parts[1] != Version {
		return ID{}, invalidf("invalid volume id %q: unsupported version %q, want %q", s, parts[1], Version)
	}

	id := ID{
		Mode:      Mode(parts[2]),
		ClusterID: parts[3],
		Volume:    parts[4],
	}
	if len(parts) == 6 {
		if parts[5] == "" {
			// "gfs:v1:static:c:vol:" must not decode: the canonical form of a
			// volume without a subdir has exactly 5 segments.
			return ID{}, invalidf("invalid volume id %q: empty subdir segment, use the 5 segment form", s)
		}
		id.Subdir = parts[5]
	}

	if err := id.Validate(); err != nil {
		return ID{}, err
	}
	return id, nil
}

// format renders the canonical string without validating it.
func (id ID) format() string {
	var b strings.Builder
	b.WriteString(Prefix)
	b.WriteByte(':')
	b.WriteString(Version)
	b.WriteByte(':')
	b.WriteString(string(id.Mode))
	b.WriteByte(':')
	b.WriteString(id.ClusterID)
	b.WriteByte(':')
	b.WriteString(id.Volume)
	if id.Subdir != "" {
		b.WriteByte(':')
		b.WriteString(id.Subdir)
	}
	return b.String()
}
