package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// identityService is the part of [Driver] that implements csi.IdentityServer.
// Keeping it a separate embedded type means the Controller and Node methods
// added by later tasks never collide with the Identity ones and that the
// UnimplementedIdentityServer embedding (required by the generated code) stays
// in one place.
type identityService struct {
	csi.UnimplementedIdentityServer
	driver *Driver
}

// GetPluginInfo returns the driver name and the build version.
//
// The manifest field is left unset: this driver needs no extra configuration
// file beyond the ConfigMap that the sidecars mount for it, and a manifest
// would make the CO print it in error messages.
func (s *identityService) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          s.driver.driverName,
		VendorVersion: s.driver.version,
	}, nil
}

// GetPluginCapabilities reports the plugin level capabilities.
//
// The list is derived from the services that are actually registered, never
// from the mode alone: advertising CONTROLLER_SERVICE without a Controller
// service makes the CO call RPCs that answer Unimplemented.
//
// VOLUME_ACCESSIBILITY_CONSTRAINTS is never advertised: every node can reach
// every GlusterFS cluster, so topology would only add scheduling constraints
// (docs/architecture.md §5.4).
func (s *identityService) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	// An empty, non-nil slice: csi-sanity distinguishes "no capabilities" from
	// an error, and a nil slice would serialize to the same empty list but is
	// easier to mistake for "not initialised" when read in a debugger.
	capabilities := make([]*csi.PluginCapability, 0, 1)

	if s.driver.controllerRegistered {
		capabilities = append(capabilities, &csi.PluginCapability{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{
					Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
				},
			},
		})
	}

	return &csi.GetPluginCapabilitiesResponse{Capabilities: capabilities}, nil
}

// Probe reports whether the plugin is ready to serve.
//
// Every readiness check of the current mode runs in order and the first failure
// is returned as FailedPrecondition, which is what the CSI spec requires for an
// unhealthy plugin: the kubelet livenessprobe sidecar restarts the container on
// an error and keeps it running on ready=false.
//
// A cancelled or expired ctx wins over a check, so that shutting down does not
// report an unhealthy node.
func (s *identityService) Probe(ctx context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}

	for _, c := range s.driver.checks {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		if err := c.fn(ctx); err != nil {
			// The check names the dependency (for example "/dev/fuse"), never a
			// secret; the message is what an operator sees in the sidecar log.
			return nil, status.Errorf(codes.FailedPrecondition, "%s is not ready: %v", c.name, err)
		}
	}

	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
