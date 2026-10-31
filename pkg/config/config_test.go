package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// validDoc renders a JSON document holding exactly the given clusters.
func validDoc(t *testing.T, clusters ...Cluster) string {
	t.Helper()
	data, err := json.Marshal(clusters)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	return string(data)
}

// maxLabelHostname returns an RFC 1123 host name of exactly n bytes (n >= 1),
// using as many 63 byte labels as possible.
func maxLabelHostname(n int) string {
	var b strings.Builder
	for b.Len() < n {
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		label := min(n-b.Len(), 63)
		b.WriteString(strings.Repeat("a", label))
	}
	return b.String()
}

func TestParse(t *testing.T) {
	secretRef := &SecretRef{Name: "glusterfs-ssh", Namespace: "glusterfs-csi"}

	tests := []struct {
		name string
		doc  string
		want map[string]Cluster
	}{
		{
			name: "single cluster with ipv4",
			doc:  `[{"clusterID":"prod","servers":["10.0.0.11"]}]`,
			want: map[string]Cluster{
				"prod": {ClusterID: "prod", Servers: []string{"10.0.0.11"}},
			},
		},
		{
			name: "single cluster without executorSecretRef",
			doc: validDoc(t, Cluster{
				ClusterID: "prod",
				Servers:   []string{"dxt-gluster-node-1", "dxt-gluster-node-2", "10.0.0.13"},
			}),
			want: map[string]Cluster{
				"prod": {ClusterID: "prod", Servers: []string{"dxt-gluster-node-1", "dxt-gluster-node-2", "10.0.0.13"}},
			},
		},
		{
			name: "single cluster with executorSecretRef",
			doc: validDoc(t, Cluster{
				ClusterID:         "prod",
				Servers:           []string{"gluster-1"},
				ExecutorSecretRef: secretRef,
			}),
			want: map[string]Cluster{
				"prod": {ClusterID: "prod", Servers: []string{"gluster-1"}, ExecutorSecretRef: &SecretRef{
					Name: "glusterfs-ssh", Namespace: "glusterfs-csi",
				}},
			},
		},
		{
			name: "multiple clusters keep their identity",
			doc: validDoc(t,
				Cluster{ClusterID: "zeta", Servers: []string{"10.0.0.30"}},
				Cluster{ClusterID: "alpha", Servers: []string{"10.0.0.10"}, ExecutorSecretRef: secretRef},
				Cluster{ClusterID: "prod", Servers: []string{"10.0.0.20"}},
			),
			want: map[string]Cluster{
				"zeta":  {ClusterID: "zeta", Servers: []string{"10.0.0.30"}},
				"alpha": {ClusterID: "alpha", Servers: []string{"10.0.0.10"}, ExecutorSecretRef: secretRef},
				"prod":  {ClusterID: "prod", Servers: []string{"10.0.0.20"}},
			},
		},
		{
			name: "ipv4, ipv6 and host names mixed, order preserved",
			doc:  `[{"clusterID":"mixed","servers":["10.0.0.13","2001:db8::1","dxt-gluster-node-1"]}]`,
			want: map[string]Cluster{
				"mixed": {ClusterID: "mixed", Servers: []string{"10.0.0.13", "2001:db8::1", "dxt-gluster-node-1"}},
			},
		},
		{
			name: "upper case host names and leading digit labels are valid",
			doc:  `[{"clusterID":"Prod_1","servers":["Gluster-1.Example.COM","1node.local"]}]`,
			want: map[string]Cluster{
				"Prod_1": {ClusterID: "Prod_1", Servers: []string{"Gluster-1.Example.COM", "1node.local"}},
			},
		},
		{
			name: "surrounding whitespace is accepted",
			doc:  "\n\t [ { \"clusterID\" : \"prod\" , \"servers\" : [ \"host\" ] } ] \n",
			want: map[string]Cluster{
				"prod": {ClusterID: "prod", Servers: []string{"host"}},
			},
		},
		{
			name: "exactly MaxServers servers",
			doc:  validDoc(t, Cluster{ClusterID: "prod", Servers: manyServers(MaxServers)}),
			want: map[string]Cluster{
				"prod": {ClusterID: "prod", Servers: manyServers(MaxServers)},
			},
		},
		{
			name: "63 byte label and 253 byte host name",
			doc: validDoc(t, Cluster{
				ClusterID: "prod",
				Servers:   []string{strings.Repeat("a", 63), maxLabelHostname(253)},
			}),
			want: map[string]Cluster{
				"prod": {ClusterID: "prod", Servers: []string{strings.Repeat("a", 63), maxLabelHostname(253)}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.doc))
			if err != nil {
				t.Fatalf("Parse() = %v, want nil", err)
			}
			if got := len(cfg.ClusterIDs()); got != len(tc.want) {
				t.Fatalf("ClusterIDs() = %v, want %d entries", cfg.ClusterIDs(), len(tc.want))
			}
			for id, want := range tc.want {
				got, ok := cfg.Get(id)
				if !ok {
					t.Fatalf("Get(%q) not found, want %+v", id, want)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("Get(%q) = %+v, want %+v", id, got, want)
				}
			}
		})
	}
}

// manyServers returns n distinct servers.
func manyServers(n int) []string {
	servers := make([]string, 0, n)
	for i := 0; i < n; i++ {
		servers = append(servers, fmt.Sprintf("10.0.0.%d", i))
	}
	return servers
}

func TestParseClusterIDsSorted(t *testing.T) {
	cfg, err := Parse([]byte(validDoc(t,
		Cluster{ClusterID: "zeta", Servers: []string{"host-1"}},
		Cluster{ClusterID: "Alpha", Servers: []string{"host-2"}},
		Cluster{ClusterID: "prod", Servers: []string{"host-3"}},
	)))
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	// Byte order: upper case sorts before lower case.
	want := []string{"Alpha", "prod", "zeta"}
	if got := cfg.ClusterIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ClusterIDs() = %v, want %v", got, want)
	}
}

func TestParseInvalid(t *testing.T) {
	tooManyServers := validDoc(t, Cluster{ClusterID: "prod", Servers: manyServers(MaxServers + 1)})
	longHostname := maxLabelHostname(254)

	tests := []struct {
		name    string
		doc     string
		wantMsg string
	}{
		{name: "empty input", doc: "", wantMsg: "document is empty"},
		{name: "only whitespace", doc: " \n\t ", wantMsg: "document is empty"},
		{name: "object instead of array", doc: `{"clusterID":"prod"}`, wantMsg: "must be a JSON array"},
		{name: "empty array", doc: `[]`, wantMsg: "no clusters defined"},
		{name: "trailing array after array", doc: `[{"clusterID":"prod","servers":["h"]}] []`, wantMsg: "unexpected content after"},
		{name: "trailing garbage after array", doc: `[{"clusterID":"prod","servers":["h"]}]x`, wantMsg: "unexpected content after"},
		{name: "unknown field server", doc: `[{"clusterID":"prod","server":["h"]}]`, wantMsg: `unknown field "server"`},
		{name: "unknown field in secret ref",
			doc:     `[{"clusterID":"prod","servers":["h"],"executorSecretRef":{"name":"a","namespace":"b","key":"k"}}]`,
			wantMsg: `unknown field "key"`},
		{name: "element is not an object", doc: `["prod"]`, wantMsg: "clusters[0]"},
		{name: "clusterID missing", doc: `[{"servers":["h"]}]`, wantMsg: "clusters[0]"},
		{name: "clusterID empty", doc: `[{"clusterID":"","servers":["h"]}]`, wantMsg: "must not be empty"},
		{name: "clusterID dot dot", doc: `[{"clusterID":"..","servers":["h"]}]`, wantMsg: "relative path element"},
		{name: "clusterID leading dash", doc: `[{"clusterID":"-x","servers":["h"]}]`, wantMsg: "must not start with"},
		{name: "clusterID semicolon", doc: `[{"clusterID":"a;b","servers":["h"]}]`, wantMsg: "must match"},
		{name: "clusterID duplicate", doc: `[{"clusterID":"prod","servers":["h1"]},{"clusterID":"prod","servers":["h2"]}]`,
			wantMsg: `clusters[1] (clusterID "prod"): duplicate clusterID`},
		{name: "servers missing", doc: `[{"clusterID":"prod"}]`, wantMsg: "at least 1 server is required"},
		{name: "servers null", doc: `[{"clusterID":"prod","servers":null}]`, wantMsg: "at least 1 server is required"},
		{name: "servers empty array", doc: `[{"clusterID":"prod","servers":[]}]`, wantMsg: "at least 1 server is required"},
		{name: "too many servers", doc: tooManyServers, wantMsg: "exceeds the maximum of 32"},
		{name: "empty server", doc: `[{"clusterID":"prod","servers":["ok",""]}]`, wantMsg: `servers[1] ""`},
		{name: "server with space", doc: `[{"clusterID":"prod","servers":["ok","a b"]}]`,
			wantMsg: `clusters[0] (clusterID "prod"): servers[1] "a b": not a valid hostname or IP address`},
		{name: "server with port", doc: `[{"clusterID":"prod","servers":["host:24007"]}]`, wantMsg: "not a valid hostname"},
		{name: "server leading dash", doc: `[{"clusterID":"prod","servers":["-host"]}]`, wantMsg: "not a valid hostname"},
		{name: "server trailing dash", doc: `[{"clusterID":"prod","servers":["host-"]}]`, wantMsg: "not a valid hostname"},
		{name: "server empty label", doc: `[{"clusterID":"prod","servers":["a..b"]}]`, wantMsg: "not a valid hostname"},
		{name: "server with leading dot", doc: `[{"clusterID":"prod","servers":[".host"]}]`, wantMsg: "not a valid hostname"},
		{name: "server with trailing dot", doc: `[{"clusterID":"prod","servers":["host."]}]`, wantMsg: "not a valid hostname"},
		{name: "server label too long", doc: `[{"clusterID":"prod","servers":["` + strings.Repeat("a", 64) + `"]}]`,
			wantMsg: "not a valid hostname"},
		{name: "server host name too long", doc: `[{"clusterID":"prod","servers":["` + longHostname + `"]}]`,
			wantMsg: "not a valid hostname"},
		{name: "server with ipv6 zone", doc: `[{"clusterID":"prod","servers":["fe80::1%eth0"]}]`,
			wantMsg: "not a valid hostname"},
		{name: "server with scheme", doc: `[{"clusterID":"prod","servers":["http://host"]}]`, wantMsg: "not a valid hostname"},
		{name: "server with slash", doc: `[{"clusterID":"prod","servers":["10.0.0.11/24"]}]`, wantMsg: "not a valid hostname"},
		{name: "servers differ only in case", doc: `[{"clusterID":"prod","servers":["Host-1","host-1"]}]`,
			wantMsg: "host names are case insensitive"},
		{name: "secret ref without name",
			doc:     `[{"clusterID":"prod","servers":["h"],"executorSecretRef":{"namespace":"glusterfs-csi"}}]`,
			wantMsg: "name is required"},
		{name: "secret ref without namespace",
			doc:     `[{"clusterID":"prod","servers":["h"],"executorSecretRef":{"name":"glusterfs-ssh"}}]`,
			wantMsg: "namespace is required"},
		{name: "secret ref name with upper case",
			doc:     `[{"clusterID":"prod","servers":["h"],"executorSecretRef":{"name":"Glusterfs-SSH","namespace":"ns"}}]`,
			wantMsg: "not a valid DNS-1123 subdomain"},
		{name: "secret ref name with underscore",
			doc:     `[{"clusterID":"prod","servers":["h"],"executorSecretRef":{"name":"glusterfs_ssh","namespace":"ns"}}]`,
			wantMsg: "not a valid DNS-1123 subdomain"},
		{name: "secret ref namespace with dot",
			doc:     `[{"clusterID":"prod","servers":["h"],"executorSecretRef":{"name":"glusterfs-ssh","namespace":"a.b"}}]`,
			wantMsg: "not a valid DNS-1123 label"},
		{name: "secret ref namespace with upper case",
			doc:     `[{"clusterID":"prod","servers":["h"],"executorSecretRef":{"name":"glusterfs-ssh","namespace":"NS"}}]`,
			wantMsg: "not a valid DNS-1123 label"},
		{name: "not json", doc: `[{"clusterID":`, wantMsg: "must be a JSON array"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatalf("Parse() = %+v, want error", cfg)
			}
			if cfg != nil {
				t.Fatalf("Parse() returned a config alongside the error %v", err)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse() error = %v, want errors.Is(err, ErrInvalid)", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("Parse() error = %q, want it to contain %q", err, tc.wantMsg)
			}
		})
	}
}

// TestParseErrorDoesNotEchoDocument makes sure a validation error locates the
// element without dumping the whole file, which would leak into the logs.
func TestParseErrorDoesNotEchoDocument(t *testing.T) {
	doc := `[{"clusterID":"prod","servers":["10.0.0.1","` + strings.Repeat("x", 64) + `","secret-looking-value"]}]`
	_, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse() = nil, want error")
	}
	if strings.Contains(err.Error(), "secret-looking-value") {
		t.Fatalf("error %q echoes later servers", err)
	}
}

// TestOversizedValuesAreNotEchoed makes sure that a user supplied value that is
// too long to be useful (a single JSON string may be almost MaxFileSize bytes)
// is reported by its length only, so that an error message can never become a
// huge log line.
func TestOversizedValuesAreNotEchoed(t *testing.T) {
	// A repeating pattern the surrounding message text cannot produce by
	// accident, so any 64 byte run found in the error really came from the
	// value itself.
	const (
		pattern = "x9"
		size    = 300
	)
	long := strings.Repeat(pattern, size/len(pattern))

	tests := []struct {
		name string
		doc  string
	}{
		{
			name: "server",
			doc:  `[{"clusterID":"prod","servers":["` + long + `"]}]`,
		},
		{
			name: "secret name",
			doc: `[{"clusterID":"prod","servers":["h"],` +
				`"executorSecretRef":{"name":"` + long + `","namespace":"glusterfs-csi"}}]`,
		},
		{
			name: "secret namespace",
			doc: `[{"clusterID":"prod","servers":["h"],` +
				`"executorSecretRef":{"name":"glusterfs-ssh","namespace":"` + long + `"}}]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatal("Parse() = nil, want error")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse() error = %v, want errors.Is(err, ErrInvalid)", err)
			}
			msg := err.Error()
			if want := fmt.Sprintf("length %d", size); !strings.Contains(msg, want) {
				t.Fatalf("Parse() error = %q, want it to contain %q", msg, want)
			}
			for i := 0; i+64 <= len(long); i++ {
				if strings.Contains(msg, long[i:i+64]) {
					t.Fatalf("Parse() error echoes the oversized value: %q", msg)
				}
			}
		})
	}
}

func TestGetReturnsCopy(t *testing.T) {
	cfg, err := Parse([]byte(`[{"clusterID":"prod","servers":["10.0.0.1","10.0.0.2"],` +
		`"executorSecretRef":{"name":"glusterfs-ssh","namespace":"glusterfs-csi"}}]`))
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}

	first, ok := cfg.Get("prod")
	if !ok {
		t.Fatal("Get(prod) not found")
	}
	first.Servers[0] = "tampered"
	first.ExecutorSecretRef.Name = "tampered"
	first.Servers = append(first.Servers, "extra")

	second, ok := cfg.Get("prod")
	if !ok {
		t.Fatal("Get(prod) not found on the second call")
	}
	want := Cluster{
		ClusterID:         "prod",
		Servers:           []string{"10.0.0.1", "10.0.0.2"},
		ExecutorSecretRef: &SecretRef{Name: "glusterfs-ssh", Namespace: "glusterfs-csi"},
	}
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("Get(prod) after mutating the copy = %+v, want %+v", second, want)
	}
}

func TestGetUnknownCluster(t *testing.T) {
	cfg, err := Parse([]byte(`[{"clusterID":"prod","servers":["h"]}]`))
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	if got, ok := cfg.Get("dev"); ok || got.ClusterID != "" || got.Servers != nil {
		t.Fatalf("Get(dev) = %+v, %v, want zero value and false", got, ok)
	}
}

func TestCurrentIsNeverNil(t *testing.T) {
	// Even a zero Store must hand out a usable snapshot instead of nil.
	var s Store
	cfg := s.Current()
	if cfg == nil {
		t.Fatal("Current() = nil, want an empty snapshot")
	}
	if ids := cfg.ClusterIDs(); ids != nil {
		t.Fatalf("ClusterIDs() = %v, want nil", ids)
	}
	if cl, ok := cfg.Get("prod"); ok || cl.ClusterID != "" {
		t.Fatalf("Get(prod) = %+v, %v, want zero value and false", cl, ok)
	}
}

func TestNilConfigAccessors(t *testing.T) {
	var cfg *Config
	if got, ok := cfg.Get("prod"); ok || got.ClusterID != "" {
		t.Fatalf("Get(prod) on a nil Config = %+v, %v, want zero value and false", got, ok)
	}
	if ids := cfg.ClusterIDs(); ids != nil {
		t.Fatalf("ClusterIDs() on a nil Config = %v, want nil", ids)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()

	valid := filepath.Join(dir, "config.json")
	writeFile(t, valid, `[{"clusterID":"prod","servers":["10.0.0.1"]}]`)

	doc := `[{"clusterID":"prod","servers":["h"]}]`
	atLimit := filepath.Join(dir, "limit.json")
	writeFile(t, atLimit, doc+strings.Repeat(" ", MaxFileSize-len(doc)))

	tooLarge := filepath.Join(dir, "large.json")
	writeFile(t, tooLarge, doc+strings.Repeat(" ", MaxFileSize+1-len(doc)))

	t.Run("valid", func(t *testing.T) {
		cfg, err := Load(valid)
		if err != nil {
			t.Fatalf("Load() = %v, want nil", err)
		}
		if _, ok := cfg.Get("prod"); !ok {
			t.Fatalf("Get(prod) not found in %v", cfg.ClusterIDs())
		}
	})

	t.Run("exactly at the limit", func(t *testing.T) {
		cfg, err := Load(atLimit)
		if err != nil {
			t.Fatalf("Load() = %v, want nil", err)
		}
		if ids := cfg.ClusterIDs(); !reflect.DeepEqual(ids, []string{"prod"}) {
			t.Fatalf("ClusterIDs() = %v, want [prod]", ids)
		}
	})

	t.Run("too large", func(t *testing.T) {
		if _, err := Load(tooLarge); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Load() error = %v, want errors.Is(err, ErrInvalid)", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := Load(filepath.Join(dir, "nope.json"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Load() error = %v, want errors.Is(err, fs.ErrNotExist)", err)
		}
		if errors.Is(err, ErrInvalid) {
			t.Fatalf("Load() error = %v, must not wrap ErrInvalid", err)
		}
	})

	t.Run("unreadable path", func(t *testing.T) {
		// A directory cannot be read as a file: the cause must survive instead
		// of being reported as invalid content.
		_, err := Load(dir)
		if err == nil {
			t.Fatal("Load(directory) = nil, want error")
		}
		if errors.Is(err, ErrInvalid) {
			t.Fatalf("Load(directory) error = %v, must not wrap ErrInvalid", err)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		if _, err := Load(""); err == nil {
			t.Fatal("Load(\"\") = nil, want error")
		}
	})
}

// writeFile writes content to path, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
