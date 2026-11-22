package service

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsBundleRejectsMixedOrEscapingEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		ok    bool
	}{
		{"complete", []string{"btawg-client.exe", "wintun.dll", "Wintun-LICENSE.txt"}, true},
		{"missing DLL", []string{"btawg-client.exe", "Wintun-LICENSE.txt"}, false},
		{"duplicate DLL", []string{"btawg-client.exe", "wintun.dll", "wintun.dll", "Wintun-LICENSE.txt"}, false},
		{"escaping path", []string{"btawg-client.exe", "wintun.dll", "Wintun-LICENSE.txt", "../outside"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "bundle.zip")
			out, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			z := zip.NewWriter(out)
			for _, name := range tc.files {
				w, err := z.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write([]byte(name + " payload")); err != nil {
					t.Fatal(err)
				}
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
			staged := filepath.Join(dir, "staged")
			if err := os.Mkdir(staged, 0o700); err != nil {
				t.Fatal(err)
			}
			err = unpackWindowsBundle(archive, staged)
			if (err == nil) != tc.ok {
				t.Fatalf("unpack = %v, success want %t", err, tc.ok)
			}
			if !tc.ok {
				if entries, err := os.ReadDir(staged); err != nil || len(entries) != 0 {
					t.Fatalf("rejected bundle wrote staged content: %v, %v", entries, err)
				}
				return
			}
			for _, name := range tc.files {
				data, err := os.ReadFile(filepath.Join(staged, name))
				if err != nil || string(data) != name+" payload" {
					t.Fatalf("staged %s = %q, %v", name, data, err)
				}
			}
		})
	}
}
