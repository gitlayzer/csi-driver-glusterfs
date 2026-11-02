package driver

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

// Parameter keys of a StorageClass (D-22). They are case sensitive.
const (
	paramClusterID        = "clusterID"
	paramProvisioningMode = "provisioningMode"
	paramParentVolume     = "parentVolume"
	paramOnDelete         = "onDelete"
	paramQuota            = "quota"

	// reservedParamPrefix is the prefix of the parameters the CSI sidecars add
	// or interpret themselves (secrets, --extra-create-metadata). They are not
	// parameters of this driver and are ignored.
	reservedParamPrefix = "csi.storage.k8s.io/"
)

// parameter values that are not spelled as constants of their own.
const (
	// quotaEnabled and quotaDisabled are the only accepted values of the quota
	// parameter. They are a string, not a bool, because Kubernetes passes the
	// parameters of a StorageClass as strings.
	quotaEnabled  = "true"
	quotaDisabled = "false"
)

// maxEchoedParamLength is the longest parameter value echoed verbatim in an
// error message. A longer value is reported by its length only, like an
// oversized volume_id in echoVolumeID, so that a huge StorageClass value never
// ends up in an error message or a log line.
const maxEchoedParamLength = volumeid.MaxLength

// deletePolicy is what DeleteVolume does with the directory of a volume.
type deletePolicy string

const (
	deletePolicyDelete  deletePolicy = "delete"
	deletePolicyRetain  deletePolicy = "retain"
	deletePolicyArchive deletePolicy = "archive"
)

// deletePolicies is the accepted set of the onDelete parameter, in the order
// the error message lists them.
var deletePolicies = []deletePolicy{deletePolicyDelete, deletePolicyRetain, deletePolicyArchive}

// volumeParameters are the validated StorageClass parameters of a volume.
type volumeParameters struct {
	ClusterID    string
	Mode         volumeid.Mode // always volumeid.ModeSubdir in M2
	ParentVolume string
	OnDelete     deletePolicy
}

// errInvalidParameters is wrapped by every error of parseVolumeParameters.
var errInvalidParameters = errors.New("invalid storage class parameters")

// parseVolumeParameters validates the parameters of a StorageClass (D-22).
//
// The function is pure: it never looks at the cluster configuration, at the
// cluster or at the filesystem, so T-11 and ValidateVolumeCapabilities can call
// it with the parameters of a request that describes a volume the driver never
// saw. The same input always produces the same error message, including the
// order of the problems, because the keys are checked in sorted order.
//
// Unrecognised keys are an error instead of being ignored: a StorageClass with
// a typo ("onDelte") would otherwise silently fall back to the default, which
// for onDelete means deleting data the administrator meant to keep. The keys
// the CSI sidecars add ("csi.storage.k8s.io/...") are not parameters of this
// driver and are skipped.
func parseVolumeParameters(params map[string]string) (volumeParameters, error) {
	out := volumeParameters{
		// The defaults: a subdirectory of a shared parent volume, deleted with
		// the PVC (D-22, docs/architecture.md §4.2 and §5.6).
		Mode:     volumeid.ModeSubdir,
		OnDelete: deletePolicyDelete,
	}

	// All problems are reported at once: an administrator who has to fix a
	// StorageClass should not have to iterate one wrong key at a time.
	var problems []error
	for _, key := range sortedParamKeys(params) {
		value := params[key]
		if strings.HasPrefix(key, reservedParamPrefix) {
			continue
		}

		var err error
		switch key {
		case paramClusterID:
			// Whether the cluster exists is a question for the configuration,
			// which this pure function does not have; the caller checks it.
			err = volumeid.ValidateName(paramClusterID, value)
			out.ClusterID = value
		case paramProvisioningMode:
			out.Mode, err = parseProvisioningMode(value)
		case paramParentVolume:
			// Validated even in a mode that does not use it: a parentVolume
			// that is set has to be a usable name.
			err = volumeid.ValidateName(paramParentVolume, value)
			out.ParentVolume = value
		case paramOnDelete:
			out.OnDelete, err = parseDeletePolicy(value)
		case paramQuota:
			err = checkQuota(value)
		default:
			problems = append(problems, fmt.Errorf("unknown parameter %s", echoParamKey(key)))
			continue
		}
		if err != nil {
			// The key is echoed through echoParamKey, never as it came in: a key
			// of unknown length must not end up in the message.
			problems = append(problems, fmt.Errorf("%s: %w", echoParamKey(key), err))
		}
	}

	// A missing key is reported separately from an invalid value, and only once
	// the loop settled the mode: the parent volume is required by the subdir
	// mode only.
	if _, ok := params[paramClusterID]; !ok {
		problems = append(problems, fmt.Errorf("%s is required", paramClusterID))
	}
	if out.Mode == volumeid.ModeSubdir {
		if _, ok := params[paramParentVolume]; !ok {
			problems = append(problems, fmt.Errorf("%s is required in mode %q",
				paramParentVolume, volumeid.ModeSubdir))
		}
	}

	if len(problems) > 0 {
		// errors.Join keeps every problem in the message and makes errors.Is
		// work for each of them, and fmt.Errorf("%w: %w", ...) keeps
		// errors.Is(err, errInvalidParameters) true at the same time.
		return volumeParameters{}, fmt.Errorf("%w: %w", errInvalidParameters, errors.Join(problems...))
	}
	return out, nil
}

// sortedParamKeys returns the keys of params in ascending order, so that the
// messages of parseVolumeParameters do not depend on map iteration order.
func sortedParamKeys(params map[string]string) []string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// parseProvisioningMode maps the provisioningMode parameter to a volume mode.
//
// Only the subdir mode is implemented in M2. The other two are recognised and
// refused with a message that says why, instead of the generic "unknown value":
// a StorageClass written for a later milestone then fails with an answer that
// explains the plan rather than looking like a typo.
func parseProvisioningMode(value string) (volumeid.Mode, error) {
	switch value {
	case string(volumeid.ModeSubdir):
		return volumeid.ModeSubdir, nil
	case string(volumeid.ModeStatic):
		return "", errors.New("static volumes are not provisioned, create the PersistentVolume by hand")
	case string(volumeid.ModeVolume):
		return "", fmt.Errorf("provisioningMode %q is not supported yet", string(volumeid.ModeVolume))
	default:
		return "", fmt.Errorf("must be %q, got %s",
			string(volumeid.ModeSubdir), echoParamValue(value))
	}
}

// parseDeletePolicy maps the onDelete parameter to a deletePolicy.
//
// Only the exact spellings are accepted. A value that differs in case is
// rejected instead of being folded: "Delete" would be the default anyway, but a
// parameter that is accepted with an unexpected spelling hides a wrong one.
func parseDeletePolicy(value string) (deletePolicy, error) {
	for _, policy := range deletePolicies {
		if value == string(policy) {
			return policy, nil
		}
	}
	return "", fmt.Errorf("must be one of %s, got %s", joinDeletePolicies(), echoParamValue(value))
}

// joinDeletePolicies renders the accepted onDelete values for an error message.
func joinDeletePolicies() string {
	names := make([]string, 0, len(deletePolicies))
	for _, policy := range deletePolicies {
		names = append(names, fmt.Sprintf("%q", string(policy)))
	}
	return strings.Join(names, ", ")
}

// checkQuota validates the quota parameter.
//
// Quota enforcement arrives with M3. Accepting quota=true and ignoring it would
// let a user believe the capacity of a volume is enforced while it is not, so
// it is refused until the controller can implement it; quota=false changes
// nothing and is therefore accepted (D-22).
func checkQuota(value string) error {
	switch value {
	case quotaDisabled:
		return nil
	case quotaEnabled:
		return fmt.Errorf("%q is not supported yet", quotaEnabled)
	default:
		return fmt.Errorf("must be %q or %q, got %s", quotaEnabled, quotaDisabled, echoParamValue(value))
	}
}

// echoParamValue renders a parameter value for an error message. Values that
// are too long to be a valid name are replaced by their length and never echoed
// at all: the length of a value is unbounded, the message should not be.
func echoParamValue(value string) string {
	if len(value) > maxEchoedParamLength {
		return fmt.Sprintf("a value of length %d", len(value))
	}
	return fmt.Sprintf("%q", value)
}

// echoParamKey is echoParamValue for a parameter key. An unknown key is user
// input of unknown size as well.
func echoParamKey(key string) string {
	if len(key) > maxEchoedParamLength {
		return fmt.Sprintf("of length %d", len(key))
	}
	return fmt.Sprintf("%q", key)
}
