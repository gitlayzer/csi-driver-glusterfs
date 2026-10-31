// Package version exposes build information about the glusterfs-csi binary.
//
// The variables below are meant to be overridden at link time, e.g.:
//
//	go build -ldflags "-X github.com/gitlayzer/csi-driver-glusterfs/pkg/version.Version=v1.2.3"
package version

import "fmt"

// Values injected via -ldflags "-X ...". They keep their placeholder value when
// the binary is built without the linker flags (plain "go build" / "go test").
var (
	// Version is the semantic version of the build, e.g. "v0.1.0".
	Version = "unknown"
	// GitCommit is the short git commit the binary was built from.
	GitCommit = "unknown"
	// BuildDate is the UTC timestamp of the build in RFC 3339 format.
	BuildDate = "unknown"
)

// Info is an immutable snapshot of the build information.
type Info struct {
	Version   string `json:"version"`
	GitCommit string `json:"gitCommit"`
	BuildDate string `json:"buildDate"`
}

// Get returns the build information of the running binary.
func Get() Info {
	return Info{
		Version:   Version,
		GitCommit: GitCommit,
		BuildDate: BuildDate,
	}
}

// String renders the build information on a single human readable line.
func (i Info) String() string {
	return fmt.Sprintf("glusterfs-csi %s (commit: %s, build date: %s)", i.Version, i.GitCommit, i.BuildDate)
}
