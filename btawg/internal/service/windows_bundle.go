package service

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

var windowsBundleFiles = [...]string{"btawg-client.exe", "wintun.dll", "Wintun-LICENSE.txt"}

func unpackWindowsBundle(archive, dest string) error {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer z.Close()
	var files [len(windowsBundleFiles)]*zip.File
	var total uint64
	for _, f := range z.File {
		valid := false
		for i, name := range windowsBundleFiles {
			if f.Name == name && files[i] == nil {
				files[i] = f
				valid = true
				break
			}
		}
		if !valid || !f.Mode().IsRegular() || f.UncompressedSize64 == 0 || f.UncompressedSize64 > maxAssetSize-total {
			return fmt.Errorf("invalid Windows bundle entry %q", f.Name)
		}
		total += f.UncompressedSize64
	}
	for i, f := range files {
		if f == nil {
			return fmt.Errorf("Windows bundle is missing %s", windowsBundleFiles[i])
		}
	}
	for _, f := range files {
		r, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(filepath.Join(dest, f.Name), io.LimitReader(r, int64(f.UncompressedSize64)+1), 0o700)
		_ = r.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func verifyWindowsDriver(path string) error {
	// Digests from the verified Wintun 0.14.1 archive, selected by binary architecture.
	var want string
	switch runtime.GOARCH {
	case "amd64":
		want = "e5da8447dc2c320edc0fc52fa01885c103de8c118481f683643cacc3220dafce"
	case "arm64":
		want = "f7ba89005544be9d85231a9e0d5f23b2d15b3311667e2dad0debd344918a3f80"
	default:
		return fmt.Errorf("no pinned Wintun driver for %s", runtime.GOARCH)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxAssetSize+1))
	if err != nil {
		return err
	}
	if n > maxAssetSize || fmt.Sprintf("%x", h.Sum(nil)) != want {
		return fmt.Errorf("wintun.dll is not the pinned %s release driver", runtime.GOARCH)
	}
	return nil
}
