package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// SSRF guard + OpenGraph metadata parser for the /api/og endpoint.
// Two layers of defence:
//   1. isPrivateHost rejects obviously-internal destinations at URL-parse time.
//   2. safeSSRFTransport re-resolves at dial time and refuses any private IP
//      that came back, closing the DNS-rebinding TOCTOU window.

// isPrivateIP reports whether ip is one we never want to dial out to. Covers
// loopback, RFC1918, link-local, multicast, unspecified, IPv4-broadcast, plus
// 100.64/10 (carrier-grade NAT) and the 169.254 metadata range.
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip.Equal(net.IPv4bcast) {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1]&0xc0 == 64 {
			return true
		}
		if v4[0] == 169 && v4[1] == 254 {
			return true
		}
	}
	return false
}

// isPrivateHost is the fast-fail check at URL-parse time. Real enforcement
// is in safeSSRFTransport's dial.
func isPrivateHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return true
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return true
		}
	}
	return false
}

var errBlockedDestination = errors.New("destination blocked: private/loopback IP")

// safeSSRFTransport returns an http.Transport whose dial resolves the host
// itself and rejects private IPs at connect time, closing the lookup-then-dial
// TOCTOU that isPrivateHost can't (DNS rebinding). Re-enters on every redirect
// so the guard is enforced across the whole chain.
func safeSSRFTransport() *http.Transport {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if isPrivateIP(ip.IP) {
					return nil, errBlockedDestination
				}
			}
			return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 6 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
	}
}

var (
	ogTagRE    = regexp.MustCompile(`(?is)<meta\s+[^>]*?(?:property|name)=["']\s*(og:[a-z:]+|twitter:[a-z:]+|description)\s*["'][^>]*?content=["']([^"']*)["']`)
	ogTagAltRE = regexp.MustCompile(`(?is)<meta\s+[^>]*?content=["']([^"']*)["'][^>]*?(?:property|name)=["']\s*(og:[a-z:]+|twitter:[a-z:]+|description)\s*["']`)
	titleRE    = regexp.MustCompile(`(?is)<title[^>]*>([^<]+)</title>`)
)

// parseOG extracts og:* / twitter:* / description / title from a fetched HTML
// page and promotes a few keys to the canonical names the dashboard expects.
func parseOG(html, base string) map[string]string {
	out := map[string]string{}
	add := func(k, v string) {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	for _, m := range ogTagRE.FindAllStringSubmatch(html, -1) {
		add(m[1], m[2])
	}
	for _, m := range ogTagAltRE.FindAllStringSubmatch(html, -1) {
		add(m[2], m[1])
	}
	if t := titleRE.FindStringSubmatch(html); len(t) > 1 {
		add("title", strings.TrimSpace(t[1]))
	}
	if v, ok := out["og:image"]; ok {
		out["image"] = absURL(base, v)
	} else if v, ok := out["twitter:image"]; ok {
		out["image"] = absURL(base, v)
	}
	if v, ok := out["og:title"]; ok {
		out["title"] = v
	}
	if v, ok := out["og:description"]; ok {
		out["description"] = v
	} else if v, ok := out["description"]; ok {
		out["description"] = v
	}
	if v, ok := out["og:site_name"]; ok {
		out["site"] = v
	}
	return out
}

func absURL(base, ref string) string {
	if ref == "" {
		return ref
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}
