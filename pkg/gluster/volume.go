package gluster

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// The values of [Volume.Status]. gluster reports the state as both a number and
// this string; only the string is exposed.
const (
	// VolumeCreated is the state of a volume that exists but was never started.
	VolumeCreated = "Created"
	// VolumeStarted is the state of a running volume.
	VolumeStarted = "Started"
	// VolumeStopped is the state of a volume that was started and then stopped.
	VolumeStopped = "Stopped"
)

// Volume is one Gluster volume as reported by `volume info`.
type Volume struct {
	// Name is the volume name.
	Name string
	// ID is the volume id of the glusterd, a UUID.
	ID string
	// Status is one of [VolumeCreated], [VolumeStarted] and [VolumeStopped], or the
	// raw text of a release that reports another state.
	Status string
	// Type is the kind of volume: "Distribute", "Replicate",
	// "Distributed-Replicate", ...
	Type string
	// BrickCount is the number of bricks the glusterd reports.
	BrickCount int
	// DistCount is the number of distribute sets.
	DistCount int
	// ReplicaCount is the replica count; 1 for a plain distribute volume.
	ReplicaCount int
	// ArbiterCount is the arbiter count.
	ArbiterCount int
	// DisperseCount is the disperse count.
	DisperseCount int
	// RedundancyCount is the redundancy of a disperse volume.
	RedundancyCount int
	// Bricks are the bricks of the volume, in the order gluster reported them.
	Bricks []VolumeBrick
	// Options maps a volume option name to its value.
	Options map[string]string
}

// VolumeBrick is one brick of a [Volume].
type VolumeBrick struct {
	// Host is the part of the brick name before the first ":/".
	Host string
	// Path is the absolute path of the brick directory, the part from "/" on.
	Path string
	// HostUUID is the UUID of the peer that stores the brick.
	HostUUID string
	// Arbiter reports whether the brick is an arbiter brick.
	Arbiter bool
}

// BrickStatus is the state of one brick process as reported by
// `volume status <name> detail`.
type BrickStatus struct {
	// Host is the host name of the node that runs the brick.
	Host string
	// Path is the brick directory.
	Path string
	// PeerID is the UUID of the peer that stores the brick.
	PeerID string
	// Online reports whether the brick process is running.
	Online bool
	// Port is the port of the brick process, 0 when gluster reported "N/A".
	Port int
	// PID is the process id, 0 when it is "N/A".
	PID int
	// SizeTotal is the size of the file system of the brick in bytes, 0 when it
	// was not reported.
	SizeTotal uint64
	// SizeFree is the free space of that file system in bytes, 0 when it was not
	// reported.
	SizeFree uint64
	// Device is the device the brick lives on.
	Device string
	// FSName is the file system type, for example "xfs" or "overlay".
	FSName string
}

// CreateOptions is the request of [Client.VolumeCreate].
type CreateOptions struct {
	// Name is the name of the new volume.
	Name string
	// Replica is the replica count: 0 for a plain distribute volume, otherwise at
	// least 2, and the number of bricks has to be a multiple of it.
	Replica int
	// Bricks are the bricks of the new volume; at least one, none repeated.
	Bricks []Brick
	// Force appends "force", which lets gluster create the volume over existing
	// data in the brick directories. The driver does not use it; it exists for an
	// operator.
	Force bool
}

// Volumes returns all volumes of the cluster, or an empty slice when there is none.
func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	const op = "volume info"
	output, host, err := c.run(ctx, op, "volume", "info")
	if err != nil {
		return nil, err
	}
	volumes, err := volumesOf(op, host, output)
	if err != nil {
		return nil, err
	}
	result := make([]Volume, 0, len(volumes))
	for _, volume := range volumes {
		parsed, parseErr := parseVolume(op, host, volume)
		if parseErr != nil {
			return nil, parseErr
		}
		result = append(result, *parsed)
	}
	return result, nil
}

// VolumeInfo returns the volume called name.
//
// It returns an error wrapping [ErrVolumeNotFound] when the volume does not exist,
// so that a caller which only needs the volume to be absent can tell that case
// apart from a failure of the cluster.
func (c *Client) VolumeInfo(ctx context.Context, name string) (*Volume, error) {
	const op = "volume info"
	if err := validateVolumeName("volume name", name); err != nil {
		return nil, err
	}
	output, host, err := c.run(ctx, op, "volume", "info", name)
	if err != nil {
		return nil, err
	}
	volumes, err := volumesOf(op, host, output)
	if err != nil {
		return nil, err
	}
	if len(volumes) != 1 {
		return nil, unexpectedf(op, host, "expected exactly one volume, got %d", len(volumes))
	}
	volume, err := parseVolume(op, host, volumes[0])
	if err != nil {
		return nil, err
	}
	if volume.Name != name {
		return nil, unexpectedf(op, host, "asked for volume %s but got volume %s",
			cleanValue(name), cleanValue(volume.Name))
	}
	return volume, nil
}

// VolumeStatus returns the state of the bricks of the volume called name.
//
// It returns an error wrapping [ErrVolumeNotFound] when the volume does not exist
// and one wrapping [ErrVolumeNotStarted] when it is not started. The daemons that
// gluster lists next to the bricks of a started volume are not bricks and are
// skipped.
func (c *Client) VolumeStatus(ctx context.Context, name string) ([]BrickStatus, error) {
	const op = "volume status"
	if err := validateVolumeName("volume name", name); err != nil {
		return nil, err
	}
	output, host, err := c.run(ctx, op, "volume", "status", name, "detail")
	if err != nil {
		return nil, err
	}
	if output.VolStatus == nil || output.VolStatus.Volumes == nil {
		return nil, unexpectedf(op, host, "the xml output has no volumes element")
	}
	volumes := output.VolStatus.Volumes.Volumes
	if len(volumes) != 1 {
		return nil, unexpectedf(op, host, "expected exactly one volume, got %d", len(volumes))
	}
	volume := volumes[0]
	if volume.VolName != name {
		return nil, unexpectedf(op, host, "asked for volume %s but got volume %s",
			cleanValue(name), cleanValue(volume.VolName))
	}
	statuses := make([]BrickStatus, 0, len(volume.Nodes))
	for _, node := range volume.Nodes {
		// A started volume also reports its self-heal and quota daemons, whose path
		// is "localhost" instead of an absolute brick path.
		if !strings.HasPrefix(node.Path, "/") {
			continue
		}
		status, parseErr := parseBrickStatus(op, host, node)
		if parseErr != nil {
			return nil, parseErr
		}
		statuses = append(statuses, *status)
	}
	return statuses, nil
}

// VolumeCreate creates a volume.
//
// It returns an error wrapping [ErrVolumeExists] when the name is taken and one
// wrapping [ErrBrickInUse] when a brick overlaps a brick of an existing volume. It
// does not treat "the volume exists with these parameters" as success: that
// comparison needs the expectations of CreateVolume and belongs to T-18.
func (c *Client) VolumeCreate(ctx context.Context, opts CreateOptions) error {
	const op = "volume create"
	if err := validateCreateOptions(opts); err != nil {
		return err
	}
	args := make([]string, 0, 4+2+len(opts.Bricks)+1)
	args = append(args, "volume", "create", opts.Name)
	if opts.Replica != 0 {
		args = append(args, "replica", decimalInt(opts.Replica))
	}
	for _, brick := range opts.Bricks {
		args = append(args, brick.String())
	}
	if opts.Force {
		args = append(args, "force")
	}
	_, host, err := c.run(ctx, op, args...)
	if err != nil {
		return err
	}
	logSuccess(op, opts.Name, host)
	return nil
}

// VolumeStart starts the volume called name.
//
// Starting an already started volume is success (D-35), so a retried call does not
// fail. A missing volume wraps [ErrVolumeNotFound].
func (c *Client) VolumeStart(ctx context.Context, name string) error {
	const op = "volume start"
	if err := validateVolumeName("volume name", name); err != nil {
		return err
	}
	_, host, err := c.run(ctx, op, "volume", "start", name)
	if err != nil {
		if errors.Is(err, errAlreadyStarted) {
			// The state is already reached, so a retried call is a success (D-35).
			logSuccess(op, name, host)
			return nil
		}
		return err
	}
	logSuccess(op, name, host)
	return nil
}

// VolumeStop stops the volume called name.
//
// Stopping a volume that is not started is success (D-35), including a volume that
// was never started, for which gluster answers with the same message.
func (c *Client) VolumeStop(ctx context.Context, name string) error {
	const op = "volume stop"
	if err := validateVolumeName("volume name", name); err != nil {
		return err
	}
	_, host, err := c.run(ctx, op, "volume", "stop", name)
	if err != nil {
		if errors.Is(err, errAlreadyStopped) {
			logSuccess(op, name, host)
			return nil
		}
		return err
	}
	logSuccess(op, name, host)
	return nil
}

// VolumeDelete deletes the volume called name.
//
// A volume that is still started is reported with an error wrapping
// [ErrVolumeStarted]; the caller has to stop it first. A missing volume wraps
// [ErrVolumeNotFound]; whether that counts as success is the decision of
// DeleteVolume (T-18), not of this package.
func (c *Client) VolumeDelete(ctx context.Context, name string) error {
	const op = "volume delete"
	if err := validateVolumeName("volume name", name); err != nil {
		return err
	}
	_, host, err := c.run(ctx, op, "volume", "delete", name)
	if err != nil {
		return err
	}
	logSuccess(op, name, host)
	return nil
}

// VolumeSet sets one volume option, for example performance.cache-size to 64MB.
//
// An option the release does not know wraps [ErrUnknownOption].
func (c *Client) VolumeSet(ctx context.Context, name, key, value string) error {
	const op = "volume set"
	if err := validateVolumeName("volume name", name); err != nil {
		return err
	}
	if err := validateOptionKey(key); err != nil {
		return err
	}
	if err := validateOptionValue(value); err != nil {
		return err
	}
	_, host, err := c.run(ctx, op, "volume", "set", name, key, value)
	if err != nil {
		return err
	}
	logSuccess(op, name, host)
	return nil
}

// volumesOf extracts the <volume> elements of a `volume info` answer. It reports
// an error when the answer holds no volInfo element at all or when its count does
// not match the number of elements.
func volumesOf(op, host string, output *cliOutput) ([]volumeXML, error) {
	if output.VolInfo == nil {
		return nil, unexpectedf(op, host, "the xml output has no volInfo element")
	}
	if err := checkVolumeCount(op, host, output.VolInfo.Volumes); err != nil {
		return nil, err
	}
	return output.VolInfo.Volumes.Volumes, nil
}

// parseVolume converts one <volume> element of `volume info` into a [Volume].
func parseVolume(op, host string, volume volumeXML) (*Volume, error) {
	result := &Volume{
		Name:    volume.Name,
		ID:      volume.ID,
		Status:  volume.StatusStr,
		Type:    volume.TypeStr,
		Options: make(map[string]string, len(volume.Options)),
		Bricks:  make([]VolumeBrick, 0, len(volume.Bricks)),
	}
	// Every text field that is copied into the result is checked first: a corrupted
	// answer must not be able to put a control character into a log line or a mount
	// option, and a brick name ends up in a volume id.
	for _, field := range []struct{ name, value string }{
		{"volume name", volume.Name},
		{"volume id", volume.ID},
		{"statusStr", volume.StatusStr},
		{"typeStr", volume.TypeStr},
	} {
		if err := checkText(op, host, field.name, field.value); err != nil {
			return nil, err
		}
	}
	// The counts are parsed in a fixed order so that the message of a broken answer
	// does not depend on the iteration order of a map.
	counts := []struct {
		field string
		value string
		dest  *int
	}{
		{"brickCount", volume.BrickCount, &result.BrickCount},
		{"distCount", volume.DistCount, &result.DistCount},
		{"replicaCount", volume.ReplicaCount, &result.ReplicaCount},
		{"arbiterCount", volume.ArbiterCount, &result.ArbiterCount},
		{"disperseCount", volume.DisperseCount, &result.DisperseCount},
		{"redundancyCount", volume.RedundancyCount, &result.RedundancyCount},
	}
	for _, count := range counts {
		parsed, err := intField(op, host, count.field, count.value)
		if err != nil {
			return nil, err
		}
		*count.dest = parsed
	}
	for _, brick := range volume.Bricks {
		parsed, err := parseVolumeBrick(op, host, brick)
		if err != nil {
			return nil, err
		}
		result.Bricks = append(result.Bricks, *parsed)
	}
	for _, option := range volume.Options {
		if err := checkText(op, host, "option name", option.Name); err != nil {
			return nil, err
		}
		if err := checkText(op, host, "option value", option.Value); err != nil {
			return nil, err
		}
		result.Options[option.Name] = option.Value
	}
	return result, nil
}

// parseVolumeBrick converts one <brick> element of `volume info`.
func parseVolumeBrick(op, host string, brick brickXML) (*VolumeBrick, error) {
	if err := checkText(op, host, "brick name", brick.Name); err != nil {
		return nil, err
	}
	brickHost, brickPath, found := strings.Cut(brick.Name, ":/")
	if !found {
		return nil, unexpectedf(op, host, "the brick name %s has no %q separator",
			cleanValue(brick.Name), ":/")
	}
	arbiter, err := intField(op, host, "isArbiter", brick.IsArbiter)
	if err != nil {
		return nil, err
	}
	if brickHost == "" || brickPath == "" {
		return nil, unexpectedf(op, host, "the brick name %s has an empty host or path",
			cleanValue(brick.Name))
	}
	return &VolumeBrick{
		Host:     brickHost,
		Path:     "/" + brickPath,
		HostUUID: brick.HostUUID,
		Arbiter:  arbiter == 1,
	}, nil
}

// parseBrickStatus converts one <node> element of `volume status`.
func parseBrickStatus(op, host string, node nodeXML) (*BrickStatus, error) {
	for _, field := range []struct{ name, value string }{
		{"hostname", node.Hostname},
		{"path", node.Path},
		{"peerid", node.PeerID},
		{"device", node.Device},
		{"fsName", node.FSName},
	} {
		if err := checkText(op, host, field.name, field.value); err != nil {
			return nil, err
		}
	}
	status, err := intField(op, host, "status", node.Status)
	if err != nil {
		return nil, err
	}
	port, err := optionalIntField(op, host, "port", node.Port)
	if err != nil {
		return nil, err
	}
	pid, err := optionalIntField(op, host, "pid", node.PID)
	if err != nil {
		return nil, err
	}
	if pid < 0 {
		pid = 0
	}
	sizeTotal, err := optionalUintField(op, host, "sizeTotal", node.SizeTotal)
	if err != nil {
		return nil, err
	}
	sizeFree, err := optionalUintField(op, host, "sizeFree", node.SizeFree)
	if err != nil {
		return nil, err
	}
	return &BrickStatus{
		Host:      node.Hostname,
		Path:      node.Path,
		PeerID:    node.PeerID,
		Online:    status == 1,
		Port:      port,
		PID:       pid,
		SizeTotal: sizeTotal,
		SizeFree:  sizeFree,
		Device:    node.Device,
		FSName:    node.FSName,
	}, nil
}

// decimalInt formats n as a decimal number. It exists so that no caller is tempted
// to use a formatted string with a unit.
func decimalInt(n int) string { return strconv.Itoa(n) }
