// Command measure runs isolated carrier measurements across btawg modes and
// experimental settings. It uses the real btawg server and client with
// registered keys, looping back through the local overlay network.
//
// Run with root privileges to enable tc delay and tcpdump capture.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"bittorrent-tunnel/btawg/experiments/carrier-metrics/internal/harness"
	"bittorrent-tunnel/btawg/internal/config"
)

func main() {
	var (
		modes                = flag.String("modes", "ext-tcp,ext-utp,req-tcp,req-utp", "comma-separated carrier modes")
		experiments          = flag.String("experiments", "baseline", "comma-separated experiments: baseline,req-window,req-varied,ext-protected,ext-packed,ext-padded")
		delays               = flag.String("delays", "0,40,200", "comma-separated one-way delays in ms")
		repeats              = flag.Int("repeats", 3, "repetitions per configuration")
		captureDir           = flag.String("capture", "", "directory for tcpdump pcaps")
		captureIface         = flag.String("capture-iface", "lo", "interface for tcpdump")
		toolsDir             = flag.String("tools", "/tmp/btawg-dpi-harness.tjayWT8n/tools/usr/bin", "directory with tcpdump")
		out                  = flag.String("out", "", "JSONL output file (default stdout)")
		smallCount           = flag.Int("small", 100, "small round trips")
		smallSize            = flag.Int("small-size", 64, "small payload bytes")
		bulkBytes            = flag.Int("bulk", 1<<20, "bulk echo bytes")
		concurrentSmallCount = flag.Int("concurrent-small", 100, "small round trips during bulk (0 disables)")
		concurrentSmallSize  = flag.Int("concurrent-small-size", 64, "bytes per concurrent small round trip")
		paceMbps             = flag.Int("pace-mbps", 0, "BTAWG_CARRIER_PACE_MBPS in Mbps (0=disabled)")
		idleKAs              = flag.Int("idle-keepalives", 0, "BTAWG_CARRIER_IDLE_KEEPALIVES, max 12 (0=disabled)")
	)
	flag.Parse()

	modeList := split(*modes)
	expList := split(*experiments)
	delayList := parseInts(*delays)

	enc := json.NewEncoder(os.Stdout)
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", *out, err)
			os.Exit(1)
		}
		defer f.Close()
		enc = json.NewEncoder(f)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, mode := range modeList {
		if !validMode(mode) {
			fmt.Fprintf(os.Stderr, "skipping unknown mode %q\n", mode)
			continue
		}
		for _, exp := range expList {
			for _, delay := range delayList {
				for r := range *repeats {
					if ctx.Err() != nil {
						return
					}
					res := run(ctx, mode, exp, delay, r, *captureDir, *captureIface, *toolsDir, *smallCount, *smallSize, *bulkBytes,
						*concurrentSmallCount, *concurrentSmallSize, *paceMbps, *idleKAs)
					if err := enc.Encode(res); err != nil {
						fmt.Fprintf(os.Stderr, "encode: %v\n", err)
					}
					fmt.Fprintf(os.Stderr, "%s %s delay=%d repeat=%d small=%d/%d bulk=%.3f Mbps first=%.3f ms conc_p95=%.3f ms\n",
						res.Mode, res.Experiment, res.DelayMS, res.Repeat,
						res.SmallReceived, res.SmallSent, res.BulkThroughput, res.FirstEchoMS, res.ConcurrentSmallP95MS)
				}
			}
		}
	}
}

func run(ctx context.Context, mode, exp string, delayMS, repeat int, captureDir, captureIface, toolsDir string,
	smallCount, smallSize, bulkBytes, concurrentSmallCount, concurrentSmallSize, paceMbps, idleKeepalives int) *harness.Result {
	mode = config.CanonicalMode(mode)
	harness.SetExperimentEnv(exp, paceMbps, idleKeepalives)
	netem := harness.NewLoopbackNetem(delayMS)
	defer netem.Cleanup()

	var cap *harness.Capture
	if captureDir != "" {
		cap, _ = harness.StartCapture(captureIface, captureDir, toolsDir)
	}
	defer func() {
		if cap != nil {
			cap.Stop()
		}
	}()

	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	hub, err := harness.SetupHub(runCtx, mode)
	if err != nil {
		return &harness.Result{Mode: mode, Experiment: exp, DelayMS: delayMS, Repeat: repeat, Error: err.Error()}
	}
	defer hub.Close()

	if err := hub.StartClient(runCtx, &hub.A); err != nil {
		return &harness.Result{Mode: mode, Experiment: exp, DelayMS: delayMS, Repeat: repeat, Error: err.Error()}
	}
	if err := hub.StartClient(runCtx, &hub.B); err != nil {
		return &harness.Result{Mode: mode, Experiment: exp, DelayMS: delayMS, Repeat: repeat, Error: err.Error()}
	}

	cfg := harness.WorkloadConfig{
		Mode:                   mode,
		Experiment:             exp,
		DelayMS:                delayMS,
		Repeat:                 repeat,
		SmallCount:             smallCount,
		SmallPayload:           smallSize,
		BulkBytes:              bulkBytes,
		ConcurrentSmallCount:   concurrentSmallCount,
		ConcurrentSmallPayload: concurrentSmallSize,
		SmallDeadline:          30 * time.Second,
		BulkDeadline:           60 * time.Second,
		ConnectTimeout:         20 * time.Second,
	}
	res, err := harness.RunOverlayWorkload(runCtx, hub, cfg)
	if err != nil {
		if res == nil {
			res = &harness.Result{Mode: mode, Experiment: exp, DelayMS: delayMS, Repeat: repeat, Error: err.Error()}
		} else {
			res.Error = err.Error()
		}
	}
	if cap != nil {
		res.Pcap = cap.Stop()
	}
	return res
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseInts(s string) []int {
	parts := split(s)
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		var v int
		if _, err := fmt.Sscanf(p, "%d", &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

func validMode(m string) bool {
	cm := config.CanonicalMode(m)
	return config.IsCarrierMode(cm)
}
