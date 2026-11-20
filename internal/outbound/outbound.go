// Package outbound enforces SSRF, DNS-rebinding, port-normalization, redirect,
// and error-sanitization controls for outbound HTTP and SMTP connections.
package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrBlockedAddress reports an outbound destination denied by SSRF policy.
	ErrBlockedAddress = errors.New("outbound target address is blocked by policy")
	// ErrRedirectBlocked reports a refused 3xx HTTP redirect.
	ErrRedirectBlocked = errors.New("outbound HTTP redirects are disabled")
	// ErrInvalidTarget reports a malformed URL, host, or port.
	ErrInvalidTarget = errors.New("invalid outbound target")
)

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // RFC 1122 "this network"
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC 1918 private
	netip.MustParsePrefix("100.64.0.0/10"),  // RFC 6598 CGNAT / Alibaba Cloud metadata (100.100.100.200)
	netip.MustParsePrefix("127.0.0.0/8"),    // RFC 1122 loopback
	netip.MustParsePrefix("169.254.0.0/16"), // RFC 3927 link-local / cloud metadata (169.254.169.254)
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC 1918 private
	netip.MustParsePrefix("192.0.0.0/24"),   // RFC 6890 IETF protocol assignments
	netip.MustParsePrefix("192.168.0.0/16"), // RFC 1918 private
	netip.MustParsePrefix("198.18.0.0/15"),  // RFC 2544 benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // RFC 1112 reserved / broadcast
	netip.MustParsePrefix("::/128"),         // unspecified
	netip.MustParsePrefix("::1/128"),        // loopback
	netip.MustParsePrefix("100::/64"),       // RFC 6666 discard
	netip.MustParsePrefix("64:ff9b::/96"),   // RFC 6052 NAT64 well-known
	netip.MustParsePrefix("64:ff9b:1::/48"), // RFC 8215 NAT64 local-use
	netip.MustParsePrefix("fc00::/7"),       // RFC 4193 unique local
	netip.MustParsePrefix("fe80::/10"),      // RFC 4291 link-local
	netip.MustParsePrefix("ff00::/8"),       // multicast
}

var metadataPrefixes = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("100.100.100.200/32"),
	netip.MustParsePrefix("fe80::/10"),
}

// AllowedHost authorizes one explicit IP or hostname (and optional port).
type AllowedHost struct {
	Host string
	IP   netip.Addr
	Port int // 0 matches any port
}

// Policy governs which outbound destinations may be dialed. The zero value is
// strict: only public global-unicast IP addresses are allowed.
type Policy struct {
	AllowPrivate bool
	AllowedCIDRs []netip.Prefix
	AllowedHosts []AllowedHost
	BlockedPorts map[int]struct{}
}

// Resolver resolves hostnames to IP addresses for SafeDialer.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// SafeDialer resolves DNS up front, validates every returned IP against Policy,
// and dials the validated IP literal directly so DNS rebinding cannot occur.
type SafeDialer struct {
	Policy   Policy
	Dialer   *net.Dialer
	Resolver Resolver
}

// ParseAllowlist parses operator-configured outbound allowlist entries. Each
// entry may be a CIDR (10.0.0.0/8, 127.0.0.1/32), IP (127.0.0.1, ::1),
// host:port (127.0.0.1:11434, localhost:11434, [::1]:11434), or hostname.
func ParseAllowlist(entries []string) (Policy, error) {
	var policy Policy
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				return Policy{}, fmt.Errorf("%w: invalid outbound allowlist CIDR %q", ErrInvalidTarget, entry)
			}
			addr := prefix.Addr().Unmap()
			bits := prefix.Bits()
			if prefix.Addr().Is4In6() && bits >= 96 {
				bits -= 96
			}
			policy.AllowedCIDRs = append(policy.AllowedCIDRs, netip.PrefixFrom(addr, bits).Masked())
			continue
		}
		if addr, err := netip.ParseAddr(entry); err == nil {
			policy.AllowedHosts = append(policy.AllowedHosts, AllowedHost{IP: addr.Unmap()})
			continue
		}
		if host, portStr, err := net.SplitHostPort(entry); err == nil {
			port, _, portErr := NormalizePort(portStr)
			if portErr != nil {
				return Policy{}, fmt.Errorf("%w: invalid outbound allowlist port in %q", ErrInvalidTarget, entry)
			}
			host = strings.TrimSpace(host)
			if host == "" || hasControlOrSpace(host) {
				return Policy{}, fmt.Errorf("%w: invalid outbound allowlist host %q", ErrInvalidTarget, entry)
			}
			if addr, ipErr := netip.ParseAddr(host); ipErr == nil {
				policy.AllowedHosts = append(policy.AllowedHosts, AllowedHost{IP: addr.Unmap(), Port: port})
			} else {
				policy.AllowedHosts = append(
					policy.AllowedHosts,
					AllowedHost{Host: strings.ToLower(strings.TrimSuffix(host, ".")), Port: port},
				)
			}
			continue
		}
		if hasControlOrSpace(entry) || strings.Contains(entry, ":") {
			return Policy{}, fmt.Errorf("%w: invalid outbound allowlist entry %q", ErrInvalidTarget, entry)
		}
		policy.AllowedHosts = append(
			policy.AllowedHosts,
			AllowedHost{Host: strings.ToLower(strings.TrimSuffix(entry, "."))},
		)
	}
	return policy, nil
}

// NormalizePort validates a decimal TCP port without leading zeros or sign.
func NormalizePort(raw string) (int, string, error) {
	if raw == "" || len(raw) > 5 {
		return 0, "", fmt.Errorf("%w: invalid port", ErrInvalidTarget)
	}
	if raw[0] == '0' {
		return 0, "", fmt.Errorf("%w: port must not have leading zeros", ErrInvalidTarget)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, "", fmt.Errorf("%w: invalid port", ErrInvalidTarget)
		}
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, "", fmt.Errorf("%w: port out of range", ErrInvalidTarget)
	}
	return port, strconv.Itoa(port), nil
}

// IsBlockedIP reports whether ip falls into loopback, private, link-local,
// cloud metadata, CGNAT, multicast, unspecified, or reserved address space.
func IsBlockedIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func isMetadataIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, prefix := range metadataPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// CheckIP verifies whether dialing ip (for original host and normalized port)
// is permitted by Policy.
func (p Policy) CheckIP(ip netip.Addr, host string, port int) error {
	if !ip.IsValid() {
		return ErrBlockedAddress
	}
	ip = ip.Unmap()
	// Unspecified addresses (0.0.0.0/8, ::) are never valid outbound targets.
	if ip.IsUnspecified() || (ip.Is4() && ip.As4()[0] == 0) {
		return ErrBlockedAddress
	}
	// Platform storage and gateway ports on loopback are always denied, even
	// when loopback is allowlisted for a local webhook or Ollama server.
	if ip.IsLoopback() && len(p.BlockedPorts) > 0 {
		if _, blocked := p.BlockedPorts[port]; blocked {
			return ErrBlockedAddress
		}
	}
	if p.AllowPrivate {
		return nil
	}
	if !IsBlockedIP(ip) {
		return nil
	}
	for _, prefix := range p.AllowedCIDRs {
		if prefix.Contains(ip) {
			return nil
		}
	}
	normalizedHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, entry := range p.AllowedHosts {
		if entry.Port != 0 && entry.Port != port {
			continue
		}
		if entry.IP.IsValid() && entry.IP == ip {
			return nil
		}
		if entry.Host != "" && entry.Host == normalizedHost && !isMetadataIP(ip) {
			return nil
		}
	}
	return ErrBlockedAddress
}

// ValidateURL parses an outbound http/https URL, normalizes its port, and
// checks literal IP addresses and localhost names against Policy.
func ValidateURL(raw string, policy Policy) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: url is required", ErrInvalidTarget)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed url", ErrInvalidTarget)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Opaque != "" {
		return nil, fmt.Errorf("%w: url scheme must be http or https", ErrInvalidTarget)
	}
	host := parsed.Hostname()
	if host == "" || hasControlOrSpace(host) {
		return nil, fmt.Errorf("%w: url host is required", ErrInvalidTarget)
	}
	port, err := extractURLPort(parsed, scheme)
	if err != nil {
		return nil, err
	}
	if err := checkLiteralHost(host, port, policy); err != nil {
		return nil, err
	}
	return parsed, nil
}

// ValidateHostPort checks an outbound TCP host and port (such as SMTP) against
// syntax, port normalization, and literal IP policy.
func ValidateHostPort(host string, port int, policy Policy) error {
	host = strings.TrimSpace(host)
	if host == "" || hasControlOrSpace(host) {
		return fmt.Errorf("%w: host is required", ErrInvalidTarget)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: port out of range", ErrInvalidTarget)
	}
	return checkLiteralHost(host, port, policy)
}

func extractURLPort(parsed *url.URL, scheme string) (int, error) {
	rawPort := parsed.Port()
	if rawPort != "" {
		port, _, err := NormalizePort(rawPort)
		if err != nil {
			return 0, err
		}
		return port, nil
	}
	// Reject empty trailing colon such as http://example.com:/hook.
	hostPort := parsed.Host
	if strings.HasPrefix(hostPort, "[") {
		if idx := strings.LastIndex(hostPort, "]"); idx >= 0 && idx+1 < len(hostPort) {
			return 0, fmt.Errorf("%w: invalid port", ErrInvalidTarget)
		}
	} else if strings.Contains(hostPort, ":") {
		return 0, fmt.Errorf("%w: invalid port", ErrInvalidTarget)
	}
	if scheme == "https" {
		return 443, nil
	}
	return 80, nil
}

func checkLiteralHost(host string, port int, policy Policy) error {
	if addr, err := netip.ParseAddr(host); err == nil {
		return policy.CheckIP(addr, host, port)
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		if err := policy.CheckIP(netip.AddrFrom4([4]byte{127, 0, 0, 1}), host, port); err != nil {
			return err
		}
		if err := policy.CheckIP(netip.MustParseAddr("::1"), host, port); err != nil {
			return err
		}
	}
	return nil
}

// NewDialer constructs a SafeDialer for the given Policy.
func NewDialer(policy Policy) *SafeDialer {
	return &SafeDialer{
		Policy: policy,
		Dialer: &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		},
	}
}

func (d *SafeDialer) dialer() *net.Dialer {
	if d != nil && d.Dialer != nil {
		return d.Dialer
	}
	return &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
}

func (d *SafeDialer) resolver() Resolver {
	if d != nil && d.Resolver != nil {
		return d.Resolver
	}
	return net.DefaultResolver
}

// DialContext resolves host, validates all resolved IPs against d.Policy, and
// dials a validated IP directly to prevent DNS rebinding.
func (d *SafeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var ipNetwork string
	switch network {
	case "tcp":
		ipNetwork = "ip"
	case "tcp4":
		ipNetwork = "ip4"
	case "tcp6":
		ipNetwork = "ip6"
	default:
		return nil, fmt.Errorf("%w: unsupported network %q", ErrInvalidTarget, network)
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid address", ErrInvalidTarget)
	}
	port, canonicalPort, err := NormalizePort(portStr)
	if err != nil {
		return nil, err
	}
	host = strings.TrimSpace(host)
	if host == "" || hasControlOrSpace(host) {
		return nil, fmt.Errorf("%w: invalid host", ErrInvalidTarget)
	}
	if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
		if err := d.Policy.CheckIP(addr, host, port); err != nil {
			return nil, err
		}
		return d.dialer().DialContext(ctx, network, net.JoinHostPort(addr.Unmap().String(), canonicalPort))
	}
	addrs, err := d.resolver().LookupNetIP(ctx, ipNetwork, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: host resolved to no addresses", ErrInvalidTarget)
	}
	// Reject if any resolved address violates policy so dual-record DNS
	// responses cannot slip a private address past validation.
	for _, addr := range addrs {
		if err := d.Policy.CheckIP(addr, host, port); err != nil {
			return nil, err
		}
	}
	var lastErr error
	for _, addr := range addrs {
		conn, dialErr := d.dialer().DialContext(ctx, network, net.JoinHostPort(addr.Unmap().String(), canonicalPort))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// RejectRedirects disables HTTP 3xx redirect following on outbound clients so
// credentials (including non-standard headers like Anthropic x-api-key) are
// never forwarded to another host and redirects cannot pivot to internal IPs.
func RejectRedirects(_ *http.Request, _ []*http.Request) error {
	return ErrRedirectBlocked
}

type safeRoundTripper struct {
	policy Policy
	base   http.RoundTripper
}

func (rt *safeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, ErrInvalidTarget
	}
	if _, err := ValidateURL(req.URL.String(), rt.policy); err != nil {
		return nil, err
	}
	return rt.base.RoundTrip(req)
}

// NewHTTPClient builds an outbound HTTP client with SafeDialer, pre-request URL
// validation, and redirects disabled.
func NewHTTPClient(timeout time.Duration, policy Policy) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := NewDialer(policy)
	if timeout > 0 && timeout < dialer.Dialer.Timeout {
		dialer.Dialer.Timeout = timeout
	}
	transport.DialContext = dialer.DialContext
	return &http.Client{
		Timeout:       timeout,
		Transport:     &safeRoundTripper{policy: policy, base: transport},
		CheckRedirect: RejectRedirects,
	}
}

func hasControlOrSpace(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}
