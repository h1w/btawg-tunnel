package service

import "testing"

func TestReleaseAssetMatchesTarget(t *testing.T) {
	for _, tc := range []struct {
		unit           Unit
		os, arch, want string
	}{
		{Client, "linux", "amd64", "btawg-client"},
		{Server, "linux", "amd64", "btawg-server"},
		{Client, "linux", "arm64", "btawg-client-linux-arm64"},
		{Server, "linux", "arm64", "btawg-server-linux-arm64"},
		{Client, "windows", "amd64", "btawg-client-windows-amd64.zip"},
		{Client, "windows", "arm64", "btawg-client-windows-arm64.zip"},
		{Server, "windows", "arm64", ""},
		{Client, "windows", "386", ""},
	} {
		if got := tc.unit.releaseAsset(tc.os, tc.arch); got != tc.want {
			t.Errorf("%s %s/%s: got %q, want %q", tc.unit, tc.os, tc.arch, got, tc.want)
		}
	}
}
