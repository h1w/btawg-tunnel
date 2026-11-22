package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Version is set to the release tag by the GitHub build.
var Version = "dev"

const (
	latestRelease = "https://api.github.com/repos/h1w/btawg-tunnel/releases/latest"
	assetBase     = "https://github.com/h1w/btawg-tunnel/releases/download/"
	maxAssetSize  = 64 << 20
)

type updateResult struct {
	installed, latest string
	updated           bool
}

func (u Unit) releaseAsset(goos, goarch string) string {
	if goarch != "amd64" && goarch != "arm64" {
		return ""
	}
	switch goos {
	case "linux":
		if u == Client || u == Server {
			return u.binaryName() + "-linux-" + goarch
		}
	case "windows":
		if u == Client {
			return "btawg-client-windows-" + goarch + ".zip"
		}
	}
	return ""
}

func (u Unit) updateScreen(ctx context.Context, progress func(string)) (*Screen, error) {
	if u.releaseAsset(runtime.GOOS, runtime.GOARCH) == "" {
		return nil, fmt.Errorf("no GitHub release binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client := directUpdateClient()
	defer client.CloseIdleConnections()
	result, err := u.updateFrom(ctx, client, progress)
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("Installed: %s\nLatest GitHub release: %s\n\nNo update needed.", result.installed, result.latest)
	if result.updated {
		if runtime.GOOS == "windows" {
			text = fmt.Sprintf("Update %s → %s handed to the Windows activation helper. The result is pending; reopen the TUI to see it.", result.installed, result.latest)
		} else {
			text = fmt.Sprintf("Updated %s → %s.\n\nReopen the TUI to use the new version.", result.installed, result.latest)
		}
	}
	if notice := platformUpdateNotice(); notice != "" {
		text += "\n\n" + notice
	}
	return &Screen{Title: "GitHub update", Text: text}, nil
}

func directUpdateClient() *http.Client {
	return &http.Client{
		// No proxy lookup, including HTTP_PROXY and HTTPS_PROXY. btawg
		// does not install an Internet default route on either host.
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) < 10 && req.URL.Scheme == "https" && req.URL.User == nil {
				switch req.URL.Host {
				case "github.com", "api.github.com", "release-assets.githubusercontent.com":
					return nil
				}
			}
			return fmt.Errorf("unexpected GitHub release redirect")
		},
	}
}

func (u Unit) updateFrom(ctx context.Context, client *http.Client, progress func(string)) (updateResult, error) {
	var result updateResult
	if err := prepareUpdateLock(u.updateLockPath()); err != nil {
		return result, err
	}
	lock, err := os.OpenFile(u.updateLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return result, fmt.Errorf("open update lock: %w", err)
	}
	defer lock.Close()
	if err := lockUpdate(lock); err != nil {
		return result, fmt.Errorf("lock update: %w", err)
	}
	progress("Checking latest release…")
	result, err = u.checkRelease(ctx, client)
	if err != nil || !result.updated {
		return result, err
	}
	return result, u.applyRelease(ctx, client, result.latest, progress)
}

func (u Unit) availableRelease(ctx context.Context, client *http.Client) string {
	if result, err := u.checkRelease(ctx, client); err == nil && result.updated {
		return result.latest
	}
	return ""
}

func (u Unit) checkRelease(ctx context.Context, client *http.Client) (updateResult, error) {
	var result updateResult

	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	out, err := exec.CommandContext(versionCtx, u.binaryPath(), "--version").Output()
	cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.installed = strings.TrimSpace(string(out))
	current, valid := stableVersion(result.installed)
	if err != nil || !valid {
		return result, fmt.Errorf("installed binary has no release version; install a current GitHub release manually first")
	}

	metadata, err := getReleaseFile(ctx, client, latestRelease)
	if err != nil {
		return result, fmt.Errorf("check latest release: %w", err)
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	defer metadata.Body.Close()
	err = json.NewDecoder(io.LimitReader(metadata.Body, 1<<20)).Decode(&release)
	if err != nil {
		return result, fmt.Errorf("read release metadata: %w", err)
	}
	result.latest = release.Tag
	latest, valid := stableVersion(result.latest)
	if !valid || release.Draft || release.Prerelease {
		return result, fmt.Errorf("latest GitHub release has no stable version tag")
	}
	for i := range latest {
		if latest[i] != current[i] {
			result.updated = latest[i] > current[i]
			break
		}
	}
	return result, nil
}

func stableVersion(tag string) ([3]uint64, bool) {
	var version [3]uint64
	if !strings.HasPrefix(tag, "v") {
		return version, false
	}
	parts := strings.Split(tag[1:], ".")
	if len(parts) != len(version) {
		return version, false
	}
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil || len(part) > 1 && part[0] == '0' {
			return version, false
		}
		version[i] = n
	}
	return version, true
}

func getReleaseFile(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("GitHub returned %s", res.Status)
	}
	return res, nil
}

func releaseChecksum(ctx context.Context, client *http.Client, base, name string) ([]byte, error) {
	res, err := getReleaseFile(ctx, client, base+"SHA256SUMS")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	scanner := bufio.NewScanner(io.LimitReader(res.Body, 64<<10))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 {
			file := strings.TrimPrefix(fields[1], "*")
			if file == name || file == "dist/"+name {
				digest, err := hex.DecodeString(fields[0])
				if err != nil || len(digest) != sha256.Size {
					return nil, fmt.Errorf("invalid SHA-256 for %s", name)
				}
				return digest, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no SHA-256 for %s", name)
}

func downloadBinary(ctx context.Context, client *http.Client, url, path string, want []byte, progress func(string)) error {
	progress("Connecting to download…")
	res, err := getReleaseFile(ctx, client, url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	meter := downloadMeter{progress: progress, total: res.ContentLength, last: -1}
	meter.Write(nil)
	n, err := io.Copy(io.MultiWriter(f, hash, &meter), io.LimitReader(res.Body, maxAssetSize+1))
	if err != nil {
		return err
	}
	progress("Verifying SHA-256…")
	if n == 0 || n > maxAssetSize || !bytes.Equal(hash.Sum(nil), want) {
		return fmt.Errorf("invalid binary size or SHA-256 mismatch")
	}
	return f.Sync()
}

type downloadMeter struct {
	progress             func(string)
	total, written, last int64
}

func (m *downloadMeter) Write(b []byte) (int, error) {
	m.written += int64(len(b))
	if m.total > 0 {
		percent := min(100, m.written*100/m.total)
		if percent != m.last {
			m.last = percent
			m.progress(fmt.Sprintf("Downloading: %d%%", percent))
		}
	} else if step := m.written / (64 << 10); step != m.last {
		m.last = step
		m.progress(fmt.Sprintf("Downloading: %d KiB", m.written>>10))
	}
	return len(b), nil
}
