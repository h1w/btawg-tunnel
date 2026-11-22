package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateFromRelease(t *testing.T) {
	const old = "#!/bin/sh\nprintf 'v0.5.9\\n'\n"
	const next = "#!/bin/sh\nprintf 'v0.5.10\\n'\n"
	for _, tc := range []struct {
		name, tag                string
		unit                     Unit
		installed, asset         string
		active, rejectRestart    bool
		cancelAfterDownload      bool
		wantFailed, wantDownload bool
		wantInstalled            string
	}{
		{name: "client updates an active service", tag: "v0.5.10", unit: Client, installed: old, asset: next, active: true, wantDownload: true, wantInstalled: next},
		{name: "server updates without starting a stopped service", tag: "v0.5.10", unit: Server, installed: old, asset: next, wantDownload: true, wantInstalled: next},
		{name: "equal version", tag: "v0.5.9", unit: Client, installed: old, wantInstalled: old},
		{name: "never downgrade", tag: "v0.5.8", unit: Client, installed: old, wantInstalled: old},
		{name: "never install a prerelease", tag: "v0.5.10-rc.1", unit: Client, installed: old, wantFailed: true, wantInstalled: old},
		{name: "reject corrupt download", tag: "v0.5.10", unit: Server, installed: old, asset: "corrupt", wantFailed: true, wantDownload: true, wantInstalled: old},
		{name: "cancel before replacing the executable", tag: "v0.5.10", unit: Client, installed: old, asset: next, active: true, cancelAfterDownload: true, wantFailed: true, wantDownload: true, wantInstalled: old},
		{name: "restore after restart failure", tag: "v0.5.10", unit: Client, installed: old, asset: next, active: true, rejectRestart: true, wantFailed: true, wantDownload: true, wantInstalled: old},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldBinDir, oldRunner := binDir, systemctlRunner
			binDir = t.TempDir()
			t.Cleanup(func() { binDir, systemctlRunner = oldBinDir, oldRunner })
			path := filepath.Join(binDir, "btawg-"+string(tc.unit))
			if err := os.WriteFile(path, []byte(tc.installed), 0o755); err != nil {
				t.Fatal(err)
			}
			active, running := tc.active, tc.installed
			systemctlRunner = func(_ context.Context, args ...string) (string, error) {
				if len(args) != 2 || args[1] != "btawg-"+string(tc.unit)+".service" {
					t.Fatalf("unexpected systemctl arguments: %v", args)
				}
				switch args[0] {
				case "try-restart":
					if !active {
						return "", nil
					}
					if tc.rejectRestart {
						active, running = false, ""
						return "", errors.New("new binary failed to start")
					}
				case "restart":
				default:
					t.Fatalf("unexpected systemctl operation: %v", args)
				}
				b, err := os.ReadFile(path)
				active, running = err == nil, string(b)
				return "", err
			}
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				base := "/h1w/btawg-tunnel/releases/download/" + tc.tag + "/"
				switch r.URL.Path {
				case "/repos/h1w/btawg-tunnel/releases/latest":
					fmt.Fprintf(w, `{"tag_name":%q}`, tc.tag)
				case base + "SHA256SUMS":
					fmt.Fprintf(w, "%x  dist/btawg-%s\n", sha256.Sum256([]byte(next)), tc.unit)
				case base + "btawg-" + string(tc.unit):
					fmt.Fprint(w, tc.asset)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			hc := srv.Client()
			transport := hc.Transport
			downloaded := false
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hc.Transport = updateRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/h1w/btawg-tunnel/releases/download/"+tc.tag+"/btawg-"+string(tc.unit) {
					downloaded = true
				}
				clone := r.Clone(r.Context())
				clone.URL.Host = srv.Listener.Addr().String()
				res, err := transport.RoundTrip(clone)
				if err == nil && tc.cancelAfterDownload && downloaded {
					res.Body = cancelOnClose{res.Body, cancel}
				}
				return res, err
			})
			_, err := tc.unit.updateFrom(ctx, hc, func(string) {})
			if tc.cancelAfterDownload && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled update returned %v, want context.Canceled", err)
			}
			if (err != nil) != tc.wantFailed {
				t.Fatalf("error = %v, want failure %t", err, tc.wantFailed)
			}
			if downloaded != tc.wantDownload {
				t.Fatalf("binary downloaded = %t, want %t", downloaded, tc.wantDownload)
			}
			b, err := os.ReadFile(path)
			if err != nil || string(b) != tc.wantInstalled {
				t.Fatalf("installed = %q, %v; want %q", b, err, tc.wantInstalled)
			}
			if active != tc.active || active && running != tc.wantInstalled {
				t.Fatalf("service active = %t, running %q after update", active, running)
			}
		})
	}
}

type updateRoundTrip func(*http.Request) (*http.Response, error)

func (rt updateRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body cancelOnClose) Close() error {
	body.cancel()
	return body.ReadCloser.Close()
}

func TestAvailableRelease(t *testing.T) {
	for _, tc := range []struct {
		name, current, response, want string
		status                        int
	}{
		{"newer", "v0.5.9", `{"tag_name":"v0.5.10"}`, "v0.5.10", http.StatusOK},
		{"current", "v0.5.10", `{"tag_name":"v0.5.10"}`, "", http.StatusOK},
		{"older", "v0.5.11", `{"tag_name":"v0.5.10"}`, "", http.StatusOK},
		{"prerelease", "v0.5.9", `{"tag_name":"v0.5.10","prerelease":true}`, "", http.StatusOK},
		{"GitHub unavailable", "v0.5.9", "", "", http.StatusServiceUnavailable},
		{"unversioned", "dev", `{"tag_name":"v0.5.10"}`, "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldBinDir := binDir
			binDir = t.TempDir()
			t.Cleanup(func() { binDir = oldBinDir })
			binary := []byte("#!/bin/sh\nprintf '%s\\n' '" + tc.current + "'\n")
			if err := os.WriteFile(filepath.Join(binDir, "btawg-client"), binary, 0o755); err != nil {
				t.Fatal(err)
			}
			hc := &http.Client{Transport: updateRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.String() != "https://api.github.com/repos/h1w/btawg-tunnel/releases/latest" {
					t.Fatalf("background check sent unexpected request: %s %s", r.Method, r.URL)
				}
				w := httptest.NewRecorder()
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.response)
				return w.Result(), nil
			})}
			if got := Client.availableRelease(context.Background(), hc); got != tc.want {
				t.Fatalf("available release = %q, want %q", got, tc.want)
			}
		})
	}
}
