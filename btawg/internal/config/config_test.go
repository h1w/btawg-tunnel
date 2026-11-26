package config

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

const wantProfileString = "jc=4\njmin=40\njmax=100\ns1=32\ns2=32\ns3=32\ns4=32\nh1=101\nh2=102\nh3=103\nh4=104\n"

// 64 hex chars = 20-byte infohash.
const testInfoHash = "0123456789abcdef0123456789abcdef01234567"

func testClientConfig() ClientConfig {
	return ClientConfig{
		Version:       ClientVersion,
		InfoHash:      testInfoHash,
		ServerPubKey:  "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899",
		ClientPrivKey: "11223344556677889900aabbccddeeff0011223344556677889900aabbccdd00",
		ClientPubKey:  "44556677889900aabbccddeeff0011223344556677889900aabbccddeeff00",
		ClientIP:      "10.77.0.2",
		Subnet:        "10.77.0.0/24",
		ServerIP:      "10.77.0.1",
		Transport:     "ext-utp",
		SocksPort:     1080,
		Profile:       DefaultProfile(),
		DHTBootstrap:  []string{"router.bittorrent.com:6881"},
	}
}

func TestProfileString(t *testing.T) {
	got := DefaultProfile().DeviceIPC()
	if got != wantProfileString {
		t.Fatalf("DefaultProfile().DeviceIPC() = %q, want %q", got, wantProfileString)
	}
}

func TestImportRoundTrip(t *testing.T) {
	orig := testClientConfig()
	s, err := orig.ImportString()
	if err != nil {
		t.Fatalf("ImportString: %v", err)
	}
	if !strings.HasPrefix(s, "btawg://") {
		t.Fatalf("import string %q lacks btawg:// prefix", s)
	}
	got, err := ParseImportString(s)
	if err != nil {
		t.Fatalf("ParseImportString: %v", err)
	}
	if !reflect.DeepEqual(got, orig) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, orig)
	}
}

func TestImportWithoutAWGProfileUsesLegacyDefaults(t *testing.T) {
	const minimal = `{"version":2,"transport":"ext-tcp"}`
	cfg, err := ParseImportString(ImportPrefix + base64.RawURLEncoding.EncodeToString([]byte(minimal)))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Profile.DeviceIPC(); got != wantProfileString {
		t.Fatalf("default AWG profile = %q, want %q", got, wantProfileString)
	}
}

func TestParseRejectsLegacyVersion(t *testing.T) {
	legacy := testClientConfig()
	legacy.Version = 1
	s, err := legacy.ImportString()
	if err != nil {
		t.Fatalf("ImportString: %v", err)
	}
	if _, err := ParseImportString(s); err == nil {
		t.Fatal("ParseImportString accepted version 1, want error")
	}
}

func TestImportHasNoServerEndpoint(t *testing.T) {
	// The public endpoint of the server (e.g. its real-world IP:port) must
	// never leak into the import string; clients learn it via the DHT swarm.
	s, err := testClientConfig().ImportString()
	if err != nil {
		t.Fatalf("ImportString: %v", err)
	}
	externalIP := "203.0.113.7"
	if strings.Contains(s, "endpoint") {
		t.Fatalf("import string contains \"endpoint\": %q", s)
	}
	if strings.Contains(s, externalIP) {
		t.Fatalf("import string contains external IP %q", externalIP)
	}
}

func TestParseRejectsFutureVersion(t *testing.T) {
	future := testClientConfig()
	future.Version = 999
	s, err := future.ImportString()
	if err != nil {
		t.Fatalf("ImportString: %v", err)
	}
	_, err = ParseImportString(s)
	if err == nil {
		t.Fatal("ParseImportString accepted version 999, want error")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("error %q does not mention \"version\"", err)
	}
}

func TestParseRejectsBareBase64(t *testing.T) {
	s, err := testClientConfig().ImportString()
	if err != nil {
		t.Fatalf("ImportString: %v", err)
	}
	bare := strings.TrimPrefix(s, "btawg://")
	if _, err := ParseImportString(bare); err == nil {
		t.Fatal("ParseImportString accepted input without btawg:// prefix, want error")
	}
}

func TestDefaultCarrierPortIsStandardTorrentPort(t *testing.T) {
	if got := DefaultServerConfig().CarrierPort; got != 6881 {
		t.Fatalf("default CarrierPort = %d, want canonical torrent port 6881", got)
	}
}

func TestDefaultNativePorts(t *testing.T) {
	def := DefaultServerConfig()
	if def.AWGPort != 51821 {
		t.Fatalf("default AWGPort = %d, want 51821", def.AWGPort)
	}
	if def.WGPort != 51820 {
		t.Fatalf("default WGPort = %d, want 51820", def.WGPort)
	}
}

func TestServerConfigNormalize(t *testing.T) {
	c := ServerConfig{}
	c.Normalize()
	want := DefaultServerConfig()
	if c != want {
		t.Fatalf("Normalize() = %+v, want %+v", c, want)
	}

	legacy := ServerConfig{CarrierPort: 7777}
	legacy.Normalize()
	if legacy.CarrierPort != 7777 {
		t.Fatalf("Normalize overwrote explicit CarrierPort")
	}
	if legacy.AWGPort != want.AWGPort {
		t.Fatalf("Normalize did not fill zero AWGPort, got %d", legacy.AWGPort)
	}
}

func TestCarrierHashPreservesPreferredMode(t *testing.T) {
	got, err := CarrierHash(testInfoHash, "ext-utp", "ext-utp")
	if err != nil {
		t.Fatalf("CarrierHash: %v", err)
	}
	if got != testInfoHash {
		t.Fatalf("CarrierHash preferred = %q, want base %q", got, testInfoHash)
	}
}

func TestCarrierHashSharesFramingFamily(t *testing.T) {
	extUTP, err := CarrierHash(testInfoHash, "ext-utp", "ext-utp")
	if err != nil {
		t.Fatalf("CarrierHash: %v", err)
	}
	extTCP, err := CarrierHash(testInfoHash, "ext-utp", "ext-tcp")
	if err != nil {
		t.Fatalf("CarrierHash: %v", err)
	}
	if extUTP != extTCP || extUTP != testInfoHash {
		t.Fatalf("ext modes should share base: utp=%q tcp=%q", extUTP, extTCP)
	}

	reqUTP, err := CarrierHash(testInfoHash, "ext-utp", "req-utp")
	if err != nil {
		t.Fatalf("CarrierHash: %v", err)
	}
	reqTCP, err := CarrierHash(testInfoHash, "ext-utp", "req-tcp")
	if err != nil {
		t.Fatalf("CarrierHash: %v", err)
	}
	if reqUTP != reqTCP {
		t.Fatalf("req modes should share hash: utp=%q tcp=%q", reqUTP, reqTCP)
	}
	if reqUTP == testInfoHash {
		t.Fatal("req framing should differ from ext base")
	}

	// The preferred mode's framing is the one that keeps the base.
	fromReq, err := CarrierHash(testInfoHash, "req-tcp", "req-utp")
	if err != nil {
		t.Fatalf("CarrierHash: %v", err)
	}
	if fromReq != testInfoHash {
		t.Fatalf("req preferred should keep base, got %q", fromReq)
	}
}

func TestCarrierHashRejectsBadBase(t *testing.T) {
	if _, err := CarrierHash("notahash", "ext-utp", "ext-tcp"); err == nil {
		t.Fatal("CarrierHash accepted invalid base")
	}
	if _, err := CarrierHash(testInfoHash, "ext-utp", ""); err == nil {
		t.Fatal("CarrierHash accepted empty mode")
	}
}

func TestCarrierHashIsDeterministic(t *testing.T) {
	a, err := CarrierHash(testInfoHash, "ext-utp", "req-tcp")
	if err != nil {
		t.Fatalf("CarrierHash: %v", err)
	}
	b, err := CarrierHash(testInfoHash, "ext-utp", "req-tcp")
	if err != nil {
		t.Fatalf("CarrierHash: %v", err)
	}
	if a != b {
		t.Fatalf("CarrierHash not deterministic: %q vs %q", a, b)
	}
}

func TestNamedConnectionChains(t *testing.T) {
	for _, name := range []string{
		"socks5-awg-ext-utp", "socks5-awg-ext-tcp", "socks5-awg-req-utp", "socks5-awg-req-tcp",
		"socks5-ext-utp", "socks5-ext-tcp", "socks5-req-utp", "socks5-req-tcp",
		"socks5-awg-udp", "socks5-wg-udp",
	} {
		if !ValidClientMode(name) {
			t.Errorf("connection chain %q not accepted", name)
		}
	}
	c := ServerConfig{Transport: "req-tcp"}
	c.Normalize()
	if c.Transport != "socks5-awg-req-tcp" {
		t.Fatalf("legacy preferred mode became %q", c.Transport)
	}
}

func TestTLSCarrierHashIsolation(t *testing.T) {
	const preferred = "socks5-awg-ext-utp"
	seen := map[string]string{}
	for _, family := range []string{"socks5-awg-ext", "socks5-awg-req", "socks5-ext", "socks5-req"} {
		tcp, errTCP := CarrierHash(testInfoHash, preferred, family+"-tcp")
		utp, errUTP := CarrierHash(testInfoHash, preferred, family+"-utp")
		if errTCP != nil || errUTP != nil || tcp != utp {
			t.Fatalf("%s: carrier hashes differ or failed: %s, %s (%v, %v)", family, tcp, utp, errTCP, errUTP)
		}
		if other, duplicate := seen[tcp]; duplicate {
			t.Fatalf("%s and %s share one discovery hash", family, other)
		}
		seen[tcp] = family
	}
	if seen[testInfoHash] != "socks5-awg-ext" {
		t.Fatalf("legacy deployment hash was not preserved: %v", seen)
	}
}

func TestNativeConfigRendersQuickINI(t *testing.T) {
	cfg := testClientConfig()
	cfg.AWGEndpoint = "203.0.113.7:51821"
	cfg.WGEndpoint = "203.0.113.7:51820"
	cfg.Routes = []string{"10.77.0.0/24"}

	awgConf, err := cfg.NativeConfig(ModeAWG)
	if err != nil {
		t.Fatalf("NativeConfig(awg): %v", err)
	}
	if !strings.Contains(awgConf, "[Interface]") || !strings.Contains(awgConf, "[Peer]") {
		t.Fatalf("AWG native config missing INI sections: %s", awgConf)
	}
	if !strings.Contains(awgConf, "Jc = 4") || !strings.Contains(awgConf, "H1 = 101") {
		t.Fatalf("AWG native config missing obfuscation profile: %s", awgConf)
	}
	if !strings.Contains(awgConf, "Endpoint = 203.0.113.7:51821") {
		t.Fatalf("AWG native config endpoint wrong: %s", awgConf)
	}
	if !strings.Contains(awgConf, "AllowedIPs = 10.77.0.0/24") {
		t.Fatalf("AWG native config missing AllowedIPs: %s", awgConf)
	}
	if strings.Contains(awgConf, "Table = off") {
		t.Fatal("native export must let wg-quick install its explicit non-default routes")
	}
	if strings.Contains(awgConf, "AllowedIPs = 0.0.0.0/0") {
		t.Fatal("AWG native export must not contain default AllowedIPs")
	}
	if !strings.Contains(awgConf, "Address = 10.77.0.2/24") {
		t.Fatalf("AWG native config missing address: %s", awgConf)
	}

	wgConf, err := cfg.NativeConfig(ModeWG)
	if err != nil {
		t.Fatalf("NativeConfig(wg): %v", err)
	}
	if strings.Contains(wgConf, "Jc =") {
		t.Fatal("WG native config must not contain AWG options")
	}
	if !strings.Contains(wgConf, "Endpoint = 203.0.113.7:51820") {
		t.Fatalf("WG native config endpoint wrong: %s", wgConf)
	}
	if !strings.Contains(wgConf, "PrivateKey =") || !strings.Contains(wgConf, "PublicKey =") {
		t.Fatal("native config missing keys")
	}

	// Keys are base64 in exports.
	for _, conf := range []string{awgConf, wgConf} {
		if strings.Contains(conf, cfg.ClientPrivKey) || strings.Contains(conf, cfg.ServerPubKey) {
			t.Fatal("native export contained hex keys instead of base64")
		}
	}
}

func TestNativeAWGAdvancedProfileDoesNotReachWG(t *testing.T) {
	cfg := testClientConfig()
	cfg.AWGEndpoint = "203.0.113.7:51821"
	cfg.WGEndpoint = "203.0.113.7:51820"
	cfg.Routes = []string{"10.77.0.0/24"}
	cfg.Profile.HeaderProtectionKey = strings.Repeat("01", 32)
	cfg.Profile.H1 = "1000-1099"
	cfg.Profile.RandomTrailers = true
	cfg.Profile.PersistentKeepalive = "20-30"
	awg, err := cfg.NativeConfig(ModeAWG)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"HeaderProtectionKey = AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=", "H1 = 1000-1099", "RandomTrailers = on", "PersistentKeepalive = 20-30"} {
		if !strings.Contains(awg, line+"\n") {
			t.Fatalf("native AWG config missing %s", strings.SplitN(line, " = ", 2)[0])
		}
	}
	wg, err := cfg.NativeConfig(ModeWG)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wg, "HeaderProtectionKey") || strings.Contains(wg, "RandomTrailers") || !strings.Contains(wg, "PersistentKeepalive = 25\n") {
		t.Fatal("native WireGuard config inherited AWG options")
	}
}

func TestNativeDeviceConfigRendersIPC(t *testing.T) {
	cfg := testClientConfig()
	cfg.AWGEndpoint = "203.0.113.7:51821"
	cfg.WGEndpoint = "203.0.113.7:51820"
	cfg.Routes = []string{"10.77.0.0/24"}

	ipc, err := cfg.NativeDeviceConfig(ModeAWG)
	if err != nil {
		t.Fatalf("NativeDeviceConfig(awg): %v", err)
	}
	if !strings.Contains(ipc, "private_key="+cfg.ClientPrivKey) {
		t.Fatal("AWG IPC missing hex private key")
	}
	if !strings.Contains(ipc, "public_key="+cfg.ServerPubKey) {
		t.Fatal("AWG IPC missing hex public key")
	}
	if !strings.Contains(ipc, "endpoint=203.0.113.7:51821") {
		t.Fatalf("AWG IPC endpoint wrong: %s", ipc)
	}
	if !strings.Contains(ipc, "allowed_ip=0.0.0.0/0") {
		t.Fatalf("AWG IPC should accept any inner source: %s", ipc)
	}
	if !strings.Contains(ipc, "persistent_keepalive_interval=25") {
		t.Fatalf("AWG IPC missing persistent keepalive: %s", ipc)
	}
	if !strings.Contains(ipc, "jc=") {
		t.Fatal("AWG IPC missing lowercase profile")
	}

	wgIPC, err := cfg.NativeDeviceConfig(ModeWG)
	if err != nil {
		t.Fatalf("NativeDeviceConfig(wg): %v", err)
	}
	if strings.Contains(wgIPC, "jc=") {
		t.Fatal("WG IPC must not contain AWG options")
	}
	if !strings.Contains(wgIPC, "allowed_ip=0.0.0.0/0") {
		t.Fatalf("WG IPC should accept any inner source: %s", wgIPC)
	}
}

func TestNativeConfigRejectsDefaultRoute(t *testing.T) {
	cfg := testClientConfig()
	cfg.AWGEndpoint = "203.0.113.7:51821"
	cfg.Routes = []string{"10.77.0.1/0"}
	if _, err := cfg.NativeConfig(ModeAWG); err == nil {
		t.Fatal("NativeConfig accepted default route")
	}
}

func TestNativeConfigRejectsIPv6Route(t *testing.T) {
	cfg := testClientConfig()
	cfg.AWGEndpoint = "203.0.113.7:51821"
	cfg.Routes = []string{"2001:db8::/32"}
	if _, err := cfg.NativeConfig(ModeAWG); err == nil {
		t.Fatal("NativeConfig accepted IPv6 route")
	}
}

func TestNativeConfigRejectsBadMode(t *testing.T) {
	cfg := testClientConfig()
	cfg.AWGEndpoint = "203.0.113.7:51821"
	if _, err := cfg.NativeConfig("ext-utp"); err == nil {
		t.Fatal("NativeConfig accepted carrier mode")
	}
}

func TestNativeConfigRejectsMissingEndpoint(t *testing.T) {
	cfg := testClientConfig()
	if _, err := cfg.NativeConfig(ModeAWG); err == nil {
		t.Fatal("NativeConfig(awg) accepted missing AWGEndpoint")
	}
	cfg.AWGEndpoint = "not-an-endpoint"
	if _, err := cfg.NativeConfig(ModeAWG); err == nil {
		t.Fatal("NativeConfig(awg) accepted invalid endpoint")
	}
}

func TestNativeConfigRejectsInvalidRoute(t *testing.T) {
	cfg := testClientConfig()
	cfg.AWGEndpoint = "203.0.113.7:51821"
	cfg.Routes = []string{"not-a-prefix"}
	if _, err := cfg.NativeConfig(ModeAWG); err == nil {
		t.Fatal("NativeConfig accepted invalid route")
	}
}

func TestImportRoundTripWithNativeFields(t *testing.T) {
	cfg := testClientConfig()
	cfg.CarrierHashes = map[string]string{
		"ext-utp": testInfoHash,
		"ext-tcp": "fedcba9876543210fedcba9876543210fedcba98",
	}
	cfg.AWGEndpoint = "203.0.113.7:51821"
	cfg.WGEndpoint = "203.0.113.7:51820"
	cfg.Routes = []string{"10.77.0.0/24", "10.88.0.0/24"}

	s, err := cfg.ImportString()
	if err != nil {
		t.Fatalf("ImportString: %v", err)
	}
	got, err := ParseImportString(s)
	if err != nil {
		t.Fatalf("ParseImportString: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, cfg)
	}
}
