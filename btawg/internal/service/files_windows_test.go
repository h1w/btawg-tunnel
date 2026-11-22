package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsManagedConfigReplacementKeepsPrivateACL(t *testing.T) {
	if !Privileged() {
		t.Skip("requires an elevated Windows test process")
	}
	dir := filepath.Join(t.TempDir(), "managed")
	path := filepath.Join(dir, "client.json")
	for _, data := range []string{"first secret", "replacement secret"} {
		if err := WriteManagedFile(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "replacement secret" {
		t.Fatalf("config after replacement = %q, %v", got, err)
	}
	for _, p := range []string{dir, path} {
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		sddl := sd.String()
		if !strings.Contains(sddl, ";;;BA)") || !strings.Contains(sddl, ";;;SY)") || strings.Contains(sddl, ";;;BU)") || strings.Contains(sddl, ";;;WD)") || strings.Contains(sddl, ";;;AU)") {
			t.Fatalf("unexpected managed ACL on %s: %s", p, sddl)
		}
	}
}
