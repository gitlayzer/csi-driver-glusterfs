package version

import "testing"

func TestGet(t *testing.T) {
	origVersion, origCommit, origDate := Version, GitCommit, BuildDate
	t.Cleanup(func() {
		Version, GitCommit, BuildDate = origVersion, origCommit, origDate
	})

	tests := []struct {
		name    string
		version string
		commit  string
		date    string
		want    Info
	}{
		{
			name:    "defaults without ldflags",
			version: "unknown",
			commit:  "unknown",
			date:    "unknown",
			want:    Info{Version: "unknown", GitCommit: "unknown", BuildDate: "unknown"},
		},
		{
			name:    "injected values",
			version: "v0.1.0",
			commit:  "abc1234",
			date:    "2026-10-31T00:00:00Z",
			want:    Info{Version: "v0.1.0", GitCommit: "abc1234", BuildDate: "2026-10-31T00:00:00Z"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			Version, GitCommit, BuildDate = tc.version, tc.commit, tc.date

			if got := Get(); got != tc.want {
				t.Fatalf("Get() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInfoString(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "unknown build",
			info: Info{Version: "unknown", GitCommit: "unknown", BuildDate: "unknown"},
			want: "glusterfs-csi unknown (commit: unknown, build date: unknown)",
		},
		{
			name: "injected build",
			info: Info{Version: "v0.1.0", GitCommit: "abc1234", BuildDate: "2026-10-31T00:00:00Z"},
			want: "glusterfs-csi v0.1.0 (commit: abc1234, build date: 2026-10-31T00:00:00Z)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.String(); got != tc.want {
				t.Fatalf("Info.String() = %q, want %q", got, tc.want)
			}
		})
	}
}
