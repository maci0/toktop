package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// --add endpoint and --ingest listen address parsing, and the cleartext and
// routability warnings they feed.

// routableBind reports whether a bound listen address is reachable from
// beyond this host: a wildcard or non-loopback interface rather than
// loopback. The ingest endpoint authenticates nothing, so such a bind is
// worth naming at startup instead of leaving the widening silent.
func routableBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false // no port to split: treat as local, not as an alarm
	}
	return host == "" || !localHost(host) // ":port" binds every interface
}

// localHost reports whether a host names this machine only. A literal loopback
// address, and the "localhost" name, are local; every other name is treated as
// routable without a lookup, so `--ingest box.internal:8420` is named at
// startup rather than staying silent because the name did not parse as an IP.
// A name that does resolve to loopback (ip6-localhost and friends) costs one
// extra warning line; the reverse mistake is a publicly reachable endpoint
// nobody was told about.
func localHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	return name == "localhost" || strings.HasSuffix(name, ".localhost")
}

// warnInsecureAdd names a --add endpoint that would receive the bearer token
// in cleartext: plain HTTP to a host that is not this machine. Loopback
// endpoints are exempt, as are the local engines discovery finds on their own.
func warnInsecureAdd(adds []string) {
	for _, raw := range adds {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || localHost(u.Hostname()) {
			continue
		}
		fmt.Fprintf(os.Stderr, "toktop: warning: %s is plain http, so the bearer token crosses the network in cleartext\n", u.Host)
	}
}

func parseAdd(v string, target *[]string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("empty URL")
	}
	if err := validateAddURL(v); err != nil {
		return err
	}
	// One poll per endpoint. Each --add builds its own provider, and the
	// dashboard sums them, so the same URL named twice reads as double the
	// tokens. Compared the way main.go polls it (trailing slashes trimmed),
	// since that is the form under which the two collide.
	endpoint := strings.TrimRight(v, "/")
	for _, seen := range *target {
		if strings.TrimRight(seen, "/") == endpoint {
			return fmt.Errorf("duplicate --add endpoint %q", v)
		}
	}
	*target = append(*target, v)
	return nil
}

// validateAddURL rejects values that cannot be polled as an OpenAI-compatible
// engine: missing scheme, non-http(s), no host, or credentials in the URL
// (those belong in --bearer / $TOKTOP_BEARER, not in argv or logs).
func validateAddURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL must be http:// or https://, got %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must be http:// or https://, got %q", raw)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("URL missing host, got %q", raw)
	}
	if port := u.Port(); port != "" {
		n, convErr := strconv.Atoi(port)
		if convErr != nil || n < 0 || n > 65535 {
			return fmt.Errorf("URL port must be 0-65535, got %q", port)
		}
	}
	if u.User != nil {
		return errors.New("URL must not contain userinfo; set --bearer or $TOKTOP_BEARER")
	}
	return nil
}
