package gluster

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// The types below mirror the --xml output of `gluster --mode=script --xml`, as
// captured from GlusterFS 9.2, 10.3 and 11.1 (pkg/gluster/testdata). They are
// unexported: the rest of the package converts them into the types of the client
// API, which is where validation and error reporting happen.
//
// encoding/xml ignores unknown elements, which is what keeps one set of structs
// working for all three releases: 9.2 adds <stripeCount> to a volume and 11.1
// adds <ports> to a status node, and neither is used here.

// naValue is how the gluster CLI spells a value that does not apply.
const naValue = "N/A"

// cliOutput is the root element of every answer of the gluster CLI.
type cliOutput struct {
	XMLName xml.Name `xml:"cliOutput"`
	// OpRet is the result of the command: 0 is success, everything else
	// (including -1 and -2) is a failure. It is authoritative; the exit status of
	// the process is not.
	OpRet string `xml:"opRet"`
	// OpErrno is the errno the glusterd reported. It is almost always the generic
	// 30800, so it is only reported and never used for a decision.
	OpErrno string `xml:"opErrno"`
	// OpErrstr is the human readable cause, and the only thing that can be
	// classified into a sentinel error.
	OpErrstr string `xml:"opErrstr"`

	VolInfo   *volInfoXML   `xml:"volInfo"`
	VolStatus *volStatusXML `xml:"volStatus"`
	VolQuota  *volQuotaXML  `xml:"volQuota"`
}

// volInfoXML is the payload of `volume info`.
type volInfoXML struct {
	Volumes *volumesXML `xml:"volumes"`
}

// volStatusXML is the payload of `volume status`.
type volStatusXML struct {
	Volumes *volumesXML `xml:"volumes"`
}

// volumesXML holds the <volume> elements of a `volume info` or `volume status`
// answer together with the <count> the glusterd believes it reported.
type volumesXML struct {
	Count   string      `xml:"count"`
	Volumes []volumeXML `xml:"volume"`
}

// volumeXML is one <volume> element. `volume info` fills the scalar fields and the
// bricks; `volume status` fills VolName and Nodes.
type volumeXML struct {
	Name            string      `xml:"name"`
	ID              string      `xml:"id"`
	VolName         string      `xml:"volName"`
	StatusStr       string      `xml:"statusStr"`
	BrickCount      string      `xml:"brickCount"`
	DistCount       string      `xml:"distCount"`
	ReplicaCount    string      `xml:"replicaCount"`
	ArbiterCount    string      `xml:"arbiterCount"`
	DisperseCount   string      `xml:"disperseCount"`
	RedundancyCount string      `xml:"redundancyCount"`
	TypeStr         string      `xml:"typeStr"`
	Bricks          []brickXML  `xml:"bricks>brick"`
	Options         []optionXML `xml:"options>option"`
	Nodes           []nodeXML   `xml:"node"`
}

// brickXML is one <brick> element. The brick name is also the character data of
// the element, but the samples repeat it as a nested <name> child, so only the
// child is read.
type brickXML struct {
	Name      string `xml:"name"`
	HostUUID  string `xml:"hostUuid"`
	IsArbiter string `xml:"isArbiter"`
}

// optionXML is one <option> of the volume options.
type optionXML struct {
	Name  string `xml:"name"`
	Value string `xml:"value"`
}

// nodeXML is one <node> element of `volume status`: a brick process or one of the
// daemons that are reported for a started volume. Brick nodes are told apart by
// their path, which starts with "/"; a daemon's path is "localhost".
type nodeXML struct {
	Hostname  string `xml:"hostname"`
	Path      string `xml:"path"`
	PeerID    string `xml:"peerid"`
	Status    string `xml:"status"`
	Port      string `xml:"port"`
	PID       string `xml:"pid"`
	SizeTotal string `xml:"sizeTotal"`
	SizeFree  string `xml:"sizeFree"`
	Device    string `xml:"device"`
	FSName    string `xml:"fsName"`
}

// volQuotaXML is the payload of `volume quota <vol> list`. It is empty when the
// command succeeded but no path matched.
type volQuotaXML struct {
	Limits []quotaLimitXML `xml:"limit"`
}

// quotaLimitXML is one <limit> element. Every numeric field is either a decimal
// number or "N/A"; the soft limit percentage carries a trailing "%".
type quotaLimitXML struct {
	Path             string `xml:"path"`
	HardLimit        string `xml:"hard_limit"`
	SoftLimitPercent string `xml:"soft_limit_percent"`
	SoftLimitValue   string `xml:"soft_limit_value"`
	UsedSpace        string `xml:"used_space"`
	AvailSpace       string `xml:"avail_space"`
	SLExceeded       string `xml:"sl_exceeded"`
	HLExceeded       string `xml:"hl_exceeded"`
}

// parseCLIOutput decodes one answer of the gluster CLI. Every failure wraps
// [ErrUnexpectedOutput]: the answer is not what this package parses. The message
// names the op and the host and never contains the output itself.
func parseCLIOutput(op, host string, stdout []byte) (*cliOutput, error) {
	if len(strings.TrimSpace(string(stdout))) == 0 {
		return nil, unexpectedf(op, host, "the command printed nothing to stdout")
	}
	var output cliOutput
	if err := xml.Unmarshal(stdout, &output); err != nil {
		return nil, unexpectedf(op, host, "cannot parse the xml output: %s", cleanValue(err.Error()))
	}
	if strings.TrimSpace(output.OpRet) == "" {
		return nil, unexpectedf(op, host, "the xml output has no opRet element")
	}
	return &output, nil
}

// ret parses the opRet of the answer. A value that is not a number is an
// [ErrUnexpectedOutput], because the result of the command would then be unknown.
func (o *cliOutput) ret(op, host string) (int, error) {
	ret, err := strconv.Atoi(strings.TrimSpace(o.OpRet))
	if err != nil {
		return 0, unexpectedf(op, host, "opRet is %s, which is not an integer", cleanValue(o.OpRet))
	}
	return ret, nil
}

// errno parses opErrno. A missing or unparsable value is reported as 0: the errno
// is only carried in an error message and never used for a decision.
func (o *cliOutput) errno() int {
	errno, err := strconv.Atoi(strings.TrimSpace(o.OpErrno))
	if err != nil {
		return 0
	}
	return errno
}

// intField parses a decimal integer field of the XML. An empty field is an
// [ErrUnexpectedOutput] as well: gluster writes the element whenever it applies,
// if only as "N/A", so an empty one means the format changed.
func intField(op, host, field, value string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, unexpectedf(op, host, "the field %s is empty", field)
	}
	parsed, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, unexpectedf(op, host, "the field %s is %s, which is not an integer", field, cleanValue(trimmed))
	}
	return parsed, nil
}

// uintField parses a decimal unsigned integer field of the XML.
func uintField(op, host, field, value string) (uint64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, unexpectedf(op, host, "the field %s is empty", field)
	}
	parsed, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return 0, unexpectedf(op, host, "the field %s is %s, which is not an unsigned integer",
			field, cleanValue(trimmed))
	}
	return parsed, nil
}

// optionalIntField parses a field that may be missing or "N/A". gluster prints
// "N/A" for a value that does not apply, for example the port of a self-heal daemon
// or the pid of a brick whose process is down, and it leaves the element out
// entirely in the non-detail form of `volume status`, which has no size fields.
func optionalIntField(op, host, field, value string) (int, error) {
	if isAbsentValue(value) {
		return 0, nil
	}
	return intField(op, host, field, value)
}

// optionalUintField is optionalIntField for an unsigned value such as a size.
func optionalUintField(op, host, field, value string) (uint64, error) {
	if isAbsentValue(value) {
		return 0, nil
	}
	return uintField(op, host, field, value)
}

// isAbsentValue reports whether an optional field was not reported at all, either
// with the "N/A" gluster prints or by leaving the element out.
func isAbsentValue(value string) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed == "" || trimmed == naValue
}

// percentField parses a soft limit percentage such as "80%" or "N/A".
func percentField(op, host, field, value string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if isAbsentValue(trimmed) {
		return 0, nil
	}
	percent, err := strconv.Atoi(strings.TrimSuffix(trimmed, "%"))
	if err != nil || !strings.HasSuffix(trimmed, "%") {
		return 0, unexpectedf(op, host, "the field %s is %s, which is not a percentage",
			field, cleanValue(trimmed))
	}
	if percent < 0 || percent > 100 {
		return 0, unexpectedf(op, host, "the field %s is %d, which is not between 0 and 100", field, percent)
	}
	return percent, nil
}

// flagField parses a Yes/No/N/A field such as sl_exceeded.
func flagField(op, host, field, value string) (bool, error) {
	switch strings.TrimSpace(value) {
	case "Yes":
		return true, nil
	case "No", naValue:
		return false, nil
	default:
		return false, unexpectedf(op, host, "the field %s is %s, which is not Yes, No or N/A",
			field, cleanValue(value))
	}
}

// checkVolumeCount compares <count> with the number of <volume> elements. A
// mismatch means the answer is truncated or was not parsed as intended, which must
// never be reported as "this volume does not exist".
func checkVolumeCount(op, host string, volumes *volumesXML) error {
	if volumes == nil {
		return unexpectedf(op, host, "the xml output has no volumes element")
	}
	count, err := intField(op, host, "count", volumes.Count)
	if err != nil {
		return err
	}
	if count != len(volumes.Volumes) {
		return unexpectedf(op, host, "count is %d but the output holds %d volumes", count, len(volumes.Volumes))
	}
	return nil
}

// maxTextBytes bounds the length of a text field that is copied out of the XML
// into the result: a volume id, a brick name or an option value. They are opaque
// here, but a corrupted answer must not be able to allocate an arbitrary amount of
// memory in a consumer, and they end up in log lines and mount options.
const maxTextBytes = 4096

// checkText validates a text field that is copied into the result.
func checkText(op, host, field, value string) error {
	if len(value) > maxTextBytes {
		return unexpectedf(op, host, "the field %s is %d bytes, limit is %d", field, len(value), maxTextBytes)
	}
	for i := 0; i < len(value); i++ {
		if isControlByte(value[i]) {
			return unexpectedf(op, host, "the field %s contains a control character at offset %d", field, i)
		}
	}
	return nil
}

// unexpectedf builds an error that wraps [ErrUnexpectedOutput].
func unexpectedf(op, host, format string, args ...any) error {
	reason := fmt.Sprintf(format, args...)
	return fmt.Errorf("%w: gluster %s on %s: %s", ErrUnexpectedOutput, op, host, reason)
}
