package requrl

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// Scheme returns the client-facing scheme: TLS, else the first
// X-Forwarded-Proto value when it is http or https, else http.
func Scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	if fwd = strings.ToLower(strings.TrimSpace(fwd)); fwd == "https" {
		return fwd
	}
	return "http"
}

// BaseURL returns the client-facing scheme://host, answering 400 on an
// invalid Host.
func BaseURL(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !ValidHost(r.Host) {
		http.Error(w, "invalid host header", http.StatusBadRequest)
		return "", false
	}
	return Scheme(r) + "://" + r.Host, true
}

// ValidHost accepts host[:port] where host is a DNS name or IP literal.
func ValidHost(hostport string) bool {
	host, port := hostport, ""
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		if p == "" {
			return false
		}
		host, port = h, p
	} else if strings.HasPrefix(hostport, "[") {
		return strings.HasSuffix(hostport, "]") && net.ParseIP(hostport[1:len(hostport)-1]) != nil
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if strings.HasPrefix(hostport, "[") {
		return false
	}
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' ||
			strings.Trim(strings.ToLower(label), "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			return false
		}
	}
	return true
}
