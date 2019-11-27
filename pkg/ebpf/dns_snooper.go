// +build linux_bpf

package ebpf

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/google/gopacket/afpacket"
	bpflib "github.com/iovisor/gobpf/elf"
)

const (
	dnsCacheTTL              = 3 * time.Minute
	dnsCacheExpirationPeriod = 1 * time.Minute
	dnsCacheSize             = 100000
)

var _ ReverseDNS = &SocketFilterSnooper{}

// SocketFilterSnooper is a DNS traffic snooper built on top of an eBPF SOCKET_FILTER
type SocketFilterSnooper struct {
	source *packetSource
	parser *dnsParser
	cache  *reverseDNSCache
	exit   chan struct{}
	wg     sync.WaitGroup

	// packet telemetry
	captured       int64
	processed      int64
	dropped        int64
	polls          int64
	decodingErrors int64
}

// NewSocketFilterSnooper returns a new SocketFilterSnooper
func NewSocketFilterSnooper(filter *bpflib.SocketFilter) (*SocketFilterSnooper, error) {
	packetSrc, err := newPacketSource(filter)
	if err != nil {
		return nil, err
	}

	cache := newReverseDNSCache(dnsCacheSize, dnsCacheTTL, dnsCacheExpirationPeriod)
	snooper := &SocketFilterSnooper{
		source: packetSrc,
		parser: newDNSParser(),
		cache:  cache,
		exit:   make(chan struct{}),
	}

	// Start consuming packets
	snooper.wg.Add(1)
	go func() {
		snooper.pollPackets()
		snooper.wg.Done()
	}()

	// Start polling socket stats
	snooper.wg.Add(1)
	go func() {
		snooper.pollStats()
		snooper.wg.Done()
	}()

	return snooper, nil
}

// Resolve IPs to Names
func (s *SocketFilterSnooper) Resolve(connections []ConnectionStats) map[util.Address][]string {
	return s.cache.Get(connections, time.Now())
}

func (s *SocketFilterSnooper) GetStats() map[string]int64 {
	stats := s.cache.Stats()
	stats["socket_polls"] = atomic.SwapInt64(&s.polls, 0)
	stats["packets_processed"] = atomic.SwapInt64(&s.processed, 0)
	stats["packets_captured"] = atomic.SwapInt64(&s.captured, 0)
	stats["packets_dropped"] = atomic.SwapInt64(&s.dropped, 0)
	stats["decoding_errors"] = atomic.SwapInt64(&s.decodingErrors, 0)

	return stats
}

// Close terminates the DNS traffic snooper as well as the underlying socket and the attached filter
func (s *SocketFilterSnooper) Close() {
	close(s.exit)
	s.wg.Wait()
	s.source.Close()
	s.cache.Close()
}

// processPacket retrieves DNS information from the received packet data and adds it to
// the reverse DNS cache. The underlying packet data can't be referenced after this method
// call since gopacket re-uses it.
func (s *SocketFilterSnooper) processPacket(data []byte) {
	translation := s.parser.Parse(data)
	if translation == nil {
		atomic.AddInt64(&s.decodingErrors, 1)
		return
	}

	s.cache.Add(translation, time.Now())
}

func (s *SocketFilterSnooper) pollPackets() {
	for {
		data, _, err := s.source.ZeroCopyReadPacketData()

		// Properly synchronizes termination process
		select {
		case <-s.exit:
			return
		default:
		}

		if err == nil {
			s.processPacket(data)
			continue
		}

		// Immediately retry for EAGAIN
		if err == syscall.EAGAIN {
			continue
		}

		// Sleep briefly and try again
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *SocketFilterSnooper) pollStats() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var (
		prevPolls     int64
		prevProcessed int64
		prevCaptured  int64
		prevDropped   int64
	)

	for {
		select {
		case <-ticker.C:
			sourceStats, _ := s.source.Stats()
			_, socketStats, err := s.source.SocketStats()
			if err != nil {
				log.Errorf("error polling socket stats: %s", err)
				continue
			}

			atomic.AddInt64(&s.polls, sourceStats.Polls-prevPolls)
			atomic.AddInt64(&s.processed, sourceStats.Packets-prevProcessed)
			atomic.AddInt64(&s.captured, int64(socketStats.Packets())-prevCaptured)
			atomic.AddInt64(&s.dropped, int64(socketStats.Drops())-prevDropped)

			prevPolls = sourceStats.Polls
			prevProcessed = sourceStats.Packets
			prevCaptured = int64(socketStats.Packets())
			prevDropped = int64(socketStats.Drops())
		case <-s.exit:
			return
		}
	}
}

// packetSource provides a RAW_SOCKET attached to an eBPF SOCKET_FILTER
type packetSource struct {
	*afpacket.TPacket
	socketFilter *bpflib.SocketFilter
	socketFD     int
}

func newPacketSource(filter *bpflib.SocketFilter) (*packetSource, error) {
	rawSocket, err := afpacket.NewTPacket(
		afpacket.OptPollTimeout(1*time.Second),
		// This setup will require ~4Mb that is mmap'd into the process virtual space
		// More information here: https://www.kernel.org/doc/Documentation/networking/packet_mmap.txt
		afpacket.OptFrameSize(4096),
		afpacket.OptBlockSize(4096*128),
		afpacket.OptNumBlocks(8),
	)
	if err != nil {
		return nil, fmt.Errorf("error creating raw socket: %s", err)
	}

	// The underlying socket file descriptor is private, hence the use of reflection
	socketFD := int(reflect.ValueOf(rawSocket).Elem().FieldByName("fd").Int())

	// Attaches DNS socket filter to the RAW_SOCKET
	if err := bpflib.AttachSocketFilter(filter, socketFD); err != nil {
		return nil, fmt.Errorf("error attaching filter to socket: %s", err)
	}

	return &packetSource{
		TPacket:      rawSocket,
		socketFilter: filter,
		socketFD:     socketFD,
	}, nil
}

func (p *packetSource) Close() {
	if err := bpflib.DetachSocketFilter(p.socketFilter, p.socketFD); err != nil {
		log.Errorf("error detaching socket filter: %s", err)
	}

	p.TPacket.Close()
}
