package ebpf

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

type reverseDNSCache struct {
	mux  sync.Mutex
	data map[util.Address]*dnsCacheVal
	exit chan struct{}
	ttl  time.Duration
	size int

	// Telemetry
	len      int64
	lookups  int64
	resolved int64
}

type translation struct {
	name string
	ips  map[util.Address]struct{}
}

type cacheStats struct {
	lookups  int64
	resolved int64
	len      int64
}

func newReverseDNSCache(size int, ttl, expirationPeriod time.Duration) *reverseDNSCache {
	cache := &reverseDNSCache{
		data: make(map[util.Address]*dnsCacheVal),
		exit: make(chan struct{}),
		ttl:  ttl,
		size: size,
	}

	ticker := time.NewTicker(expirationPeriod)
	go func() {
		for {
			select {
			case <-ticker.C:
				cache.expire()
			case <-cache.exit:
				return
			}
		}
	}()
	return cache
}

func (c *reverseDNSCache) Add(translation *translation, now time.Time) bool {
	if translation == nil {
		return false
	}

	c.mux.Lock()
	defer c.mux.Unlock()
	if len(c.data) >= c.size {
		return false
	}

	exp := now.Add(c.ttl).Unix()
	for addr := range translation.ips {
		val, ok := c.data[addr]
		if ok {
			val.expiration = exp
			val.merge(translation.name)
			continue
		}

		c.data[addr] = &dnsCacheVal{names: []string{translation.name}, expiration: exp}
	}

	// Update cache length for telemetry purposes
	atomic.StoreInt64(&c.len, int64(len(c.data)))

	return true
}

func (c *reverseDNSCache) Get(conns []ConnectionStats, now time.Time) []NamePair {
	if len(conns) == 0 {
		return nil
	}

	names := make([]NamePair, len(conns))
	expiration := now.Add(c.ttl).Unix()

	lookups := len(conns)
	resolved := 0

	c.mux.Lock()
	for i, conn := range conns {
		names[i].Source = c.getNamesForIP(conn.Source, expiration)
		names[i].Dest = c.getNamesForIP(conn.Dest, expiration)

		// Track number of successful resolutions for destination IP only
		if names[i].Dest != nil {
			resolved++
		}
	}
	c.mux.Unlock()

	// Update stats for telemetry
	atomic.AddInt64(&c.lookups, int64(lookups))
	atomic.AddInt64(&c.resolved, int64(resolved))

	return names
}

func (c *reverseDNSCache) Len() int {
	return int(atomic.LoadInt64(&c.len))
}

func (c *reverseDNSCache) Stats() cacheStats {
	var (
		lookups  = atomic.SwapInt64(&c.lookups, 0)
		resolved = atomic.SwapInt64(&c.resolved, 0)
	)

	return cacheStats{
		lookups:  lookups,
		resolved: resolved,
		len:      int64(c.Len()),
	}
}

func (c *reverseDNSCache) Close() {
	c.exit <- struct{}{}
}

func (c *reverseDNSCache) getNamesForIP(ip util.Address, expiration int64) []string {
	val, ok := c.data[ip]
	if !ok {
		return nil
	}

	val.expiration = expiration
	return val.copy()
}

func (c *reverseDNSCache) expire() {
	expired := 0
	start := time.Now()
	deadline := start.Add(-c.ttl).Unix()

	c.mux.Lock()
	for addr, val := range c.data {
		if val.expiration > deadline {
			continue
		}

		expired++
		delete(c.data, addr)
	}
	total := len(c.data)
	c.mux.Unlock()

	atomic.StoreInt64(&c.len, int64(total))
	log.Debugf(
		"dns entries expired. took=%s total=%d expired=%d\n",
		time.Now().Sub(start), total, expired,
	)
}

type dnsCacheVal struct {
	// opting for a []string instead of map[string]struct{} since common case is len(names) == 1
	names      []string
	expiration int64
}

func (v *dnsCacheVal) merge(name string) {
	if sort.SearchStrings(v.names, name) < len(v.names) {
		return
	}

	v.names = append(v.names, name)
	sort.Strings(v.names)
}

func (v *dnsCacheVal) copy() []string {
	cpy := make([]string, len(v.names))
	copy(cpy, v.names)
	return cpy
}
