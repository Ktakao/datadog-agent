// +build !windows

package dockerproxy

import (
	"strconv"
	"strings"

	model "github.com/DataDog/agent-payload/process"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/DataDog/gopsutil/process"
)

// Filter keeps track of every docker-proxy instance and filters network traffic going through them
type Filter struct {
	proxyByTarget map[target]*proxy

	// This "secondary index" is used only during the proxy IP discovery process
	proxyByPID map[int32]*proxy
}

type target struct {
	ip    string
	port  int32
	proto model.ConnectionType
}

type proxy struct {
	pid    int32
	ip     string
	target target
}

// NewFilter instantiates a new filter loaded with docker-proxy instance information
func NewFilter() *Filter {
	filter := new(Filter)
	if procs, err := process.AllProcesses(); err == nil {
		filter.LoadProxies(procs)
	} else {
		log.Errorf("error initiating proxy filter: %s", err)
	}

	return filter
}

// LoadProxies by inspecting processes information
func (f *Filter) LoadProxies(procs map[int32]*process.FilledProcess) {
	f.proxyByPID = make(map[int32]*proxy)
	f.proxyByTarget = make(map[target]*proxy)

	for _, p := range procs {
		proxy := extractProxyTarget(p)
		if proxy == nil {
			continue
		}

		log.Debugf("detected docker-proxy with pid=%d target.ip=%s target.port=%d target.proto=%s",
			proxy.pid,
			proxy.target.ip,
			proxy.target.port,
			proxy.target.proto,
		)

		// Add proxy to cache
		f.proxyByPID[proxy.pid] = proxy
		f.proxyByTarget[proxy.target] = proxy
	}
}

// Filter all connections that have a docker-proxy at one end
func (f *Filter) Filter(payload *model.Connections) {
	if len(f.proxyByPID) == 0 {
		return
	}

	// Discover proxy IPs
	// TODO: we can probably discard the whole logic below if we determine that each proxy
	// instance will be always bound to the docker0 IP
	for _, c := range payload.Conns {
		if len(f.proxyByPID) == 0 {
			break
		}

		if proxy, ok := f.proxyByPID[c.Pid]; ok {
			if proxyIP := f.discoverProxyIP(proxy, c); proxyIP != "" {
				proxy.ip = proxyIP
				delete(f.proxyByPID, c.Pid)
			}
		}
	}

	// Filter out proxy traffic
	filtered := make([]*model.Connection, 0, len(payload.Conns))
	for _, c := range payload.Conns {
		// If either end of the connection is a proxy we can drop it
		if f.isProxied(c) {
			continue
		}

		filtered = append(filtered, c)
	}

	payload.Conns = filtered
}

func (f *Filter) isProxied(c *model.Connection) bool {
	if p, ok := f.proxyByTarget[target{ip: c.Laddr.Ip, port: c.Laddr.Port, proto: c.Type}]; ok {
		return p.ip == c.Raddr.Ip
	}

	if p, ok := f.proxyByTarget[target{ip: c.Raddr.Ip, port: c.Raddr.Port, proto: c.Type}]; ok {
		return p.ip == c.Laddr.Ip
	}

	return false
}

func (f *Filter) discoverProxyIP(p *proxy, c *model.Connection) string {
	// The heuristic here goes as follows:
	// One of the ends of this connections must match p.targetAddr;
	// The proxy IP will be the other end;
	if c.Laddr.Ip == p.target.ip && c.Laddr.Port == p.target.port {
		return c.Raddr.Ip
	}

	if c.Raddr.Ip == p.target.ip && c.Raddr.Port == p.target.port {
		return c.Laddr.Ip
	}

	return ""
}

func extractProxyTarget(p *process.FilledProcess) *proxy {
	if len(p.Cmdline) == 0 || !strings.HasSuffix(p.Cmdline[0], "docker-proxy") {
		return nil
	}

	// Extract proxy target address
	proxy := &proxy{pid: p.Pid}
	for i := 0; i < len(p.Cmdline)-1; i++ {
		switch p.Cmdline[i] {
		case "-container-ip":
			proxy.target.ip = p.Cmdline[i+1]
		case "-container-port":
			port, err := strconv.Atoi(p.Cmdline[i+1])
			if err != nil {
				return nil
			}
			proxy.target.port = int32(port)
		case "-proto":
			name := p.Cmdline[i+1]
			proto, ok := model.ConnectionType_value[name]
			if !ok {
				return nil
			}
			proxy.target.proto = model.ConnectionType(proto)
		}
	}

	if proxy.target.ip == "" || proxy.target.port == 0 {
		return nil
	}

	return proxy
}