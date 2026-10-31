package driver

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	// schemeUnix is the endpoint scheme of a unix domain socket.
	schemeUnix = "unix://"
	// schemeTCP is the endpoint scheme of a TCP address.
	schemeTCP = "tcp://"

	// networkUnix and networkTCP are the network names handed to net.Listen.
	networkUnix = "unix"
	networkTCP  = "tcp"

	// maxPort is the largest valid TCP port.
	maxPort = 65535
)

// ParseEndpoint parses the --endpoint value of the driver.
//
// It supports unix://<absolute or relative path> and tcp://<host:port> and
// returns the network name ("unix" or "tcp") and the address that net.Listen
// expects. The scheme is case sensitive: a CSI deployment configures
// "unix:///csi/csi.sock" verbatim, and anything else is a configuration error
// rather than a variation to guess at.
//
// An empty address part is always rejected. A TCP address additionally has to
// be a host:port pair with a numeric port between 0 and 65535; port 0 is
// allowed because it asks the kernel for a free port, which the tests use.
func ParseEndpoint(endpoint string) (network, address string, err error) {
	if strings.TrimSpace(endpoint) == "" {
		return "", "", errors.New("endpoint must not be empty")
	}

	switch {
	case strings.HasPrefix(endpoint, schemeUnix):
		addr := strings.TrimPrefix(endpoint, schemeUnix)
		if strings.TrimSpace(addr) == "" {
			return "", "", fmt.Errorf("endpoint %q: address must not be empty", endpoint)
		}
		return networkUnix, addr, nil

	case strings.HasPrefix(endpoint, schemeTCP):
		addr := strings.TrimPrefix(endpoint, schemeTCP)
		if strings.TrimSpace(addr) == "" {
			return "", "", fmt.Errorf("endpoint %q: address must not be empty", endpoint)
		}
		if err := validateTCPAddress(addr); err != nil {
			return "", "", fmt.Errorf("endpoint %q: %w", endpoint, err)
		}
		return networkTCP, addr, nil

	default:
		return "", "", fmt.Errorf("endpoint %q: unsupported scheme, must start with %q or %q",
			endpoint, schemeUnix, schemeTCP)
	}
}

// validateTCPAddress checks that addr is a host:port pair with a valid port.
// The host may be empty (":10000" listens on all interfaces), which net.Listen
// accepts.
func validateTCPAddress(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q must be host:port: %w", addr, err)
	}
	return validatePort(port)
}

// validatePort reports whether port is a decimal number between 0 and maxPort.
// strconv.Atoi is not enough on its own: it accepts a sign, and a "port" of
// "+10000" is not something a CSI deployment should ever configure.
func validatePort(port string) error {
	invalid := fmt.Errorf("port %q is not a number between 0 and %d", port, maxPort)
	if port == "" {
		return invalid
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return invalid
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n > maxPort {
		return invalid
	}
	return nil
}
