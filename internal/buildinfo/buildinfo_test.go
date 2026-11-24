package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	vcs := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.3.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123"},
			{Key: "vcs.time", Value: "2026-11-20T10:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	develMain := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}

	tests := []struct {
		name                    string
		version, commit, date   string
		bi                      *debug.BuildInfo
		ok                      bool
		wantVersion, wantCommit string
		wantDate                string
		wantModified            bool
	}{
		{
			name:        "stamped values win over toolchain info",
			version:     "v1.0.0",
			commit:      "feedfacecafe",
			date:        "2026-11-24T00:00:00Z",
			bi:          vcs,
			ok:          true,
			wantVersion: "v1.0.0", wantCommit: "feedfacecafe", wantDate: "2026-11-24T00:00:00Z",
			wantModified: true,
		},
		{
			name:        "falls back to toolchain module and vcs info",
			bi:          vcs,
			ok:          true,
			wantVersion: "v0.3.0", wantCommit: "0123456789ab", wantDate: "2026-11-20T10:00:00Z",
			wantModified: true,
		},
		{
			name:        "devel main module reports devel",
			bi:          develMain,
			ok:          true,
			wantVersion: develVersion,
		},
		{
			name:        "no build info at all",
			ok:          false,
			wantVersion: develVersion,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolve(tc.version, tc.commit, tc.date, tc.bi, tc.ok)
			if got.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVersion)
			}
			if got.Commit != tc.wantCommit {
				t.Errorf("Commit = %q, want %q", got.Commit, tc.wantCommit)
			}
			if got.Date != tc.wantDate {
				t.Errorf("Date = %q, want %q", got.Date, tc.wantDate)
			}
			if got.Modified != tc.wantModified {
				t.Errorf("Modified = %v, want %v", got.Modified, tc.wantModified)
			}
			if got.GoVersion != runtime.Version() {
				t.Errorf("GoVersion = %q, want %q", got.GoVersion, runtime.Version())
			}
			if got.Platform != runtime.GOOS+"/"+runtime.GOARCH {
				t.Errorf("Platform = %q", got.Platform)
			}
		})
	}
}

func TestInfoString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "full",
			info: Info{Version: "v1.0.0", Commit: "abc", Date: "d", GoVersion: "go1.27.4", Platform: "linux/amd64"},
			want: "openchef v1.0.0 (abc, d) go1.27.4 linux/amd64",
		},
		{
			name: "dirty and unknowns",
			info: Info{Version: "devel", Modified: true, GoVersion: "go1.27.4", Platform: "darwin/arm64"},
			want: "openchef devel (unknown-dirty, unknown) go1.27.4 darwin/arm64",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.info.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGetNeverEmpty(t *testing.T) {
	t.Parallel()
	info := Get()
	if strings.TrimSpace(info.Version) == "" {
		t.Fatal("Get().Version must never be empty")
	}
}
