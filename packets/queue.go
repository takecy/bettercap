package packets

import (
	"bytes"
	"fmt"
	"net"
	"sync"

	"github.com/bettercap/bettercap/network"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
)

type Activity struct {
	IP     net.IP
	MAC    net.HardwareAddr
	Source bool
}

type Traffic struct {
	Sent     uint64
	Received uint64
}

type Stats struct {
	sync.RWMutex

	Sent        uint64
	Received    uint64
	PktReceived uint64
	Errors      uint64
}

type PacketCallback func(pkt gopacket.Packet)

type Queue struct {
	sync.RWMutex

	Activities chan Activity `json:"-"`

	Stats   Stats
	Protos  map[string]uint64
	Traffic map[string]*Traffic

	iface  *network.Endpoint
	handle *pcap.Handle
	source *gopacket.PacketSource
	pktCb  PacketCallback
	active bool
}

func NewQueue(iface *network.Endpoint) (q *Queue, err error) {
	q = &Queue{
		Protos:     make(map[string]uint64),
		Traffic:    make(map[string]*Traffic),
		Activities: make(chan Activity),

		iface:  iface,
		active: !iface.IsMonitor(),
		pktCb:  nil,
	}

	if q.active == true {
		if q.handle, err = pcap.OpenLive(iface.Name(), 1024, true, pcap.BlockForever); err != nil {
			return
		}

		q.source = gopacket.NewPacketSource(q.handle, q.handle.LinkType())
		go q.worker()
	}

	return
}

func (q *Queue) OnPacket(cb PacketCallback) {
	q.Lock()
	defer q.Unlock()
	q.pktCb = cb
}

func (q *Queue) onPacketCallback(pkt gopacket.Packet) {
	q.RLock()
	defer q.RUnlock()

	if q.pktCb != nil {
		q.pktCb(pkt)
	}
}

func (q *Queue) trackProtocols(pkt gopacket.Packet) {
	// gather protocols stats
	pktLayers := pkt.Layers()
	for _, layer := range pktLayers {
		proto := layer.LayerType()
		if proto == gopacket.LayerTypeDecodeFailure || proto == gopacket.LayerTypePayload {
			continue
		}

		q.Lock()
		name := proto.String()
		if _, found := q.Protos[name]; found == false {
			q.Protos[name] = 1
		} else {
			q.Protos[name] += 1
		}
		q.Unlock()
	}
}

func (q *Queue) trackActivity(eth *layers.Ethernet, ip4 *layers.IPv4, address net.IP, pktSize uint64, isSent bool) {
	// push to activity channel
	q.Activities <- Activity{
		IP:     address,
		MAC:    eth.SrcMAC,
		Source: isSent,
	}

	q.Lock()
	defer q.Unlock()

	// initialize or update stats
	addr := address.String()
	if _, found := q.Traffic[addr]; found == false {
		if isSent {
			q.Traffic[addr] = &Traffic{Sent: pktSize}
		} else {
			q.Traffic[addr] = &Traffic{Received: pktSize}
		}
	} else {
		if isSent {
			q.Traffic[addr].Sent += pktSize
		} else {
			q.Traffic[addr].Received += pktSize
		}
	}
}

func (q *Queue) worker() {
	for pkt := range q.source.Packets() {
		if q.active == false {
			return
		}

		q.trackProtocols(pkt)

		pktSize := uint64(len(pkt.Data()))

		q.Stats.Lock()

		q.Stats.PktReceived++
		q.Stats.Received += pktSize

		q.Stats.Unlock()

		q.onPacketCallback(pkt)

		// decode eth and ipv4 layers
		leth := pkt.Layer(layers.LayerTypeEthernet)
		lip4 := pkt.Layer(layers.LayerTypeIPv4)
		if leth != nil && lip4 != nil {
			eth := leth.(*layers.Ethernet)
			ip4 := lip4.(*layers.IPv4)

			// coming from our network
			if bytes.Compare(q.iface.IP, ip4.SrcIP) != 0 && q.iface.Net.Contains(ip4.SrcIP) {
				q.trackActivity(eth, ip4, ip4.SrcIP, pktSize, true)
			}
			// coming to our network
			if bytes.Compare(q.iface.IP, ip4.DstIP) != 0 && q.iface.Net.Contains(ip4.DstIP) {
				q.trackActivity(eth, ip4, ip4.DstIP, pktSize, false)
			}
		}
	}
}

func (q *Queue) Send(raw []byte) error {
	q.Lock()
	defer q.Unlock()

	if q.active == false {
		return fmt.Errorf("Packet queue is not active.")
	}

	if err := q.handle.WritePacketData(raw); err != nil {
		q.Stats.Lock()
		q.Stats.Errors++
		q.Stats.Unlock()
		return err
	} else {
		q.Stats.Lock()
		q.Stats.Sent += uint64(len(raw))
		q.Stats.Unlock()
	}

	return nil
}

func (q *Queue) Stop() {
	q.Lock()
	defer q.Unlock()

	if q.active == true {
		q.handle.Close()
		q.active = false
	}
}
