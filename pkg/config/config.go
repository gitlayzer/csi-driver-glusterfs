// Package config loads and validates the GlusterFS CSI cluster configuration.
//
// The driver is stateless: a volume_id only carries a clusterID, so the
// connection details of a GlusterFS cluster live in the glusterfs-csi-config
// ConfigMap, which Kubernetes mounts as a file
// (/etc/glusterfs-csi/config.json by default, see docs/architecture.md §5.1
// and docs/usage.md §2). Because server addresses change without touching any
// PV, the driver has to pick up a new file content without a restart; [Store]
// does that by polling and comparing content hashes.
//
// The file is a JSON array with one object per cluster:
//
//	[
//	  {
//	    "clusterID": "prod",
//	    "servers": ["gluster-1", "gluster-2", "10.0.0.13"],
//	    "executorSecretRef": {"name": "glusterfs-ssh", "namespace": "glusterfs-csi"}
//	  }
//	]
//
// Every value is validated before it can reach a command line or a path. All
// content and validation errors wrap [ErrInvalid]; errors caused by reading the
// file are returned unchanged, so callers can use errors.Is with fs.ErrNotExist
// and friends.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

const (
	// MaxFileSize is the maximum size of the configuration file. Kubernetes
	// rejects ConfigMaps larger than 1 MiB, so a bigger file is never valid.
	MaxFileSize = 1 << 20

	// MaxServers is the maximum number of servers of a single cluster. It
	// bounds the size of the backup-volfile-servers mount option.
	MaxServers = 32
)

// ErrInvalid is the sentinel wrapped by every content and validation error of
// this package. Errors caused by reading the file (missing file, permission
// denied, ...) do not wrap it and keep their original *fs.PathError.
var ErrInvalid = errors.New("invalid glusterfs-csi config")

// dns1123LabelRE matches a single DNS-1123 label (RFC 1123), which is what a
// Kubernetes namespace and a host name label look like.
var dns1123LabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// dns1123SubdomainRE matches a DNS-1123 subdomain, which is what a Kubernetes
// object name has to be.
var dns1123SubdomainRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// SecretRef points at the Secret that holds the SSH credentials of a cluster's
// Executor. The configuration only ever contains the reference, never the key.
type SecretRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// Cluster is the connection information of a single GlusterFS cluster.
type Cluster struct {
	// ClusterID indexes the entry; it must match the clusterID embedded in a
	// volume_id.
	ClusterID string `json:"clusterID"`
	// Servers are the GlusterFS servers in failover order. The order is also
	// the order of the backup-volfile-servers mount option.
	Servers []string `json:"servers"`
	// ExecutorSecretRef is optional; without it the cluster has no Executor and
	// only supports modes that need no SSH access.
	ExecutorSecretRef *SecretRef `json:"executorSecretRef,omitempty"`
}

// Config is an immutable snapshot of the configuration file. It is safe for
// concurrent use as long as the accessors are used, because they copy.
type Config struct {
	clusters map[string]Cluster
}

// maxEchoLen is the longest user supplied value (a server, a Secret reference
// name or a namespace) that is echoed verbatim in an error message. Longer
// values are reported by their length only: a single JSON string may be almost
// MaxFileSize bytes, and echoing it in full would produce a ~1 MiB log line.
const maxEchoLen = 253

// quotedValue renders a user supplied value for an error message. Short values
// are quoted so that whitespace and empty strings stay visible; oversized ones
// are replaced by their length and never echoed at all.
func quotedValue(v string) string {
	if len(v) > maxEchoLen {
		return fmt.Sprintf("value of length %d", len(v))
	}
	return fmt.Sprintf("%q", v)
}

// invalidf builds a content or validation error that wraps [ErrInvalid].
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// clusterLabel renders the location of a cluster inside the document. The
// clusterID is omitted when it is itself invalid, so that error messages never
// echo a value that failed validation twice.
func clusterLabel(index int, clusterID string) string {
	if clusterID == "" {
		return fmt.Sprintf("clusters[%d]", index)
	}
	return fmt.Sprintf("clusters[%d] (clusterID %s)", index, quotedValue(clusterID))
}

// Parse decodes and validates a configuration document.
//
// The top level must be an array of objects, at least one cluster is required
// and unknown fields are rejected, so that a typo in the ConfigMap is reported
// instead of silently ignored.
func Parse(data []byte) (*Config, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, invalidf("document is empty")
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	// Elements are decoded one by one so that unknown fields can be reported
	// with the index of the offending cluster.
	var raws []json.RawMessage
	if err := dec.Decode(&raws); err != nil {
		return nil, invalidf("document must be a JSON array of cluster objects: %v", err)
	}

	var trailing json.RawMessage
	switch err := dec.Decode(&trailing); {
	case err == nil:
		return nil, invalidf("unexpected content after the top-level array")
	case !errors.Is(err, io.EOF):
		return nil, invalidf("unexpected content after the top-level array: %v", err)
	}

	if len(raws) == 0 {
		return nil, invalidf("no clusters defined, at least 1 is required")
	}

	clusters := make(map[string]Cluster, len(raws))
	for i, raw := range raws {
		var cl Cluster
		el := json.NewDecoder(bytes.NewReader(raw))
		el.DisallowUnknownFields()
		if err := el.Decode(&cl); err != nil {
			return nil, invalidf("clusters[%d]: %v", i, err)
		}
		if err := validateCluster(i, cl); err != nil {
			return nil, err
		}
		if _, dup := clusters[cl.ClusterID]; dup {
			return nil, invalidf("%s: duplicate clusterID", clusterLabel(i, cl.ClusterID))
		}
		clusters[cl.ClusterID] = cl
	}

	return &Config{clusters: clusters}, nil
}

// validateCluster validates a single decoded cluster entry. The index is only
// used to locate the entry in error messages.
func validateCluster(index int, cl Cluster) error {
	label := clusterLabel(index, cl.ClusterID)

	if err := volumeid.ValidateName("clusterID", cl.ClusterID); err != nil {
		return invalidf("clusters[%d]: %v", index, err)
	}
	if len(cl.Servers) == 0 {
		return invalidf("%s: servers: at least 1 server is required", label)
	}
	if len(cl.Servers) > MaxServers {
		return invalidf("%s: servers: %d exceeds the maximum of %d", label, len(cl.Servers), MaxServers)
	}

	seen := make(map[string]int, len(cl.Servers))
	for i, server := range cl.Servers {
		if !validServer(server) {
			return invalidf("%s: servers[%d] %s: not a valid hostname or IP address", label, i, quotedValue(server))
		}
		// Host names are case insensitive, so "Host" and "host" are the same
		// server and must not shift the failover order.
		key := strings.ToLower(server)
		if first, dup := seen[key]; dup {
			return invalidf("%s: servers[%d] %s: duplicate of servers[%d], host names are case insensitive",
				label, i, quotedValue(server), first)
		}
		seen[key] = i
	}

	if cl.ExecutorSecretRef != nil {
		if err := validateSecretRef(*cl.ExecutorSecretRef); err != nil {
			return invalidf("%s: executorSecretRef: %v", label, err)
		}
	}

	return nil
}

// validServer reports whether s is a bare IP address (without a zone) or an
// RFC 1123 host name. Ports, URL schemes, whitespace and trailing dots are
// rejected: every server ends up in a mount option and in an SSH target.
func validServer(s string) bool {
	if addr, err := netip.ParseAddr(s); err == nil {
		// "fe80::1%eth0" parses, but a zone identifier is meaningless on
		// another node and would end up in the mount command.
		return addr.Zone() == ""
	}
	return validHostname(s)
}

// validHostname reports whether s is an RFC 1123 host name: at most 253 bytes,
// labels of 1 to 63 characters from [A-Za-z0-9-] that neither start nor end
// with a hyphen. A trailing dot (the absolute form) is rejected.
func validHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case c == '-' && i != 0 && i != len(label)-1:
			default:
				return false
			}
		}
	}
	return true
}

// validateSecretRef validates the reference to a cluster's SSH Secret. Both
// fields are required and have to be valid Kubernetes names; Secret values are
// never part of the configuration.
func validateSecretRef(ref SecretRef) error {
	switch {
	case ref.Name == "":
		return errors.New("name is required")
	case len(ref.Name) > 253 || !dns1123SubdomainRE.MatchString(ref.Name):
		return fmt.Errorf("name %s is not a valid DNS-1123 subdomain", quotedValue(ref.Name))
	case ref.Namespace == "":
		return errors.New("namespace is required")
	case len(ref.Namespace) > 63 || !dns1123LabelRE.MatchString(ref.Namespace):
		return fmt.Errorf("namespace %s is not a valid DNS-1123 label", quotedValue(ref.Namespace))
	}
	return nil
}

// Get returns the cluster with the given ID. The result is a deep copy, so the
// caller may modify the returned slice and the SecretRef pointer without
// affecting the snapshot or any other caller.
func (c *Config) Get(clusterID string) (Cluster, bool) {
	if c == nil {
		return Cluster{}, false
	}
	cl, ok := c.clusters[clusterID]
	if !ok {
		return Cluster{}, false
	}
	out := Cluster{ClusterID: cl.ClusterID}
	if len(cl.Servers) > 0 {
		out.Servers = make([]string, len(cl.Servers))
		copy(out.Servers, cl.Servers)
	}
	if cl.ExecutorSecretRef != nil {
		ref := *cl.ExecutorSecretRef
		out.ExecutorSecretRef = &ref
	}
	return out, true
}

// ClusterIDs returns the IDs of all clusters in ascending order.
func (c *Config) ClusterIDs() []string {
	if c == nil || len(c.clusters) == 0 {
		return nil
	}
	ids := make([]string, 0, len(c.clusters))
	for id := range c.clusters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// readFile reads at most MaxFileSize+1 bytes of the file at path. Symbolic
// links are followed (os.Open), which is how a ConfigMap volume is presented:
// config.json points at the ..data symlink that kubelet swaps atomically.
//
// The result is one byte longer than MaxFileSize when the file is too large, so
// that callers can tell "too large" from "exactly at the limit" without reading
// the whole file.
func readFile(path string) ([]byte, error) {
	// #nosec G304 -- the path is the operator supplied --config path.
	f, err := os.Open(path)
	if err != nil {
		// Returned unchanged so that callers keep the *fs.PathError, e.g. for
		// errors.Is(err, fs.ErrNotExist) or a transient permission problem.
		return nil, err
	}
	defer func() { _ = f.Close() }()

	return io.ReadAll(io.LimitReader(f, MaxFileSize+1))
}

// loadHashed reads and parses path, returning the snapshot together with the
// hash of the bytes it was built from.
func loadHashed(path string) (*Config, [sha256.Size]byte, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	hash := sha256.Sum256(data)
	if len(data) > MaxFileSize {
		return nil, hash, invalidf("file %q is larger than %d bytes", path, MaxFileSize)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, hash, err
	}
	return cfg, hash, nil
}

// Load reads, decodes and validates the configuration file at path. Symbolic
// links are followed, which is how the ConfigMap volume is mounted, and the
// file may not exceed [MaxFileSize].
//
// Read errors are returned unchanged (they do not wrap [ErrInvalid]), so that
// callers can distinguish a missing file from a broken one.
func Load(path string) (*Config, error) {
	cfg, _, err := loadHashed(path)
	return cfg, err
}
