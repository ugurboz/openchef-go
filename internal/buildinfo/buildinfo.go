// Package buildinfo exposes version metadata stamped into the binary at build time.
//
// Release builds set the variables with -ldflags, for example:
//
//	go build -ldflags "-X github.com/ugurboz/openchef/internal/buildinfo.version=v1.0.0 \
//	  -X github.com/ugurboz/openchef/internal/buildinfo.commit=abc1234 \
//	  -X github.com/ugurboz/openchef/internal/buildinfo.date=2026-11-24T00:00:00Z"
//
// Development builds fall back to the module and VCS information recorded by the Go
// toolchain, so `go build` and `go install` binaries still report something useful.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set via -ldflags. Empty values mean "not stamped".
var (
	version = ""
	commit  = ""
	date    = ""
)

const develVersion = "devel"

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
	Modified  bool   `json:"modified"`
}

// Get returns the build information of the running binary.
func Get() Info {
	bi, ok := debug.ReadBuildInfo()
	return resolve(version, commit, date, bi, ok)
}

// resolve merges ldflags-stamped values with toolchain build info. It is separated from
// Get so the precedence rules can be tested without a stamped binary.
func resolve(stampedVersion, stampedCommit, stampedDate string, bi *debug.BuildInfo, ok bool) Info {
	info := Info{
		Version:   stampedVersion,
		Commit:    stampedCommit,
		Date:      stampedDate,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if ok && bi != nil {
		if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = s.Value
				}
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	if info.Version == "" {
		info.Version = develVersion
	}
	if len(info.Commit) > 12 {
		info.Commit = info.Commit[:12]
	}
	return info
}

// String renders a single human-readable line, e.g.
// "openchef v1.0.0 (abc1234def56, 2026-11-24T00:00:00Z) go1.27.4 darwin/arm64".
func (i Info) String() string {
	commit := i.Commit
	if commit == "" {
		commit = "unknown"
	}
	if i.Modified {
		commit += "-dirty"
	}
	date := i.Date
	if date == "" {
		date = "unknown"
	}
	return fmt.Sprintf("openchef %s (%s, %s) %s %s", i.Version, commit, date, i.GoVersion, i.Platform)
}
