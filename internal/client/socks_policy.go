package client

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// SOCKSBlockPolicy describes local SOCKS destinations that should be rejected
// before they open a tunnel stream.
type SOCKSBlockPolicy struct {
	Ports []int
	Hosts []string
	CIDRs []string
}

type socksBlockPolicy struct {
	ports map[int]struct{}
	hosts []string
	cidrs []*net.IPNet
}

func newSOCKSBlockPolicy(cfg SOCKSBlockPolicy) (socksBlockPolicy, error) {
	p := socksBlockPolicy{}
	for _, port := range cfg.Ports {
		if port < 1 || port > 65535 {
			return socksBlockPolicy{}, fmt.Errorf("invalid socks block port: %d", port)
		}
		if p.ports == nil {
			p.ports = make(map[int]struct{}, len(cfg.Ports))
		}
		p.ports[port] = struct{}{}
	}
	for _, host := range cfg.Hosts {
		host = normalizeSOCKSHostRule(host)
		if host != "" {
			p.hosts = append(p.hosts, host)
		}
	}
	for _, cidrText := range cfg.CIDRs {
		cidrText = strings.TrimSpace(cidrText)
		if cidrText == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(cidrText)
		if err != nil {
			return socksBlockPolicy{}, fmt.Errorf("invalid socks block cidr %q: %w", cidrText, err)
		}
		p.cidrs = append(p.cidrs, cidr)
	}
	return p, nil
}

func normalizeSOCKSHostRule(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func (p socksBlockPolicy) blocks(host string, port int) bool {
	if _, ok := p.ports[port]; ok {
		return true
	}

	host = normalizeSOCKSHostRule(host)
	if host == "" {
		return false
	}
	for _, rule := range p.hosts {
		if socksHostRuleMatches(rule, host) {
			return true
		}
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, cidr := range p.cidrs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func socksHostRuleMatches(rule, host string) bool {
	if strings.HasPrefix(rule, "*.") {
		suffix := strings.TrimPrefix(rule, "*.")
		return host == suffix || strings.HasSuffix(host, "."+suffix)
	}
	return host == rule
}

func (p socksBlockPolicy) blocksTarget(target string) bool {
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return false
	}
	return p.blocks(host, port)
}
