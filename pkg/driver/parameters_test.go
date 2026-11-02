package driver

import (
	"errors"
	"strings"
	"testing"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

// oversizedParam is longer than volumeid.MaxLength, so it is not a usable name
// and must never be echoed in an error message.
var oversizedParam = strings.Repeat("x", volumeid.MaxLength+1)

func TestParseVolumeParameters(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]string
		want   volumeParameters
		// wantErr are substrings the joined error message must contain; an
		// empty list means the parameters are valid.
		wantErr []string
		// wantNotErr are substrings that must not appear in the message, for
		// the cases that check a problem is not reported spuriously.
		wantNotErr []string
	}{
		{
			name:   "minimal valid parameters use the defaults",
			params: map[string]string{paramClusterID: "c1", paramParentVolume: "vol"},
			want: volumeParameters{
				ClusterID:    "c1",
				Mode:         volumeid.ModeSubdir,
				ParentVolume: "vol",
				OnDelete:     deletePolicyDelete,
			},
		},
		{
			name: "explicit subdir mode and delete policy",
			params: map[string]string{
				paramClusterID:        "c1",
				paramProvisioningMode: "subdir",
				paramParentVolume:     "vol",
				paramOnDelete:         "delete",
			},
			want: volumeParameters{
				ClusterID:    "c1",
				Mode:         volumeid.ModeSubdir,
				ParentVolume: "vol",
				OnDelete:     deletePolicyDelete,
			},
		},
		{
			name: "retain delete policy",
			params: map[string]string{
				paramClusterID:    "c1",
				paramParentVolume: "vol",
				paramOnDelete:     "retain",
			},
			want: volumeParameters{
				ClusterID:    "c1",
				Mode:         volumeid.ModeSubdir,
				ParentVolume: "vol",
				OnDelete:     deletePolicyRetain,
			},
		},
		{
			name: "archive delete policy",
			params: map[string]string{
				paramClusterID:    "c1",
				paramParentVolume: "vol",
				paramOnDelete:     "archive",
			},
			want: volumeParameters{
				ClusterID:    "c1",
				Mode:         volumeid.ModeSubdir,
				ParentVolume: "vol",
				OnDelete:     deletePolicyArchive,
			},
		},
		{
			name: "disabled quota changes nothing",
			params: map[string]string{
				paramClusterID:    "c1",
				paramParentVolume: "vol",
				paramQuota:        "false",
			},
			want: volumeParameters{
				ClusterID:    "c1",
				Mode:         volumeid.ModeSubdir,
				ParentVolume: "vol",
				OnDelete:     deletePolicyDelete,
			},
		},
		{
			name: "sidecar parameters are ignored",
			params: map[string]string{
				paramClusterID:                     "c1",
				paramParentVolume:                  "vol",
				"csi.storage.k8s.io/pvc/name":      "pvc-1",
				"csi.storage.k8s.io/pvc/namespace": "default",
				"csi.storage.k8s.io/pv/name":       "pv-1",
			},
			want: volumeParameters{
				ClusterID:    "c1",
				Mode:         volumeid.ModeSubdir,
				ParentVolume: "vol",
				OnDelete:     deletePolicyDelete,
			},
		},
		{
			name:    "empty parameter set",
			params:  nil,
			wantErr: []string{"clusterID is required", `parentVolume is required in mode "subdir"`},
		},
		{
			name:    "missing clusterID",
			params:  map[string]string{paramParentVolume: "vol"},
			wantErr: []string{"clusterID is required"},
		},
		{
			name:    "missing parentVolume",
			params:  map[string]string{paramClusterID: "c1"},
			wantErr: []string{`parentVolume is required in mode "subdir"`},
		},
		{
			name:    "empty clusterID is provided but invalid",
			params:  map[string]string{paramClusterID: "", paramParentVolume: "vol"},
			wantErr: []string{"clusterID", "must not be empty"},
		},
		{
			name:    "empty parentVolume is provided but invalid",
			params:  map[string]string{paramClusterID: "c1", paramParentVolume: ""},
			wantErr: []string{"parentVolume", "must not be empty"},
		},
		{
			name:    "clusterID with a slash",
			params:  map[string]string{paramClusterID: "a/b", paramParentVolume: "vol"},
			wantErr: []string{`invalid clusterID "a/b"`},
		},
		{
			name:    "clusterID with a path traversal",
			params:  map[string]string{paramClusterID: "..", paramParentVolume: "vol"},
			wantErr: []string{"clusterID", "relative path element"},
		},
		{
			name:    "clusterID with a space",
			params:  map[string]string{paramClusterID: "a b", paramParentVolume: "vol"},
			wantErr: []string{"invalid clusterID"},
		},
		{
			name:    "parentVolume with a path traversal",
			params:  map[string]string{paramClusterID: "c1", paramParentVolume: "../vol"},
			wantErr: []string{"invalid parentVolume"},
		},
		{
			name:    "oversized clusterID is not echoed",
			params:  map[string]string{paramClusterID: oversizedParam, paramParentVolume: "vol"},
			wantErr: []string{"invalid clusterID", "length"},
			wantNotErr: []string{
				oversizedParam,
			},
		},
		{
			name:    "static mode is refused",
			params:  map[string]string{paramClusterID: "c1", paramProvisioningMode: "static"},
			wantErr: []string{"static volumes are not provisioned, create the PersistentVolume by hand"},
			// The parent volume is only required by the subdir mode.
			wantNotErr: []string{"parentVolume is required"},
		},
		{
			name:    "volume mode is not supported yet",
			params:  map[string]string{paramClusterID: "c1", paramProvisioningMode: "volume", paramParentVolume: "vol"},
			wantErr: []string{`provisioningMode "volume" is not supported yet`},
		},
		{
			name:    "provisioningMode is case sensitive",
			params:  map[string]string{paramClusterID: "c1", paramProvisioningMode: "Subdir", paramParentVolume: "vol"},
			wantErr: []string{`must be "subdir", got "Subdir"`},
		},
		{
			name:    "empty provisioningMode",
			params:  map[string]string{paramClusterID: "c1", paramProvisioningMode: "", paramParentVolume: "vol"},
			wantErr: []string{`must be "subdir"`},
		},
		{
			name:    "onDelete is case sensitive",
			params:  map[string]string{paramClusterID: "c1", paramParentVolume: "vol", paramOnDelete: "Delete"},
			wantErr: []string{`must be one of "delete", "retain", "archive", got "Delete"`},
		},
		{
			name:    "unknown onDelete value",
			params:  map[string]string{paramClusterID: "c1", paramParentVolume: "vol", paramOnDelete: "remove"},
			wantErr: []string{"onDelete", "must be one of"},
		},
		{
			name:    "quota true is not supported yet",
			params:  map[string]string{paramClusterID: "c1", paramParentVolume: "vol", paramQuota: "true"},
			wantErr: []string{`quota`, `"true" is not supported yet`},
		},
		{
			name:    "quota is not a boolean",
			params:  map[string]string{paramClusterID: "c1", paramParentVolume: "vol", paramQuota: "yes"},
			wantErr: []string{`quota`, `must be "true" or "false", got "yes"`},
		},
		{
			name:    "known key with the wrong case is unknown",
			params:  map[string]string{"clusterid": "c1", paramParentVolume: "vol"},
			wantErr: []string{`unknown parameter "clusterid"`, "clusterID is required"},
		},
		{
			name:    "unknown parameter",
			params:  map[string]string{paramClusterID: "c1", paramParentVolume: "vol", "foo": "bar"},
			wantErr: []string{`unknown parameter "foo"`},
		},
		{
			name:    "oversized unknown key is not echoed",
			params:  map[string]string{paramClusterID: "c1", paramParentVolume: "vol", oversizedParam: "v"},
			wantErr: []string{"unknown parameter of length"},
			wantNotErr: []string{
				oversizedParam,
			},
		},
		{
			name: "every problem is reported",
			params: map[string]string{
				paramClusterID:    "c1",
				paramParentVolume: "../vol",
				paramOnDelete:     "nope",
				paramQuota:        "true",
				"foo":             "bar",
			},
			wantErr: []string{
				"invalid parentVolume",
				"onDelete",
				"quota",
				`unknown parameter "foo"`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVolumeParameters(tc.params)

			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("parseVolumeParameters() = %v, want nil", err)
				}
				if got != tc.want {
					t.Fatalf("parseVolumeParameters() = %+v, want %+v", got, tc.want)
				}
				return
			}

			if err == nil {
				t.Fatalf("parseVolumeParameters() = %+v, nil, want an error", got)
			}
			if got != (volumeParameters{}) {
				t.Fatalf("parseVolumeParameters() = %+v with an error, want the zero value", got)
			}
			if !errors.Is(err, errInvalidParameters) {
				t.Fatalf("errors.Is(%v, errInvalidParameters) = false, want true", err)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("parseVolumeParameters() = %q, want it to contain %q", err, want)
				}
			}
			for _, notWant := range tc.wantNotErr {
				if strings.Contains(err.Error(), notWant) {
					t.Fatalf("parseVolumeParameters() = %q, must not contain %q", err, notWant)
				}
			}

			// The message must be deterministic: the same input always produces
			// the same text, which is what makes it assertable in a test and
			// comparable in a log.
			again, againErr := parseVolumeParameters(tc.params)
			if again != (volumeParameters{}) {
				t.Fatalf("second parseVolumeParameters() = %+v, want the zero value", again)
			}
			if againErr == nil || againErr.Error() != err.Error() {
				t.Fatalf("second parseVolumeParameters() = %v, want %q", againErr, err)
			}
		})
	}
}

// TestParseVolumeParametersKeyOrderIndependence checks that the message does not
// depend on the iteration order of the map: two maps with the same content in a
// different insertion order produce the same message.
func TestParseVolumeParametersKeyOrderIndependence(t *testing.T) {
	first := map[string]string{
		"bogus":           "1",
		paramOnDelete:     "nope",
		paramParentVolume: "../vol",
		paramClusterID:    "c1",
	}
	second := map[string]string{
		paramClusterID:    "c1",
		paramParentVolume: "../vol",
		paramOnDelete:     "nope",
		"bogus":           "1",
	}

	_, firstErr := parseVolumeParameters(first)
	_, secondErr := parseVolumeParameters(second)
	if firstErr == nil || secondErr == nil {
		t.Fatalf("parseVolumeParameters() = %v, %v, want two errors", firstErr, secondErr)
	}
	if firstErr.Error() != secondErr.Error() {
		t.Fatalf("messages differ:\n%s\n%s", firstErr, secondErr)
	}
}

// TestParseVolumeParametersWrapsEveryProblem checks that each individual problem
// is reachable in the error tree, not only the sentinel.
func TestParseVolumeParametersWrapsEveryProblem(t *testing.T) {
	_, err := parseVolumeParameters(map[string]string{
		paramClusterID:    "c1",
		paramParentVolume: "vol",
		paramQuota:        "true",
		"foo":             "bar",
	})

	if !errors.Is(err, errInvalidParameters) {
		t.Fatalf("errors.Is(%v, errInvalidParameters) = false, want true", err)
	}
	// The error carries both the sentinel and the joined problems, so it
	// implements both unwrapping forms; errors.Join is the multi one.
	multi, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("%T does not implement Unwrap() []error, want the fmt.Errorf wrapping of errors.Join", err)
	}
	children := multi.Unwrap()
	if len(children) != 2 {
		t.Fatalf("Unwrap() = %v, want the sentinel and the joined problems", children)
	}
	if !errors.Is(children[0], errInvalidParameters) {
		t.Fatalf("Unwrap()[0] = %v, want errInvalidParameters", children[0])
	}
	joined := children[1]
	for _, want := range []string{"quota", "foo"} {
		if !strings.Contains(joined.Error(), want) {
			t.Fatalf("joined problems = %q, want it to mention %q", joined, want)
		}
	}
}

// TestSortedParamKeys checks the deterministic key order the messages rely on.
func TestSortedParamKeys(t *testing.T) {
	got := sortedParamKeys(map[string]string{"c": "3", "a": "1", "b": "2"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("sortedParamKeys() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedParamKeys() = %v, want %v", got, want)
		}
	}

	if got := sortedParamKeys(nil); len(got) != 0 {
		t.Fatalf("sortedParamKeys(nil) = %v, want an empty slice", got)
	}
}

// TestEchoParamValue checks the length cutoff of the echoed values.
func TestEchoParamValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "short value is quoted", value: "a b", want: `"a b"`},
		{name: "empty value stays visible", value: "", want: `""`},
		{
			name:  "value at the limit is echoed",
			value: strings.Repeat("y", volumeid.MaxLength),
			want:  `"` + strings.Repeat("y", volumeid.MaxLength) + `"`,
		},
		{
			name:  "oversized value is replaced by its length",
			value: oversizedParam,
			want:  "a value of length 129",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := echoParamValue(tc.value); got != tc.want {
				t.Fatalf("echoParamValue(%d bytes) = %q, want %q", len(tc.value), got, tc.want)
			}
		})
	}

	if got, want := echoParamKey(oversizedParam), "of length 129"; got != want {
		t.Fatalf("echoParamKey(oversized) = %q, want %q", got, want)
	}
	if got, want := echoParamKey("foo"), `"foo"`; got != want {
		t.Fatalf("echoParamKey(\"foo\") = %q, want %q", got, want)
	}
}
