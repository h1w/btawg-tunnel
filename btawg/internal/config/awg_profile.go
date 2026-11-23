package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/amnezia-vpn/amneziawg-go/v3/device"

	"bittorrent-tunnel/btawg/internal/carrier"
)

// AWGRange preserves the legacy JSON numbers for single values and encodes
// ranges as strings. An empty optional range leaves the engine default alone.
type AWGRange string

func (r AWGRange) MarshalJSON() ([]byte, error) {
	if n, err := strconv.ParseUint(string(r), 10, 32); err == nil {
		return json.Marshal(n)
	}
	return json.Marshal(string(r))
}

func (r *AWGRange) UnmarshalJSON(data []byte) error {
	var n uint32
	if len(data) > 0 && data[0] >= '0' && data[0] <= '9' {
		if err := json.Unmarshal(data, &n); err != nil {
			return err
		}
		*r = AWGRange(strconv.FormatUint(uint64(n), 10))
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*r = AWGRange(s)
	return nil
}

// AWGProfile holds the AWG 3.1 device settings shared by the hub and clients
// and the optional client-side persistent keepalive override.
type AWGProfile struct {
	JC                     int      `json:"jc"`
	JMin                   int      `json:"jmin"`
	JMax                   int      `json:"jmax"`
	S1                     int      `json:"s1"`
	S2                     int      `json:"s2"`
	S3                     int      `json:"s3"`
	S4                     int      `json:"s4"`
	H1                     AWGRange `json:"h1"`
	H2                     AWGRange `json:"h2"`
	H3                     AWGRange `json:"h3"`
	H4                     AWGRange `json:"h4"`
	I1                     string   `json:"i1,omitempty"`
	I2                     string   `json:"i2,omitempty"`
	I3                     string   `json:"i3,omitempty"`
	I4                     string   `json:"i4,omitempty"`
	I5                     string   `json:"i5,omitempty"`
	HeaderProtectionKey    string   `json:"header_protection_key,omitempty"`
	ContentPaddingAddition AWGRange `json:"content_padding_addition,omitempty"`
	RekeyAfterTime         AWGRange `json:"rekey_after_time,omitempty"`
	RekeyTimeout           AWGRange `json:"rekey_timeout,omitempty"`
	RejectAfterTime        AWGRange `json:"reject_after_time,omitempty"`
	KeepaliveTimeout       AWGRange `json:"keepalive_timeout,omitempty"`
	MaxHandshakeAttempts   AWGRange `json:"max_handshake_attempts,omitempty"`
	PersistentKeepalive    AWGRange `json:"persistent_keepalive,omitempty"`
	RandomTrailers         bool     `json:"random_trailers,omitempty"`
	DisableCookies         bool     `json:"disable_cookies,omitempty"`
}

// DefaultProfile retains the original on-wire defaults.
func DefaultProfile() AWGProfile {
	return AWGProfile{JC: 4, JMin: 40, JMax: 100, S1: 32, S2: 32, S3: 32, S4: 32, H1: "101", H2: "102", H3: "103", H4: "104"}
}

// AWG31Profile returns the protected deployment preset, retaining an existing
// header key. Pass an empty key to generate a fresh deployment secret.
func AWG31Profile(key string) AWGProfile {
	if key == "" {
		var secret [32]byte
		rand.Read(secret[:])
		key = hex.EncodeToString(secret[:])
	}
	p := DefaultProfile()
	p.HeaderProtectionKey = key
	p.RandomTrailers = true
	return p
}

// DeviceIPC renders the profile for the embedded AWG engine.
func (p AWGProfile) DeviceIPC() string {
	var b strings.Builder
	p.writeDevice(&b, false)
	return b.String()
}

type awgOption struct{ ipc, ini, value string }

func (p AWGProfile) options() [23]awgOption {
	return [23]awgOption{
		{"jc", "Jc", strconv.Itoa(p.JC)}, {"jmin", "Jmin", strconv.Itoa(p.JMin)}, {"jmax", "Jmax", strconv.Itoa(p.JMax)},
		{"s1", "S1", strconv.Itoa(p.S1)}, {"s2", "S2", strconv.Itoa(p.S2)}, {"s3", "S3", strconv.Itoa(p.S3)}, {"s4", "S4", strconv.Itoa(p.S4)},
		{"h1", "H1", string(p.H1)}, {"h2", "H2", string(p.H2)}, {"h3", "H3", string(p.H3)}, {"h4", "H4", string(p.H4)},
		{"i1", "I1", p.I1}, {"i2", "I2", p.I2}, {"i3", "I3", p.I3}, {"i4", "I4", p.I4}, {"i5", "I5", p.I5},
		{"header_protection_key", "HeaderProtectionKey", p.HeaderProtectionKey},
		{"content_padding_addition", "ContentPaddingAddition", string(p.ContentPaddingAddition)},
		{"rekey_after_time", "RekeyAfterTime", string(p.RekeyAfterTime)},
		{"rekey_timeout", "RekeyTimeout", string(p.RekeyTimeout)},
		{"reject_after_time", "RejectAfterTime", string(p.RejectAfterTime)},
		{"keepalive_timeout", "KeepaliveTimeout", string(p.KeepaliveTimeout)},
		{"max_handshake_attempts", "MaxHandshakeAttempts", string(p.MaxHandshakeAttempts)},
	}
}

func (p AWGProfile) writeDevice(b *strings.Builder, ini bool) {
	write := func(ipc, quick, value string) {
		if ini {
			b.WriteString(quick)
			b.WriteString(" = ")
		} else {
			b.WriteString(ipc)
			b.WriteByte('=')
		}
		b.WriteString(value)
		b.WriteByte('\n')
	}
	for _, option := range p.options() {
		if option.value == "" {
			continue
		}
		if ini && option.ipc == "header_protection_key" {
			// The profile is validated at import, save, and startup.
			key, _ := hex.DecodeString(option.value)
			option.value = base64.StdEncoding.EncodeToString(key)
		}
		write(option.ipc, option.ini, option.value)
	}
	flag := "1"
	if ini {
		flag = "on"
	}
	if p.RandomTrailers {
		write("random_trailers", "RandomTrailers", flag)
	}
	if p.DisableCookies {
		write("disable_cookies", "DisableCookies", flag)
	}
}

// Validate checks AWG's wire invariants and the carrier datagram limit before
// passing user input to IpcSet or rendering a standalone config.
func (p AWGProfile) Validate() error {
	const maxPacket = carrier.BlockSize - 2
	if p == (AWGProfile{}) {
		return nil // legacy client imports can omit the profile
	}
	if p.JMin < 0 || p.JMax < p.JMin || p.JMax > maxPacket {
		return fmt.Errorf("junk sizes must satisfy 0 <= Jmin <= Jmax <= %d", maxPacket)
	}
	if p.JC < 0 || p.JC > 128 || p.JC > 0 && p.JMin == 0 {
		return fmt.Errorf("Jc must be 0–128 and Jmin must be positive when junk is enabled")
	}
	if p.HeaderProtectionKey != "" {
		key, err := hex.DecodeString(p.HeaderProtectionKey)
		if err != nil || len(key) != 32 || strings.Trim(p.HeaderProtectionKey, "0") == "" {
			return fmt.Errorf("header protection key must be a nonzero 32-byte hex key")
		}
	}
	pads := [...]int{p.S1, p.S2, p.S3, p.S4}
	base := [...]int{device.MessageInitiationSize, device.MessageResponseSize, device.MessageCookieReplySize, DefaultMTU + device.MessageTransportSize}
	for i, n := range pads {
		if n < 0 || n > maxPacket-base[i] {
			return fmt.Errorf("S%d exceeds the carrier datagram budget", i+1)
		}
		if p.HeaderProtectionKey != "" && n < device.HeaderCipherNonceSize {
			return fmt.Errorf("S%d must be at least %d with header protection", i+1, device.HeaderCipherNonceSize)
		}
	}
	var headers [4]device.UintRange
	for i, value := range [...]AWGRange{p.H1, p.H2, p.H3, p.H4} {
		if err := headers[i].FromString(string(value)); err != nil || headers[i].Hi()-headers[i].Lo() == ^uint32(0) {
			return fmt.Errorf("H%d must be a valid uint32 value or range", i+1)
		}
		for j := range i {
			if headers[i].Overlap(headers[j]) {
				return fmt.Errorf("H%d and H%d must not overlap", j+1, i+1)
			}
		}
	}
	for _, option := range [...]awgOption{
		{"", "ContentPaddingAddition", string(p.ContentPaddingAddition)}, {"", "RekeyAfterTime", string(p.RekeyAfterTime)},
		{"", "RekeyTimeout", string(p.RekeyTimeout)}, {"", "RejectAfterTime", string(p.RejectAfterTime)},
		{"", "KeepaliveTimeout", string(p.KeepaliveTimeout)}, {"", "MaxHandshakeAttempts", string(p.MaxHandshakeAttempts)},
		{"", "PersistentKeepalive", string(p.PersistentKeepalive)},
	} {
		if option.value == "" {
			continue
		}
		var r device.UintRange
		if err := r.FromString(option.value); err != nil || r.Hi() > 65535 || r.Lo() == 0 && !r.IsZero() {
			return fmt.Errorf("%s must be zero or a positive uint16 value or range", option.ini)
		}
	}
	for i, spec := range [...]string{p.I1, p.I2, p.I3, p.I4, p.I5} {
		if spec != "" && !validAWGTemplate(spec, maxPacket) {
			return fmt.Errorf("I%d must be a valid packet template emitting 1–%d bytes", i+1, maxPacket)
		}
	}
	return nil
}

// Templates are evaluated with an empty input by the AWG handshake sender.
// Check the resulting size before it can allocate a packet or inject UAPI lines.
func validAWGTemplate(spec string, limit int) bool {
	if strings.ContainsAny(spec, "\r\n#") {
		return false
	}
	remaining := limit
	for spec = strings.TrimSpace(spec); spec != ""; spec = strings.TrimSpace(spec) {
		if spec[0] != '<' {
			return false
		}
		tag, rest, ok := strings.Cut(spec[1:], ">")
		if !ok {
			return false
		}
		part := strings.Fields(tag)
		if len(part) < 1 || len(part) > 2 {
			return false
		}
		var size int
		switch part[0] {
		case "t", "d", "ds":
			if len(part) != 1 {
				return false
			}
			if part[0] == "t" {
				size = 4
			}
		case "b", "r", "rd", "rc", "dz":
			if len(part) != 2 {
				return false
			}
			if part[0] == "b" {
				value, err := hex.DecodeString(strings.TrimPrefix(part[1], "0x"))
				if err != nil || len(value) == 0 {
					return false
				}
				size = len(value)
			} else {
				n, err := strconv.ParseUint(part[1], 10, 16)
				if err != nil {
					return false
				}
				size = int(n)
			}
		default:
			return false
		}
		remaining -= size
		if remaining < 0 {
			return false
		}
		spec = rest
	}
	return remaining < limit
}
