package portmap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	ssdpTimeout         = 2 * time.Second
	httpTimeout         = time.Second
	maxXMLBytes         = 256 << 10
	maxIGDServices      = 8
	igdDevice           = "urn:schemas-upnp-org:device:InternetGatewayDevice:"
	wanDevice           = "urn:schemas-upnp-org:device:WANDevice:"
	wanConnectionDevice = "urn:schemas-upnp-org:device:WANConnectionDevice:"
	wanIP               = "urn:schemas-upnp-org:service:WANIPConnection:"
	wanPPP              = "urn:schemas-upnp-org:service:WANPPPConnection:1"
)

type igdService struct {
	client  *http.Client
	control string
	kind    string
}

func verifiedRouterURL(u *url.URL, gateway netip.Addr) bool {
	if u == nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.Fragment != "" {
		return false
	}
	// Literal IPv4 only: no DNS, rebinding, userinfo or alternate hosts.
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || ip != gateway {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		return err == nil && n != 0
	}
	return u.Host == gateway.String()
}

func (r router) igdHTTP() *http.Client {
	dial := net.Dialer{LocalAddr: net.TCPAddrFromAddrPort(netip.AddrPortFrom(r.source, 0)), Control: r.socketControl}
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			// Do not use the process's proxy settings for local router control.
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				c, err := dial.DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return &gatewayConn{Conn: c, router: r}, nil
			},
			DisableCompression:     true,
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 16 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type gatewayConn struct {
	net.Conn
	router router
}

func (c *gatewayConn) Write(p []byte) (int, error) {
	if err := c.router.validateSnapshot(); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func ssdpLocation(packet []byte) string {
	// Parsing input is bounded by the caller's datagram buffer. Do not
	// accept folded, ambiguous, or missing Location values.
	reader := bufio.NewReader(bytes.NewReader(packet))
	status, err := reader.ReadString('\n')
	if err != nil {
		return ""
	}
	fields := strings.Fields(status)
	if len(fields) < 2 || fields[0] != "HTTP/1.1" && fields[0] != "HTTP/1.0" || fields[1] != "200" {
		return ""
	}
	var location string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return ""
		}
		if line == "\r\n" || line == "\n" {
			return location
		}
		key, val, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(key, "location") {
			if location != "" {
				return ""
			}
			location = strings.TrimSpace(val)
			if location == "" {
				return ""
			}
		} else if line[0] == ' ' || line[0] == '\t' {
			return ""
		}
	}
}

func (r router) discoverIGD(parent context.Context) ([]igdService, error) {
	if !r.ssdp.IsValid() {
		return nil, errors.New("no SSDP destination")
	}
	ctx, cancel := context.WithTimeout(parent, ssdpTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(r.source, 0)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if r.ssdp.Addr().IsMulticast() {
		packet := ipv4.NewPacketConn(conn)
		if err := packet.SetMulticastInterface(&net.Interface{Index: r.ifindex}); err != nil {
			return nil, err
		}
		if err := packet.SetMulticastTTL(2); err != nil {
			return nil, err
		}
	}
	if err := r.validateSnapshot(); err != nil {
		return nil, err
	}
	for _, target := range [...]string{igdDevice + "2", igdDevice + "1", "upnp:rootdevice"} {
		if err := r.validateSnapshot(); err != nil {
			return nil, err
		}
		request := "M-SEARCH * HTTP/1.1\r\nHOST: " + r.ssdp.String() + "\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: " + target + "\r\n\r\n"
		if _, err := conn.WriteToUDPAddrPort([]byte(request), r.ssdp); err != nil {
			return nil, err
		}
	}

	const maxLocations = 6
	seen := make(map[string]struct{}, maxLocations)
	var packet [8193]byte
	client := r.igdHTTP()
	for i := 0; i < 64 && len(seen) < maxLocations && ctx.Err() == nil; i++ {
		n, from, err := conn.ReadFromUDPAddrPort(packet[:])
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if from.Addr().Unmap() != r.gateway || n == len(packet) {
			continue
		}
		location := ssdpLocation(packet[:n])
		if location == "" {
			continue
		}
		u, err := url.Parse(location)
		if err != nil || !verifiedRouterURL(u, r.gateway) {
			continue
		}
		if _, exists := seen[location]; exists {
			continue
		}
		seen[location] = struct{}{}
		if services := r.readIGD(ctx, client, u); len(services) > 0 {
			return services, nil
		}
	}
	return nil, errors.New("no supported IGD description from default gateway")
}

func boundedXML(body io.Reader, out any) error {
	data, err := io.ReadAll(io.LimitReader(body, maxXMLBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxXMLBytes {
		return errors.New("IGD XML exceeds size limit")
	}
	return xml.Unmarshal(data, out)
}

func (r router) readIGD(ctx context.Context, client *http.Client, description *url.URL) []igdService {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, description.String(), nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var root struct {
		XMLName xml.Name `xml:"root"`
		URLBase string   `xml:"URLBase"`
		Device  struct {
			Type string `xml:"deviceType"`
			WAN  []struct {
				Type        string `xml:"deviceType"`
				Connections []struct {
					Type     string `xml:"deviceType"`
					Services []struct {
						Type    string `xml:"serviceType"`
						Control string `xml:"controlURL"`
					} `xml:"serviceList>service"`
				} `xml:"deviceList>device"`
			} `xml:"deviceList>device"`
		} `xml:"device"`
	}
	if resp.StatusCode != http.StatusOK || boundedXML(resp.Body, &root) != nil || !isIGDDevice(root.Device.Type, igdDevice) {
		return nil
	}
	base := description
	if value := strings.TrimSpace(root.URLBase); value != "" {
		base, err = url.Parse(value)
		if err != nil || !verifiedRouterURL(base, r.gateway) {
			return nil
		}
	}
	var byRank [3][]igdService
	var count int
	for _, wan := range root.Device.WAN {
		if !isIGDDevice(wan.Type, wanDevice) {
			continue
		}
		for _, connection := range wan.Connections {
			if !isIGDDevice(connection.Type, wanConnectionDevice) {
				continue
			}
			for _, service := range connection.Services {
				if count >= maxIGDServices {
					break
				}
				kind := strings.TrimSpace(service.Type)
				var rank int
				switch kind {
				case wanIP + "2":
					rank = 0
				case wanIP + "1":
					rank = 1
				case wanPPP:
					rank = 2
				default:
					continue
				}
				control := strings.TrimSpace(service.Control)
				if control == "" {
					continue
				}
				u, err := base.Parse(control)
				if err != nil || !verifiedRouterURL(u, r.gateway) {
					continue
				}
				byRank[rank] = append(byRank[rank], igdService{client: client, control: u.String(), kind: kind})
				count++
			}
		}
	}
	if count == 0 {
		return nil
	}
	services := make([]igdService, 0, count)
	for _, group := range byRank {
		services = append(services, group...)
	}
	return services
}

func isIGDDevice(name, prefix string) bool {
	return strings.TrimSpace(name) == prefix+"1" || strings.TrimSpace(name) == prefix+"2"
}
