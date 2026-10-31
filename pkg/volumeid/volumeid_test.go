package volumeid

import (
	"errors"
	"strings"
	"testing"
)

// largeName is 128 characters long, i.e. exactly at the limit.
var largeName = strings.Repeat("a", MaxLength)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "single letter", value: "a"},
		{name: "mixed whitelist characters", value: "A-b_c.1"},
		{name: "dots in the middle", value: "a..b"},
		{name: "exactly max length", value: largeName},
		{name: "all digits", value: "1234567890"},
		{name: "empty", value: "", wantErr: true},
		{name: "one over max length", value: largeName + "a", wantErr: true},
		{name: "dot", value: ".", wantErr: true},
		{name: "dot dot", value: "..", wantErr: true},
		{name: "leading dash", value: "-x", wantErr: true},
		{name: "long flag", value: "--help", wantErr: true},
		{name: "slash", value: "a/b", wantErr: true},
		{name: "traversal", value: "a/../../b", wantErr: true},
		{name: "space", value: "a b", wantErr: true},
		{name: "colon", value: "a:b", wantErr: true},
		{name: "semicolon", value: "a;b", wantErr: true},
		{name: "command substitution", value: "$(id)", wantErr: true},
		{name: "backtick", value: "a`id`b", wantErr: true},
		{name: "newline", value: "a\nb", wantErr: true},
		{name: "nul byte", value: "a\x00b", wantErr: true},
		{name: "non ascii", value: "ä", wantErr: true},
		{name: "glob", value: "*", wantErr: true},
		{name: "pipe", value: "a|b", wantErr: true},
		{name: "quote", value: "a'b", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName("volume", tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateName(%q) = nil, want error", tc.value)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("ValidateName(%q) error = %v, want errors.Is(err, ErrInvalid)", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateName(%q) = %v, want nil", tc.value, err)
			}
		})
	}
}

func TestValidateNameErrorMessages(t *testing.T) {
	tests := []struct {
		name      string
		field     string
		value     string
		wantParts []string
	}{
		{
			name:      "regular field name and value are reported",
			field:     "volume",
			value:     "a/b",
			wantParts: []string{"invalid volume", `"a/b"`},
		},
		{
			name:      "cluster id is reported",
			field:     "clusterID",
			value:     ".",
			wantParts: []string{"invalid clusterID", `"."`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName(tc.field, tc.value)
			if err == nil {
				t.Fatalf("ValidateName(%q, %q) = nil, want error", tc.field, tc.value)
			}
			msg := err.Error()
			for _, part := range tc.wantParts {
				if !strings.Contains(msg, part) {
					t.Fatalf("error message %q does not contain %q", msg, part)
				}
			}
			if !strings.HasPrefix(msg, "invalid ") {
				t.Fatalf("error message %q must start with %q", msg, "invalid ")
			}
		})
	}
}

func TestValidateNameOversizedValueIsNotEchoed(t *testing.T) {
	value := strings.Repeat("z", 300)

	err := ValidateName("volume", value)
	if err == nil {
		t.Fatal("ValidateName(300 chars) = nil, want error")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want errors.Is(err, ErrInvalid)", err)
	}

	msg := err.Error()
	if strings.Contains(msg, value) {
		t.Fatalf("error message %q must not contain the oversized value", msg)
	}
	if strings.Contains(msg, "zzz") {
		t.Fatalf("error message %q must not contain any part of the oversized value", msg)
	}
	if !strings.Contains(msg, "300") || !strings.Contains(msg, "128") {
		t.Fatalf("error message %q must report the actual length 300 and the limit 128", msg)
	}
	if !strings.Contains(msg, "invalid volume") {
		t.Fatalf("error message %q must name the field", msg)
	}
}

// encodeCases is shared by TestEncode, TestRoundTrip and TestDecode so that the
// canonical strings asserted by all of them cannot drift apart.
var encodeCases = []struct {
	name string
	id   ID
	want string
}{
	{
		name: "static without subdir",
		id:   ID{Mode: ModeStatic, ClusterID: "prod", Volume: "shared-vol"},
		want: "gfs:v1:static:prod:shared-vol",
	},
	{
		name: "static with subdir",
		id:   ID{Mode: ModeStatic, ClusterID: "prod", Volume: "shared-vol", Subdir: "pv-1"},
		want: "gfs:v1:static:prod:shared-vol:pv-1",
	},
	{
		name: "subdir",
		id:   ID{Mode: ModeSubdir, ClusterID: "prod", Volume: "parent", Subdir: "pvc-abc"},
		want: "gfs:v1:subdir:prod:parent:pvc-abc",
	},
	{
		name: "volume",
		id:   ID{Mode: ModeVolume, ClusterID: "prod", Volume: "pvc-abc"},
		want: "gfs:v1:volume:prod:pvc-abc",
	},
	{
		name: "single character fields",
		id:   ID{Mode: ModeVolume, ClusterID: "a", Volume: "b"},
		want: "gfs:v1:volume:a:b",
	},
	{
		name: "dots and dashes",
		id:   ID{Mode: ModeStatic, ClusterID: "us-east-1.prod", Volume: "vol_1.2", Subdir: "sub.dir-1"},
		want: "gfs:v1:static:us-east-1.prod:vol_1.2:sub.dir-1",
	},
}

func TestEncode(t *testing.T) {
	for _, tc := range encodeCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.id)
			if err != nil {
				t.Fatalf("Encode(%+v) = %v, want nil error", tc.id, err)
			}
			if got != tc.want {
				t.Fatalf("Encode(%+v) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func TestEncodeInvalid(t *testing.T) {
	// "gfs:v1:static:" is 14 bytes; the remaining 115 are split between the
	// cluster id (1) and the volume name (114) so the encoded form is 129.
	overlongVolume := strings.Repeat("v", 129-len("gfs:v1:static:c:"))

	tests := []struct {
		name string
		id   ID
	}{
		{
			name: "empty mode",
			id:   ID{Mode: "", ClusterID: "c", Volume: "v"},
		},
		{
			name: "unknown mode",
			id:   ID{Mode: "snapshot", ClusterID: "c", Volume: "v"},
		},
		{
			name: "mode with wrong case",
			id:   ID{Mode: "Static", ClusterID: "c", Volume: "v"},
		},
		{
			name: "subdir mode without subdir",
			id:   ID{Mode: ModeSubdir, ClusterID: "c", Volume: "v"},
		},
		{
			name: "volume mode with subdir",
			id:   ID{Mode: ModeVolume, ClusterID: "c", Volume: "v", Subdir: "s"},
		},
		{
			name: "empty cluster id",
			id:   ID{Mode: ModeStatic, ClusterID: "", Volume: "v"},
		},
		{
			name: "empty volume",
			id:   ID{Mode: ModeStatic, ClusterID: "c", Volume: ""},
		},
		{
			name: "invalid cluster id",
			id:   ID{Mode: ModeStatic, ClusterID: "a;b", Volume: "v"},
		},
		{
			name: "invalid volume",
			id:   ID{Mode: ModeStatic, ClusterID: "c", Volume: "../v"},
		},
		{
			name: "invalid subdir",
			id:   ID{Mode: ModeStatic, ClusterID: "c", Volume: "v", Subdir: "--help"},
		},
		{
			name: "subdir with colon",
			id:   ID{Mode: ModeStatic, ClusterID: "c", Volume: "v", Subdir: "a:b"},
		},
		{
			name: "encoded length 129",
			id:   ID{Mode: ModeStatic, ClusterID: "c", Volume: overlongVolume},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.id)
			if err == nil {
				t.Fatalf("Encode(%+v) = %q, want error", tc.id, got)
			}
			if got != "" {
				t.Fatalf("Encode(%+v) returned %q together with an error", tc.id, got)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want errors.Is(err, ErrInvalid)", err)
			}
		})
	}
}

func TestEncodeMaxLengthBoundary(t *testing.T) {
	// len("gfs:v1:static:c:") == 16, so a 112 byte volume name gives exactly 128.
	fits := strings.Repeat("v", MaxLength-len("gfs:v1:static:c:"))
	id := ID{Mode: ModeStatic, ClusterID: "c", Volume: fits}

	if err := id.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for an exactly 128 byte id", err)
	}
	got, err := Encode(id)
	if err != nil {
		t.Fatalf("Encode() = %v, want nil error", err)
	}
	if len(got) != MaxLength {
		t.Fatalf("len(Encode()) = %d, want %d", len(got), MaxLength)
	}

	id.Volume = fits + "v"
	err = id.Validate()
	if err == nil {
		t.Fatal("Validate() = nil for a 129 byte id, want error")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want errors.Is(err, ErrInvalid)", err)
	}
	if !strings.Contains(err.Error(), "129") {
		t.Fatalf("error message %q must report the actual length 129", err.Error())
	}
}

func TestDecode(t *testing.T) {
	for _, tc := range encodeCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode(tc.want)
			if err != nil {
				t.Fatalf("Decode(%q) = %v, want nil error", tc.want, err)
			}
			if got != tc.id {
				t.Fatalf("Decode(%q) = %+v, want %+v", tc.want, got, tc.id)
			}
		})
	}
}

func TestDecodeInvalid(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantParts []string
	}{
		{name: "empty", input: ""},
		{name: "four segments", input: "gfs:v1:static:c"},
		{name: "single segment", input: "gfs"},
		{name: "seven segments", input: "gfs:v1:static:c:v:extra:more"},
		{name: "uppercase prefix", input: "GFS:v1:static:c:v"},
		{name: "wrong prefix", input: "xfs:v1:static:c:v"},
		{name: "unknown version", input: "gfs:v2:static:c:v", wantParts: []string{`unsupported version "v2"`}},
		{name: "empty version", input: "gfs::static:c:v", wantParts: []string{"unsupported version"}},
		{name: "trailing colon", input: "gfs:v1:static:c:v:"},
		{name: "trailing colon with subdir", input: "gfs:v1:subdir:c:v:"},
		{name: "empty mode", input: "gfs:v1::c:v"},
		{name: "uppercase mode", input: "gfs:v1:Static:c:v"},
		{name: "unknown mode", input: "gfs:v1:snapshot:c:v"},
		{name: "subdir mode without subdir", input: "gfs:v1:subdir:c:v"},
		{name: "volume mode with subdir", input: "gfs:v1:volume:c:v:s"},
		{name: "empty cluster id", input: "gfs:v1:static::v"},
		{name: "empty volume", input: "gfs:v1:static:c:"},
		{name: "command injection in volume", input: "gfs:v1:static:c:v;id"},
		{name: "command substitution in cluster id", input: "gfs:v1:static:$(id):v"},
		{name: "space in volume", input: "gfs:v1:static:c:a b"},
		{name: "newline in volume", input: "gfs:v1:static:c:a\nb"},
		{name: "nul byte in subdir", input: "gfs:v1:static:c:v:a\x00b"},
		{name: "dash prefixed subdir", input: "gfs:v1:static:c:v:--help"},
		{name: "dot dot subdir", input: "gfs:v1:static:c:v:.."},
		{name: "slash in subdir", input: "gfs:v1:static:c:v:a/b"},
		{name: "over max length", input: "gfs:v1:volume:c:" + strings.Repeat("v", MaxLength)},
		{name: "csi-sanity volume id", input: "some-vol-id"},
		{name: "reallyfakesuffix", input: "reallyfakevolumeid"},
		{name: "spaces only", input: "   "},
		{name: "colon only", input: ":"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode(tc.input)
			if err == nil {
				t.Fatalf("Decode(%q) = %+v, want error", tc.input, got)
			}
			if got != (ID{}) {
				t.Fatalf("Decode(%q) returned %+v together with an error", tc.input, got)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Decode(%q) error = %v, want errors.Is(err, ErrInvalid)", tc.input, err)
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("Decode(%q) error = %q, want it to contain %q", tc.input, err.Error(), part)
				}
			}
		})
	}
}

func TestDecodeMaxLengthBoundary(t *testing.T) {
	fits := "gfs:v1:static:c:" + strings.Repeat("v", MaxLength-len("gfs:v1:static:c:"))
	if len(fits) != MaxLength {
		t.Fatalf("test setup: len = %d, want %d", len(fits), MaxLength)
	}
	if _, err := Decode(fits); err != nil {
		t.Fatalf("Decode(exactly %d bytes) = %v, want nil error", MaxLength, err)
	}

	tooLong := fits + "v"
	_, err := Decode(tooLong)
	if err == nil {
		t.Fatalf("Decode(%d bytes) = nil error, want error", len(tooLong))
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want errors.Is(err, ErrInvalid)", err)
	}
	if !strings.Contains(err.Error(), "129") {
		t.Fatalf("error message %q must report the actual length 129", err.Error())
	}
	if strings.Contains(err.Error(), strings.Repeat("v", 64)) {
		t.Fatalf("error message %q must not echo the oversized input", err.Error())
	}
}

func TestRoundTrip(t *testing.T) {
	for _, tc := range encodeCases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := Encode(tc.id)
			if err != nil {
				t.Fatalf("Encode(%+v) = %v, want nil error", tc.id, err)
			}
			decoded, err := Decode(encoded)
			if err != nil {
				t.Fatalf("Decode(%q) = %v, want nil error", encoded, err)
			}
			if decoded != tc.id {
				t.Fatalf("Decode(Encode(%+v)) = %+v, want the original id", tc.id, decoded)
			}
			reencoded, err := Encode(decoded)
			if err != nil {
				t.Fatalf("Encode(%+v) = %v, want nil error", decoded, err)
			}
			if reencoded != encoded {
				t.Fatalf("Encode(Decode(%q)) = %q, want the same string", encoded, reencoded)
			}
		})
	}
}

func TestIDValidate(t *testing.T) {
	tests := []struct {
		name    string
		id      ID
		wantErr bool
	}{
		{
			name: "static without subdir",
			id:   ID{Mode: ModeStatic, ClusterID: "c", Volume: "v"},
		},
		{
			name: "static with subdir",
			id:   ID{Mode: ModeStatic, ClusterID: "c", Volume: "v", Subdir: "s"},
		},
		{
			name: "subdir with subdir",
			id:   ID{Mode: ModeSubdir, ClusterID: "c", Volume: "v", Subdir: "s"},
		},
		{
			name: "volume without subdir",
			id:   ID{Mode: ModeVolume, ClusterID: "c", Volume: "v"},
		},
		{
			name:    "zero value",
			id:      ID{},
			wantErr: true,
		},
		{
			name:    "subdir mode without subdir",
			id:      ID{Mode: ModeSubdir, ClusterID: "c", Volume: "v"},
			wantErr: true,
		},
		{
			name:    "volume mode with subdir",
			id:      ID{Mode: ModeVolume, ClusterID: "c", Volume: "v", Subdir: "s"},
			wantErr: true,
		},
		{
			name:    "invalid cluster id",
			id:      ID{Mode: ModeStatic, ClusterID: "..", Volume: "v"},
			wantErr: true,
		},
		{
			name:    "invalid volume",
			id:      ID{Mode: ModeStatic, ClusterID: "c", Volume: "a b"},
			wantErr: true,
		},
		{
			name:    "invalid subdir",
			id:      ID{Mode: ModeStatic, ClusterID: "c", Volume: "v", Subdir: "-s"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.id.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate(%+v) = nil, want error", tc.id)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("Validate(%+v) error = %v, want errors.Is(err, ErrInvalid)", tc.id, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%+v) = %v, want nil", tc.id, err)
			}
		})
	}
}

func FuzzDecode(f *testing.F) {
	for _, tc := range encodeCases {
		f.Add(tc.want)
	}
	seeds := []string{
		"",
		"gfs",
		"gfs:v1:static:c",
		"gfs:v1:static:c:v",
		"gfs:v1:static:c:v:s",
		"gfs:v1:static:c:v:",
		"gfs:v1:static:c:v:s:x",
		"gfs:v1:subdir:c:v",
		"gfs:v1:subdir:c:v:s",
		"gfs:v1:volume:c:v",
		"gfs:v1:volume:c:v:s",
		"gfs:v1:snapshot:c:v",
		"gfs:v1:Static:c:v",
		"gfs:v2:static:c:v",
		"GFS:v1:static:c:v",
		"xfs:v1:static:c:v",
		"gfs:v1:static::v",
		"gfs:v1:static:c:",
		"gfs:v1:static:c:a b",
		"gfs:v1:static:c:a;b",
		"gfs:v1:static:c:$(id)",
		"gfs:v1:static:c:v:..",
		"gfs:v1:static:c:v:--help",
		"gfs:v1:static:c:" + largeName,
		"gfs:v1:static:c:" + largeName + "a",
		"some-vol-id",
		"reallyfakevolumeid",
		":::::",
		"\x00",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		id, err := Decode(s)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Decode(%q) error = %v, want errors.Is(err, ErrInvalid)", s, err)
			}
			return
		}
		// Decode only succeeds for strings within the length limit.
		if len(s) > MaxLength {
			t.Fatalf("Decode(%q) succeeded for an input longer than %d bytes", s, MaxLength)
		}
		encoded, err := Encode(id)
		if err != nil {
			t.Fatalf("Encode(Decode(%q)) = %v, want nil error", s, err)
		}
		if encoded != s {
			t.Fatalf("Encode(Decode(%q)) = %q, want the original input", s, encoded)
		}
	})
}
