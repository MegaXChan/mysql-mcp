package main

import (
	"runtime/debug"
	"testing"
)

func TestResolveCommitFromBuildInfo(t *testing.T) {
	t.Parallel()

	buildInfo := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "build-info-sha"},
	}}
	tests := []struct {
		name     string
		injected string
		info     *debug.BuildInfo
		infoOK   bool
		want     string
	}{
		{
			name:     "injected commit wins",
			injected: "ldflags-sha",
			info:     buildInfo,
			infoOK:   true,
			want:     "ldflags-sha",
		},
		{
			name:     "default uses embedded VCS revision",
			injected: "unknown",
			info:     buildInfo,
			infoOK:   true,
			want:     "build-info-sha",
		},
		{
			name:     "empty injection uses embedded VCS revision",
			injected: "",
			info:     buildInfo,
			infoOK:   true,
			want:     "build-info-sha",
		},
		{
			name:     "missing build info remains unknown",
			injected: "unknown",
			infoOK:   false,
			want:     "unknown",
		},
		{
			name:     "missing VCS revision remains unknown",
			injected: "unknown",
			info: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs", Value: "git"},
			}},
			infoOK: true,
			want:   "unknown",
		},
		{
			name:     "blank VCS revision remains unknown",
			injected: "unknown",
			info: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "  "},
			}},
			infoOK: true,
			want:   "unknown",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveCommitFromBuildInfo(test.injected, test.info, test.infoOK); got != test.want {
				t.Fatalf("resolveCommitFromBuildInfo() = %q, want %q", got, test.want)
			}
		})
	}
}
