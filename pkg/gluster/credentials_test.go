package gluster

import (
	"crypto"
	"crypto/dsa" //nolint:staticcheck // a DSA key is needed to test the rejected key type
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// markerToken is a unique string that is put into broken key material, so that a
// test can assert that no error message ever echoes the input.
const markerToken = "DO-NOT-ECHO-THIS-TOKEN"

// testKeyPEM returns the PEM encoding of a freshly generated ed25519 key.
func testKeyPEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return marshalPrivateKey(t, priv)
}

// rsaKeyPEM returns the PEM encoding of an RSA key with the given number of bits.
func rsaKeyPEM(t *testing.T, bits int) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatalf("marshal rsa key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

// ecdsaKeyPEM returns the PEM encoding of an ecdsa key of the given curve.
func ecdsaKeyPEM(t *testing.T, curve elliptic.Curve) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatalf("marshal ecdsa key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

// passphraseKeyPEM returns the PEM encoding of a passphrase protected ed25519 key.
func passphraseKeyPEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("secret"))
	if err != nil {
		t.Fatalf("marshal passphrase key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

// publicKeyOf returns the public key of an ed25519 private key.
func publicKeyOf(t *testing.T, priv ed25519.PrivateKey) ssh.PublicKey {
	t.Helper()
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer.PublicKey()
}

// knownHostsLine builds a known_hosts record for host and key.
func knownHostsLine(host string, key ssh.PublicKey) string {
	return knownhosts.Line([]string{host}, key)
}

// validCredentialsData returns Secret data that ParseCredentials accepts.
func validCredentialsData(t *testing.T) map[string]string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return map[string]string{
		KeyUsername:   "gluster",
		KeyPrivateKey: marshalPrivateKey(t, priv),
		KeyKnownHosts: knownHostsLine("gluster-1", publicKeyOf(t, priv)) + "\n",
	}
}

func TestParseCredentialsAcceptsValidData(t *testing.T) {
	t.Parallel()

	data := validCredentialsData(t)
	creds, err := ParseCredentials(data)
	if err != nil {
		t.Fatalf("ParseCredentials = %v, want nil", err)
	}
	if creds.User != "gluster" {
		t.Errorf("User = %q, want %q", creds.User, "gluster")
	}
	if creds.Port != DefaultSSHPort {
		t.Errorf("Port = %d, want %d", creds.Port, DefaultSSHPort)
	}
	if creds.signer == nil || creds.hostKeyCallback == nil {
		t.Fatal("parsed credentials have no signer or no host key callback")
	}
	if want := []string{"ssh-ed25519"}; len(creds.hostKeyAlgorithms) != 1 || creds.hostKeyAlgorithms[0] != want[0] {
		t.Errorf("hostKeyAlgorithms = %q, want %q", creds.hostKeyAlgorithms, want)
	}
}

func TestParseCredentialsKeyTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		privateKey func(*testing.T) string
		wantAlgos  []string
		wantErr    bool
	}{
		{
			name:       "ed25519",
			privateKey: testKeyPEM,
		},
		{
			name:       "ecdsa p256",
			privateKey: func(t *testing.T) string { t.Helper(); return ecdsaKeyPEM(t, elliptic.P256()) },
		},
		{
			name:       "ecdsa p384",
			privateKey: func(t *testing.T) string { t.Helper(); return ecdsaKeyPEM(t, elliptic.P384()) },
		},
		{
			name:       "ecdsa p521",
			privateKey: func(t *testing.T) string { t.Helper(); return ecdsaKeyPEM(t, elliptic.P521()) },
		},
		{
			name:       "rsa 2048 is accepted",
			privateKey: func(t *testing.T) string { t.Helper(); return rsaKeyPEM(t, 2048) },
		},
		{
			name:       "rsa 1024 is rejected",
			privateKey: func(t *testing.T) string { t.Helper(); return rsaKeyPEM(t, 1024) },
			wantErr:    true,
		},
		{
			name:       "passphrase protected key is rejected",
			privateKey: passphraseKeyPEM,
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			privateKey := tc.privateKey(t)
			// The known_hosts record uses its own ed25519 key: this table is about
			// the client key, and a passphrase protected one cannot be parsed here.
			data := map[string]string{
				KeyUsername:   "gluster",
				KeyPrivateKey: privateKey,
				KeyKnownHosts: knownHostsLine("gluster-1", sshPubKeyForTest(t)),
			}
			creds, err := ParseCredentials(data)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidCredentials) {
					t.Fatalf("ParseCredentials = %v, want ErrInvalidCredentials", err)
				}
				if strings.Contains(err.Error(), markerToken) {
					t.Fatalf("error %q contains the key material", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCredentials = %v, want nil", err)
			}
			if tc.wantAlgos != nil {
				if got := strings.Join(creds.hostKeyAlgorithms, ","); got != strings.Join(tc.wantAlgos, ",") {
					t.Errorf("hostKeyAlgorithms = %q, want %q", got, tc.wantAlgos)
				}
			}
			if creds.signer == nil {
				t.Error("signer is nil, want the parsed client key")
			}
		})
	}
}

func TestParseCredentialsFromKnownHostsKeyTypes(t *testing.T) {
	t.Parallel()

	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	edPub := publicKeyOf(t, edPriv)
	ecdsaPub, err := ssh.NewPublicKey(&ecdsaKey.PublicKey)
	if err != nil {
		t.Fatalf("ecdsa public key: %v", err)
	}
	rsaPub, err := ssh.NewPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatalf("rsa public key: %v", err)
	}

	tests := []struct {
		name       string
		knownHosts string
		wantAlgos  []string
	}{
		{
			name:       "only ed25519",
			knownHosts: knownHostsLine("gluster-1", edPub),
			wantAlgos:  []string{"ssh-ed25519"},
		},
		{
			name: "all types are ordered by the fixed preference order",
			knownHosts: strings.Join([]string{
				knownHostsLine("gluster-1", rsaPub),
				knownHostsLine("gluster-2", ecdsaPub),
				knownHostsLine("gluster-3", edPub),
			}, "\n"),
			wantAlgos: []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "rsa-sha2-512", "rsa-sha2-256"},
		},
		{
			name: "duplicates are collapsed",
			knownHosts: strings.Join([]string{
				knownHostsLine("gluster-1", edPub),
				knownHostsLine("gluster-2", edPub),
			}, "\n"),
			wantAlgos: []string{"ssh-ed25519"},
		},
		{
			name: "comments and empty lines are ignored",
			knownHosts: strings.Join([]string{
				"# a comment",
				"",
				knownHostsLine("gluster-1", edPub),
				"   ",
			}, "\n"),
			wantAlgos: []string{"ssh-ed25519"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := map[string]string{
				KeyUsername:   "gluster",
				KeyPrivateKey: testKeyPEM(t),
				KeyKnownHosts: tc.knownHosts,
			}
			creds, err := ParseCredentials(data)
			if err != nil {
				t.Fatalf("ParseCredentials = %v, want nil", err)
			}
			if got := strings.Join(creds.hostKeyAlgorithms, ","); got != strings.Join(tc.wantAlgos, ",") {
				t.Errorf("hostKeyAlgorithms = %q, want %q", got, tc.wantAlgos)
			}
		})
	}
}

func TestParseCredentialsRejectsInvalidData(t *testing.T) {
	t.Parallel()

	edPub := func(t *testing.T) ssh.PublicKey {
		t.Helper()
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		return publicKeyOf(t, priv)
	}

	tests := []struct {
		name      string
		mutate    func(*testing.T, map[string]string)
		wantInMsg []string
	}{
		{
			name:      "missing username",
			mutate:    func(_ *testing.T, d map[string]string) { delete(d, KeyUsername) },
			wantInMsg: []string{KeyUsername, "required"},
		},
		{
			name:      "empty username",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyUsername] = "" },
			wantInMsg: []string{KeyUsername, "required"},
		},
		{
			name:      "username with an upper case letter",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyUsername] = "Gluster" },
			wantInMsg: []string{KeyUsername},
		},
		{
			name:      "username with a dash at the start",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyUsername] = "-gluster" },
			wantInMsg: []string{KeyUsername},
		},
		{
			name:      "username with a space",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyUsername] = "glus ter" },
			wantInMsg: []string{KeyUsername},
		},
		{
			name:      "username too long",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyUsername] = strings.Repeat("a", 33) },
			wantInMsg: []string{KeyUsername},
		},
		{
			name:      "username with a slash",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyUsername] = "gluster/root" },
			wantInMsg: []string{KeyUsername},
		},
		{
			name:      "port zero",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyPort] = "0" },
			wantInMsg: []string{KeyPort},
		},
		{
			name:      "port above the limit",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyPort] = "65536" },
			wantInMsg: []string{KeyPort},
		},
		{
			name:      "port that is not a number",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyPort] = "abc" },
			wantInMsg: []string{KeyPort},
		},
		{
			name:      "negative port",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyPort] = "-22" },
			wantInMsg: []string{KeyPort},
		},
		{
			name:      "port with a trailing space",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyPort] = "22 " },
			wantInMsg: []string{KeyPort},
		},
		{
			name:      "missing private key",
			mutate:    func(_ *testing.T, d map[string]string) { delete(d, KeyPrivateKey) },
			wantInMsg: []string{KeyPrivateKey, "required"},
		},
		{
			name:      "private key that is only white space",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyPrivateKey] = " \n\t" },
			wantInMsg: []string{KeyPrivateKey, "required"},
		},
		{
			name: "broken private key with a marker",
			mutate: func(_ *testing.T, d map[string]string) {
				d[KeyPrivateKey] = "-----BEGIN OPENSSH PRIVATE KEY-----\n" + markerToken + "\n-----END OPENSSH PRIVATE KEY-----\n"
			},
			wantInMsg: []string{KeyPrivateKey, "not a valid unencrypted private key"},
		},
		{
			// ssh.ParsePrivateKey quotes the PEM block type in its error, so this
			// input makes the underlying error contain the marker. The message of
			// ParseCredentials must not.
			name: "private key whose PEM type names the marker",
			mutate: func(_ *testing.T, d map[string]string) {
				d[KeyPrivateKey] = "-----BEGIN " + markerToken + "-----\nAAAA\n-----END " + markerToken + "-----\n"
			},
			wantInMsg: []string{KeyPrivateKey},
		},
		{
			// ssh.ParseKnownHosts quotes the key type of a record in its error.
			name: "known_hosts whose key type names the marker",
			mutate: func(_ *testing.T, d map[string]string) {
				d[KeyKnownHosts] = "gluster-1 " + markerToken + " AAAAB3NzaC1yc2E=\n"
			},
			wantInMsg: []string{KeyKnownHosts, "line 1"},
		},
		{
			name:      "private key that is not PEM",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyPrivateKey] = markerToken },
			wantInMsg: []string{KeyPrivateKey},
		},
		{
			name:      "missing known_hosts",
			mutate:    func(_ *testing.T, d map[string]string) { delete(d, KeyKnownHosts) },
			wantInMsg: []string{KeyKnownHosts, "required"},
		},
		{
			name:      "known_hosts of only white space",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyKnownHosts] = "\n \n" },
			wantInMsg: []string{KeyKnownHosts, "required"},
		},
		{
			name:      "known_hosts of only a comment",
			mutate:    func(_ *testing.T, d map[string]string) { d[KeyKnownHosts] = "# " + markerToken + "\n" },
			wantInMsg: []string{KeyKnownHosts, "no host key record"},
		},
		{
			name: "known_hosts with a cert-authority marker",
			mutate: func(t *testing.T, d map[string]string) {
				d[KeyKnownHosts] = "@cert-authority " + knownHostsLine("gluster-1", edPub(t))
			},
			wantInMsg: []string{KeyKnownHosts, "cert-authority", "not supported"},
		},
		{
			name: "known_hosts with a revoked marker",
			mutate: func(t *testing.T, d map[string]string) {
				d[KeyKnownHosts] = "@revoked " + knownHostsLine("gluster-1", edPub(t))
			},
			wantInMsg: []string{KeyKnownHosts, "revoked", "not supported"},
		},
		{
			name: "known_hosts with a malformed line",
			mutate: func(t *testing.T, d map[string]string) {
				d[KeyKnownHosts] = knownHostsLine("gluster-1", edPub(t)) + "\ngluster-2 ssh-ed25519 " + markerToken + "\n"
			},
			wantInMsg: []string{KeyKnownHosts, "line 2"},
		},
		{
			name: "known_hosts with a line that has no key",
			mutate: func(t *testing.T, d map[string]string) {
				d[KeyKnownHosts] = knownHostsLine("gluster-1", edPub(t)) + "\ngluster-2\n"
			},
			wantInMsg: []string{KeyKnownHosts, "line 2"},
		},
		{
			name: "known_hosts with a key type that cannot be used",
			mutate: func(_ *testing.T, d map[string]string) {
				// A DSA record parses but yields no supported host key algorithm,
				// so no key type is left to offer.
				d[KeyKnownHosts] = knownHostsLine("gluster-1", dsaPublicKeyForTest(t))
			},
			wantInMsg: []string{KeyKnownHosts, "no supported host key type"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := validCredentialsData(t)
			tc.mutate(t, data)
			creds, err := ParseCredentials(data)
			if err == nil {
				t.Fatalf("ParseCredentials = %v, nil, want ErrInvalidCredentials", creds)
			}
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("ParseCredentials = %v, want errors.Is(err, ErrInvalidCredentials)", err)
			}
			msg := err.Error()
			for _, want := range tc.wantInMsg {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
			if strings.Contains(msg, markerToken) {
				t.Errorf("error %q contains the secret content", msg)
			}
			if key := data[KeyPrivateKey]; key != "" && strings.Contains(msg, key) {
				t.Errorf("error %q contains the private key", msg)
			}
		})
	}
}

func TestParseCredentialsDoesNotLeakTheUnderlyingParseError(t *testing.T) {
	t.Parallel()

	// ssh.ParsePrivateKey puts the PEM block type into its error message, which is
	// why ParseCredentials must not wrap that error: the message of a Secret value
	// would otherwise end up in a log line.
	broken := "-----BEGIN " + markerToken + "-----\nAAAA\n-----END " + markerToken + "-----\n"
	if _, parseErr := ssh.ParsePrivateKey([]byte(broken)); parseErr == nil || !strings.Contains(parseErr.Error(), markerToken) {
		t.Fatalf("ssh.ParsePrivateKey error = %v, want an error that quotes the marker", parseErr)
	}
	// The same holds for known_hosts: the parser quotes the declared key type when
	// it does not match the type inside the key blob.
	validKey := knownHostsKeyField(t, sshPubKeyForTest(t))
	brokenKnownHosts := "gluster-1 " + markerToken + " " + strings.SplitN(validKey, " ", 2)[1]
	if _, _, _, _, _, parseErr := ssh.ParseKnownHosts([]byte(brokenKnownHosts)); parseErr == nil || !strings.Contains(parseErr.Error(), markerToken) {
		t.Fatalf("ssh.ParseKnownHosts error = %v, want an error that quotes the marker", parseErr)
	}

	for _, tc := range []struct {
		name string
		key  string
		data map[string]string
	}{
		{
			name: "private key",
			data: map[string]string{
				KeyUsername:   "gluster",
				KeyPrivateKey: broken,
				KeyKnownHosts: knownHostsLine("gluster-1", sshPubKeyForTest(t)),
			},
		},
		{
			name: "known_hosts",
			data: map[string]string{
				KeyUsername:   "gluster",
				KeyPrivateKey: testKeyPEM(t),
				KeyKnownHosts: brokenKnownHosts,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseCredentials(tc.data)
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("ParseCredentials = %v, want ErrInvalidCredentials", err)
			}
			if strings.Contains(err.Error(), markerToken) {
				t.Fatalf("error %q contains the marker of the input", err)
			}
		})
	}
}

func TestParseCredentialsErrorNamesKeysNotValues(t *testing.T) {
	t.Parallel()

	data := map[string]string{
		KeyUsername:   "gluster",
		KeyPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\n" + markerToken + "\n-----END OPENSSH PRIVATE KEY-----\n",
		KeyKnownHosts: knownHostsLine("gluster-1.invalid", sshPubKeyForTest(t)),
	}
	_, err := ParseCredentials(data)
	if err == nil {
		t.Fatal("ParseCredentials = nil, want an error")
	}
	if strings.Contains(err.Error(), markerToken) {
		t.Errorf("error %q contains the private key input", err)
	}
	// The user name is not a secret, the private key and known_hosts are.
	if strings.Contains(err.Error(), data[KeyKnownHosts]) {
		t.Errorf("error %q contains known_hosts content", err)
	}
}

// sshPubKeyForTest returns a public key that is only used as known_hosts content.
func sshPubKeyForTest(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return publicKeyOf(t, priv)
}

func TestCredentialsFormattingHidesKeyMaterial(t *testing.T) {
	t.Parallel()

	data := validCredentialsData(t)
	creds, err := ParseCredentials(data)
	if err != nil {
		t.Fatalf("ParseCredentials = %v, want nil", err)
	}
	privateKey := data[KeyPrivateKey]
	knownHosts := data[KeyKnownHosts]
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		rendered := fmt.Sprintf(format, creds)
		if rendered != "username=gluster port=22" {
			t.Errorf("%s renders %q, want the summary only", format, rendered)
		}
		if strings.Contains(rendered, privateKey) || strings.Contains(rendered, knownHosts) {
			t.Errorf("%s leaks key material: %q", format, rendered)
		}
		if strings.Contains(rendered, "PRIVATE KEY") {
			t.Errorf("%s leaks the key header: %q", format, rendered)
		}
	}
	var nilCreds *Credentials
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if got := fmt.Sprintf(format, nilCreds); got != "<nil>" {
			t.Errorf("%s of a nil Credentials = %q, want %q", format, got, "<nil>")
		}
	}
}

func TestParseCredentialsRemovesTheTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	if _, err := ParseCredentials(validCredentialsData(t)); err != nil {
		t.Fatalf("ParseCredentials = %v, want nil", err)
	}
	assertDirEmpty(t, dir)

	// The same has to hold for a rejected content, which never reaches the
	// temporary file.
	data := validCredentialsData(t)
	data[KeyKnownHosts] = knownHostsLine("gluster-1", sshPubKeyForTest(t)) + "\nbroken line\n"
	if _, err := ParseCredentials(data); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ParseCredentials = %v, want ErrInvalidCredentials", err)
	}
	assertDirEmpty(t, dir)
}

// assertDirEmpty fails if dir contains anything.
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Fatalf("%s contains %d entries %v, want none", dir, len(entries), names)
	}
}

func TestWriteTempFileIsRemovable(t *testing.T) {
	t.Parallel()

	name, err := writeTempFile("glusterfs-csi-test-*", "content\n")
	if err != nil {
		t.Fatalf("writeTempFile = %v, want nil", err)
	}
	t.Cleanup(func() { _ = os.Remove(name) })

	info, err := os.Stat(name)
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if string(content) != "content\n" {
		t.Errorf("content = %q, want %q", content, "content\n")
	}
	if base := filepath.Base(name); !strings.HasPrefix(base, "glusterfs-csi-test-") {
		t.Errorf("name = %q, want the test prefix", base)
	}
}

func TestParseCredentialsUnknownKnownHostsMarker(t *testing.T) {
	t.Parallel()

	data := validCredentialsData(t)
	data[KeyKnownHosts] = "@some-future-marker gluster-1 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"
	_, err := ParseCredentials(data)
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ParseCredentials = %v, want ErrInvalidCredentials", err)
	}
	if !strings.Contains(err.Error(), "marker") {
		t.Errorf("error %q does not mention the marker", err)
	}
}

func TestParseCredentialsFailsWhenTheTemporaryFileCannotBeCreated(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-directory")
	t.Setenv("TMPDIR", missing)

	_, err := ParseCredentials(validCredentialsData(t))
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ParseCredentials = %v, want ErrInvalidCredentials", err)
	}
	if !strings.Contains(err.Error(), KeyKnownHosts) {
		t.Errorf("error %q does not name the key", err)
	}
}

func TestParseCredentialsFailsWhenTheHostKeyVerifierCannotBeBuilt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	data := validCredentialsData(t)
	// The line by line parser accepts a hashed host pattern, the knownhosts file
	// parser rejects the broken salt: this is the only content that reaches the
	// callback construction and fails there.
	data[KeyKnownHosts] = "|1|not-base64!|also-not-base64! " + knownHostsKeyField(t, sshPubKeyForTest(t)) + "\n"
	_, err := ParseCredentials(data)
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ParseCredentials = %v, want ErrInvalidCredentials", err)
	}
	if !strings.Contains(err.Error(), "host key verifier") {
		t.Errorf("error %q does not mention the host key verifier", err)
	}
	assertDirEmpty(t, dir)
}

// knownHostsKeyField returns "ssh-ed25519 <base64>" for key, which is the part of
// a known_hosts record after the host pattern.
func knownHostsKeyField(t *testing.T, key ssh.PublicKey) string {
	t.Helper()
	line := knownhosts.Line([]string{"placeholder"}, key)
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("knownhosts.Line returned %q, want at least two fields", line)
	}
	return strings.Join(fields[1:], " ")
}

func TestParseCredentialsRejectsUnsupportedKeyType(t *testing.T) {
	t.Parallel()

	// A DSA key parses, but the driver only supports ed25519, ecdsa and rsa.
	var params dsa.Parameters
	if err := dsa.GenerateParameters(&params, rand.Reader, dsa.L1024N160); err != nil {
		t.Fatalf("generate dsa parameters: %v", err)
	}
	key := &dsa.PrivateKey{PublicKey: dsa.PublicKey{Parameters: params}}
	if err := dsa.GenerateKey(key, rand.Reader); err != nil {
		t.Fatalf("generate dsa key: %v", err)
	}
	// ssh.MarshalPrivateKey does not support DSA, so the PEM is built directly from
	// the ASN.1 shape ssh.ParseDSAPrivateKey expects.
	der, err := asn1.Marshal(struct {
		Version int
		P, Q, G *big.Int
		Pub     *big.Int
		Priv    *big.Int
	}{0, key.P, key.Q, key.G, key.Y, key.X})
	if err != nil {
		t.Fatalf("marshal dsa key: %v", err)
	}
	data := validCredentialsData(t)
	data[KeyPrivateKey] = string(pem.EncodeToMemory(&pem.Block{Type: "DSA PRIVATE KEY", Bytes: der}))

	_, err = ParseCredentials(data)
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ParseCredentials = %v, want ErrInvalidCredentials", err)
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error %q does not say that the key type is not supported", err)
	}
}

func TestCheckKeyTypeRejectsUninspectableRSAKey(t *testing.T) {
	t.Parallel()

	if err := checkKeyType(typeOnlyKey(ssh.KeyAlgoRSA)); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("checkKeyType of an rsa key without CryptoPublicKey = %v, want ErrInvalidCredentials", err)
	}
	if err := checkKeyType(uninspectableKey{typ: ssh.KeyAlgoRSA}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("checkKeyType with a foreign crypto key = %v, want ErrInvalidCredentials", err)
	}
}

// uninspectableKey is an RSA typed public key whose CryptoPublicKey is not an
// *rsa.PublicKey.
type uninspectableKey struct {
	typ string
}

func (k uninspectableKey) Type() string                        { return k.typ }
func (k uninspectableKey) Marshal() []byte                     { return nil }
func (k uninspectableKey) Verify([]byte, *ssh.Signature) error { return errors.New("not implemented") }

func (k uninspectableKey) CryptoPublicKey() crypto.PublicKey {
	return ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
}

func TestReasonOfDropsThePath(t *testing.T) {
	t.Parallel()

	pathErr := &os.PathError{Op: "open", Path: "/tmp/secret-name", Err: errors.New("permission denied")}
	if got := reasonOf(pathErr); got != "permission denied" {
		t.Errorf("reasonOf(*os.PathError) = %q, want %q", got, "permission denied")
	}
	if got := reasonOf(errors.New("something else")); got != "something else" {
		t.Errorf("reasonOf(other error) = %q, want %q", got, "something else")
	}
}

// dsaPublicKeyForTest returns a DSA public key, which is only used as known_hosts
// content: it parses, but the driver offers no algorithm for that key type.
func dsaPublicKeyForTest(t *testing.T) ssh.PublicKey {
	t.Helper()
	var params dsa.Parameters
	if err := dsa.GenerateParameters(&params, rand.Reader, dsa.L1024N160); err != nil {
		t.Fatalf("generate dsa parameters: %v", err)
	}
	key := &dsa.PrivateKey{PublicKey: dsa.PublicKey{Parameters: params}}
	if err := dsa.GenerateKey(key, rand.Reader); err != nil {
		t.Fatalf("generate dsa key: %v", err)
	}
	pub, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("dsa public key: %v", err)
	}
	return pub
}

func TestCheckKeyTypeAllowsTheSupportedTypes(t *testing.T) {
	t.Parallel()

	edKey, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		// An all zero key is not a valid point, which does not matter for Type().
		t.Logf("ed25519 public key: %v", err)
	}
	_ = edKey
	for _, typ := range []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521} {
		if err := checkKeyType(typeOnlyKey(typ)); err != nil {
			t.Errorf("checkKeyType(%s) = %v, want nil", typ, err)
		}
	}
	if err := checkKeyType(typeOnlyKey("ssh-dss")); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("checkKeyType(ssh-dss) = %v, want ErrInvalidCredentials", err)
	}
}

// typeOnlyKey is a public key that only reports a type.
type typeOnlyKey string

func (k typeOnlyKey) Type() string                        { return string(k) }
func (k typeOnlyKey) Marshal() []byte                     { return nil }
func (k typeOnlyKey) Verify([]byte, *ssh.Signature) error { return errors.New("not implemented") }

func TestWriteTempFileRemovesTheFileOnEveryFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		create func(dir, pattern string) (tempFile, error)
	}{
		{
			name: "creating the file fails",
			create: func(string, string) (tempFile, error) {
				return nil, errors.New("no space left on device")
			},
		},
		{
			name: "writing fails",
			create: func(dir, pattern string) (tempFile, error) {
				f, err := os.CreateTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				// A closed file fails on Write, which is the failure this case needs.
				if closeErr := f.Close(); closeErr != nil {
					return nil, closeErr
				}
				return f, nil
			},
		},
		{
			name: "closing fails",
			create: func(dir, pattern string) (tempFile, error) {
				f, err := os.CreateTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				return closeFailingFile{f}, nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			name, err := writeTempFileWith("glusterfs-csi-test-*", "content", func(pattern string) (tempFile, error) {
				return tc.create(dir, pattern)
			})
			if err == nil {
				t.Fatalf("writeTempFileWith = %q, nil, want an error", name)
			}
			if name != "" {
				t.Errorf("writeTempFileWith returned the name %q with an error, want the empty string", name)
			}
			assertDirEmpty(t, dir)
		})
	}
}

// closeFailingFile is a tempFile whose Close fails after the content was written.
type closeFailingFile struct{ *os.File }

func (f closeFailingFile) Close() error {
	_ = f.File.Close()
	return errors.New("close failed")
}
