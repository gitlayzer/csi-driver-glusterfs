package gluster

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Keys of the Secret data that [ParseCredentials] reads. They follow the
// kubernetes.io/ssh-auth Secret type, so the same Secret can also be used by
// other tooling; unknown keys are ignored.
const (
	// KeyUsername is the login name on the GlusterFS server.
	KeyUsername = "username"
	// KeyPrivateKey is the unencrypted PEM encoded private key.
	KeyPrivateKey = "ssh-privatekey"
	// KeyKnownHosts is the known_hosts content the server host keys are
	// verified against.
	KeyKnownHosts = "known_hosts"
	// KeyPort is the optional SSH port; it defaults to [DefaultSSHPort].
	KeyPort = "port"

	// DefaultSSHPort is used when the Secret does not set a port.
	DefaultSSHPort = 22

	// minRSABits is the smallest RSA key that is accepted. 1024 bit keys can
	// still be parsed but are no longer safe.
	minRSABits = 2048
)

// ErrInvalidCredentials is the sentinel wrapped by every error of
// [ParseCredentials]. A message names the offending key and the problem but
// never contains a value from the Secret, and never wraps the original parsing
// error either: errors of ssh.ParsePrivateKey may quote the input.
var ErrInvalidCredentials = errors.New("invalid ssh credentials")

// usernameRE is the whitelist for the login name. It is the subset of what
// useradd(8) accepts that a Kubernetes Secret can be validated against: a lower
// case letter or underscore first, then lower case letters, digits, underscores
// and dashes, at most 32 characters.
var usernameRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// portRE matches the decimal form of the port.
var portRE = regexp.MustCompile(`^[0-9]{1,5}$`)

// hostKeyAlgorithmOrder is the fixed preference order of the host key algorithms
// this package offers. Only the algorithms whose key type occurs in known_hosts
// are actually offered, so the negotiated key type is always one the
// administrator pinned. RSA uses the SHA-2 signature variants only; the SHA-1
// "ssh-rsa" algorithm is never offered.
var hostKeyAlgorithmOrder = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA512,
	ssh.KeyAlgoRSASHA256,
}

// keyTypeAlgorithms maps the key type of a known_hosts record to the host key
// algorithms to offer for it. An RSA host key can sign with SHA-2 only, which is
// why it maps to two algorithm names.
var keyTypeAlgorithms = map[string][]string{
	ssh.KeyAlgoED25519:  {ssh.KeyAlgoED25519},
	ssh.KeyAlgoECDSA256: {ssh.KeyAlgoECDSA256},
	ssh.KeyAlgoECDSA384: {ssh.KeyAlgoECDSA384},
	ssh.KeyAlgoECDSA521: {ssh.KeyAlgoECDSA521},
	ssh.KeyAlgoRSA:      {ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
}

// Credentials is a parsed SSH Secret: the login name, the port, the client key
// and the pinned host keys. The key material and the host key callback are
// deliberately unexported and have no accessor, so that a [Credentials] value
// cannot leak them through formatting or through an accidental copy to a log.
type Credentials struct {
	// User is the login name on the GlusterFS server.
	User string
	// Port is the SSH port of the server, 22 by default.
	Port int

	// signer is the client key. ParseCredentials never returns a Credentials
	// with a nil signer, so there is no path that connects without a key.
	signer ssh.Signer
	// hostKeyCallback verifies the server key against known_hosts. It is never a
	// callback that accepts every key.
	hostKeyCallback ssh.HostKeyCallback
	// hostKeyAlgorithms restricts the key exchange to the key types known_hosts
	// pins. Without it the client could negotiate a key type that known_hosts
	// does not record (an ecdsa key, for instance, when only the ed25519 key was
	// pinned), which would then be reported as a host key mismatch.
	hostKeyAlgorithms []string
}

// String returns a summary of the credentials and no key material:
// "username=<user> port=<port>".
func (c *Credentials) String() string {
	if c == nil {
		return "<nil>"
	}
	return fmt.Sprintf("username=%s port=%d", c.User, c.Port)
}

// GoString returns the same summary as [Credentials.String], so that the %#v verb
// cannot print the fields either.
func (c *Credentials) GoString() string { return c.String() }

// ParseCredentials parses the data of an SSH Secret. It is independent of where
// the data comes from; deciding whether the CSI request secrets or a mounted
// Secret are used belongs to the caller (T-17).
//
// Every error wraps [ErrInvalidCredentials] and names the offending key only.
func ParseCredentials(data map[string]string) (*Credentials, error) {
	user, err := parseUsername(data[KeyUsername])
	if err != nil {
		return nil, err
	}
	port, err := parsePort(data[KeyPort])
	if err != nil {
		return nil, err
	}
	signer, err := parsePrivateKey(data[KeyPrivateKey])
	if err != nil {
		return nil, err
	}
	callback, algorithms, err := parseKnownHosts(data[KeyKnownHosts])
	if err != nil {
		return nil, err
	}
	return &Credentials{
		User:              user,
		Port:              port,
		signer:            signer,
		hostKeyCallback:   callback,
		hostKeyAlgorithms: algorithms,
	}, nil
}

// invalidCredentialsf builds an error that wraps [ErrInvalidCredentials].
func invalidCredentialsf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCredentials, fmt.Sprintf(format, args...))
}

// parseUsername validates the login name.
func parseUsername(user string) (string, error) {
	if user == "" {
		return "", invalidCredentialsf("%s is required", KeyUsername)
	}
	if !usernameRE.MatchString(user) {
		return "", invalidCredentialsf("%s must match %s", KeyUsername, usernameRE.String())
	}
	return user, nil
}

// parsePort validates the optional port.
func parsePort(value string) (int, error) {
	if value == "" {
		return DefaultSSHPort, nil
	}
	if !portRE.MatchString(value) {
		return 0, invalidCredentialsf("%s must be a decimal port number", KeyPort)
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, invalidCredentialsf("%s must be between 1 and 65535", KeyPort)
	}
	return port, nil
}

// parsePrivateKey parses the client key and rejects every form this package does
// not support: an encrypted key, a key type other than ed25519, ecdsa or rsa, and
// an RSA key that is too short.
func parsePrivateKey(pemBytes string) (ssh.Signer, error) {
	if strings.TrimSpace(pemBytes) == "" {
		return nil, invalidCredentialsf("%s is required", KeyPrivateKey)
	}
	signer, err := ssh.ParsePrivateKey([]byte(pemBytes))
	if err != nil {
		// The original error is neither wrapped nor formatted into this one: it
		// may quote the input, and the input is the private key.
		if isPassphraseMissing(err) {
			return nil, invalidCredentialsf("%s: passphrase protected private keys are not supported", KeyPrivateKey)
		}
		return nil, invalidCredentialsf("%s is not a valid unencrypted private key", KeyPrivateKey)
	}
	if err := checkKeyType(signer.PublicKey()); err != nil {
		return nil, err
	}
	return signer, nil
}

// isPassphraseMissing reports whether err says that a passphrase is required.
func isPassphraseMissing(err error) bool {
	var missing *ssh.PassphraseMissingError
	return errors.As(err, &missing)
}

// checkKeyType rejects the key types this package cannot use and RSA keys shorter
// than [minRSABits].
func checkKeyType(key ssh.PublicKey) error {
	switch key.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		return nil
	case ssh.KeyAlgoRSA:
		cryptoKey, ok := key.(ssh.CryptoPublicKey)
		if !ok {
			return invalidCredentialsf("%s: rsa key cannot be inspected", KeyPrivateKey)
		}
		rsaKey, ok := cryptoKey.CryptoPublicKey().(*rsa.PublicKey)
		if !ok {
			return invalidCredentialsf("%s: rsa key cannot be inspected", KeyPrivateKey)
		}
		if rsaKey.N.BitLen() < minRSABits {
			return invalidCredentialsf("%s: rsa keys shorter than %d bits are not supported",
				KeyPrivateKey, minRSABits)
		}
		return nil
	default:
		return invalidCredentialsf("%s: key type %s is not supported (use ed25519, ecdsa or rsa)",
			KeyPrivateKey, quoteValue(key.Type()))
	}
}

// parseKnownHosts validates the known_hosts content and builds the host key
// callback and the host key algorithm list from it.
//
// The content is parsed line by line first, so that every error can name the
// line and no message of the ssh package (which may quote the content) has to be
// propagated. The callback itself can only be built from a file, so the content
// is written to a temporary file with mode 0600 and removed again on every path.
func parseKnownHosts(content string) (ssh.HostKeyCallback, []string, error) {
	if strings.TrimSpace(content) == "" {
		return nil, nil, invalidCredentialsf("%s is required", KeyKnownHosts)
	}
	keyTypes, err := collectKnownHostKeyTypes(content)
	if err != nil {
		return nil, nil, err
	}
	algorithms := hostKeyAlgorithmsFor(keyTypes)
	if len(algorithms) == 0 {
		return nil, nil, invalidCredentialsf("%s contains no supported host key type", KeyKnownHosts)
	}
	callback, err := newKnownHostsCallback(content)
	if err != nil {
		return nil, nil, err
	}
	return callback, algorithms, nil
}

// collectKnownHostKeyTypes parses every record of the content and returns the key
// types that occurred, in order of first appearance. A record with a
// @cert-authority or @revoked marker is rejected, because neither is supported.
func collectKnownHostKeyTypes(content string) ([]string, error) {
	var (
		keyTypes []string
		seen     = make(map[string]bool)
	)
	for i, line := range strings.Split(content, "\n") {
		number := i + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		marker, _, key, _, _, err := ssh.ParseKnownHosts([]byte(line))
		if err != nil {
			return nil, invalidCredentialsf("%s line %d: not a valid known_hosts record", KeyKnownHosts, number)
		}
		switch marker {
		case "":
		case "cert-authority", "revoked":
			return nil, invalidCredentialsf("%s line %d: %q markers are not supported", KeyKnownHosts, number, marker)
		default:
			return nil, invalidCredentialsf("%s line %d: marker %s is not supported", KeyKnownHosts, number, quoteValue(marker))
		}
		if !seen[key.Type()] {
			seen[key.Type()] = true
			keyTypes = append(keyTypes, key.Type())
		}
	}
	if len(keyTypes) == 0 {
		return nil, invalidCredentialsf("%s contains no host key record", KeyKnownHosts)
	}
	return keyTypes, nil
}

// hostKeyAlgorithmsFor turns the key types found in known_hosts into the fixed
// preference order of [hostKeyAlgorithmOrder].
func hostKeyAlgorithmsFor(keyTypes []string) []string {
	present := make(map[string]bool, len(keyTypes))
	for _, keyType := range keyTypes {
		for _, algorithm := range keyTypeAlgorithms[keyType] {
			present[algorithm] = true
		}
	}
	algorithms := make([]string, 0, len(present))
	for _, algorithm := range hostKeyAlgorithmOrder {
		if present[algorithm] {
			algorithms = append(algorithms, algorithm)
		}
	}
	return algorithms
}

// newKnownHostsCallback writes content to a temporary file and builds the
// knownhosts callback from it. The file is removed again before this function
// returns, on every path: known_hosts holds public keys only, but the driver must
// not leave files behind in a container.
func newKnownHostsCallback(content string) (ssh.HostKeyCallback, error) {
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	name, err := writeTempFile("glusterfs-csi-known-hosts-*", content)
	if err != nil {
		return nil, invalidCredentialsf("%s: cannot create a temporary file: %s", KeyKnownHosts, reasonOf(err))
	}
	defer func() { _ = os.Remove(name) }()

	callback, err := knownhosts.New(name)
	if err != nil {
		// The line by line parse above already accepted every record, so this is
		// an I/O or environment failure. Its message would contain the temporary
		// file name and the record text, so it is dropped.
		return nil, invalidCredentialsf("%s: cannot build the host key verifier", KeyKnownHosts)
	}
	return callback, nil
}

// reasonOf renders an *fs.PathError without its path, which is the only part that
// is useful in an error message here.
func reasonOf(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// writeTempFile creates a file with mode 0600 in the temporary directory and
// writes content to it. On every failure the file is removed again.
func writeTempFile(pattern, content string) (string, error) {
	return writeTempFileWith(pattern, content, func(pattern string) (tempFile, error) {
		return os.CreateTemp("", pattern)
	})
}

// tempFile is the part of *os.File that [writeTempFileWith] needs.
type tempFile interface {
	// Name returns the path of the file.
	Name() string
	// WriteString writes content to the file.
	WriteString(s string) (int, error)
	// Close closes the file.
	Close() error
}

// writeTempFileWith is [writeTempFile] with an injectable file constructor, so
// that a test can supply a file whose write or close fails and assert that the
// file is removed on that path as well.
func writeTempFileWith(pattern, content string, create func(pattern string) (tempFile, error)) (string, error) {
	f, err := create(pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}
