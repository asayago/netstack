// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ipv4 contains the implementation of the ipv4 network protocol. To use
// it in the networking stack, this package must be added to the project, and
// activated on the stack by passing ipv4.ProtocolName (or "ipv4") as one of the
// network protocols when calling stack.New(). Then endpoints can be created
// by passing ipv4.ProtocolNumber as the network protocol number when calling
// Stack.NewEndpoint().
package ipv4

import (
	"sync/atomic"

	"gvisor.googlesource.com/gvisor/pkg/tcpip"
	"gvisor.googlesource.com/gvisor/pkg/tcpip/buffer"
	"gvisor.googlesource.com/gvisor/pkg/tcpip/header"
	"gvisor.googlesource.com/gvisor/pkg/tcpip/network/fragmentation"
	"gvisor.googlesource.com/gvisor/pkg/tcpip/network/hash"
	"gvisor.googlesource.com/gvisor/pkg/tcpip/stack"
)

const (
	// ProtocolName is the string representation of the ipv4 protocol name.
	ProtocolName = "ipv4"

	// ProtocolNumber is the ipv4 protocol number.
	ProtocolNumber = header.IPv4ProtocolNumber

	// MaxTotalSize is maximum size that can be encoded in the 16-bit
	// TotalLength field of the ipv4 header.
	MaxTotalSize = 0xffff

	// buckets is the number of identifier buckets.
	buckets = 2048
)

type endpoint struct {
	nicid         tcpip.NICID
	id            stack.NetworkEndpointID
	linkEP        stack.LinkEndpoint
	dispatcher    stack.TransportDispatcher
	fragmentation *fragmentation.Fragmentation
}

// NewEndpoint creates a new ipv4 endpoint.
func (p *protocol) NewEndpoint(nicid tcpip.NICID, addr tcpip.Address, linkAddrCache stack.LinkAddressCache, dispatcher stack.TransportDispatcher, linkEP stack.LinkEndpoint) (stack.NetworkEndpoint, *tcpip.Error) {
	e := &endpoint{
		nicid:         nicid,
		id:            stack.NetworkEndpointID{LocalAddress: addr},
		linkEP:        linkEP,
		dispatcher:    dispatcher,
		fragmentation: fragmentation.NewFragmentation(fragmentation.HighFragThreshold, fragmentation.LowFragThreshold, fragmentation.DefaultReassembleTimeout),
	}

	return e, nil
}

// DefaultTTL is the default time-to-live value for this endpoint.
func (e *endpoint) DefaultTTL() uint8 {
	return 255
}

// MTU implements stack.NetworkEndpoint.MTU. It returns the link-layer MTU minus
// the network layer max header length.
func (e *endpoint) MTU() uint32 {
	return calculateMTU(e.linkEP.MTU())
}

// Capabilities implements stack.NetworkEndpoint.Capabilities.
func (e *endpoint) Capabilities() stack.LinkEndpointCapabilities {
	return e.linkEP.Capabilities()
}

// NICID returns the ID of the NIC this endpoint belongs to.
func (e *endpoint) NICID() tcpip.NICID {
	return e.nicid
}

// ID returns the ipv4 endpoint ID.
func (e *endpoint) ID() *stack.NetworkEndpointID {
	return &e.id
}

// MaxHeaderLength returns the maximum length needed by ipv4 headers (and
// underlying protocols).
func (e *endpoint) MaxHeaderLength() uint16 {
	return e.linkEP.MaxHeaderLength() + header.IPv4MinimumSize
}

// GSOMaxSize returns the maximum GSO packet size.
func (e *endpoint) GSOMaxSize() uint32 {
	if gso, ok := e.linkEP.(stack.GSOEndpoint); ok {
		return gso.GSOMaxSize()
	}
	return 0
}

// WritePacket writes a packet to the given destination address and protocol.
func (e *endpoint) WritePacket(r *stack.Route, gso *stack.GSO, hdr buffer.Prependable, payload buffer.VectorisedView, protocol tcpip.TransportProtocolNumber, ttl uint8, loop stack.PacketLooping) *tcpip.Error {
	ip := header.IPv4(hdr.Prepend(header.IPv4MinimumSize))
	length := uint16(hdr.UsedLength() + payload.Size())
	id := uint32(0)
	if length > header.IPv4MaximumHeaderSize+8 {
		// Packets of 68 bytes or less are required by RFC 791 to not be
		// fragmented, so we only assign ids to larger packets.
		id = atomic.AddUint32(&ids[hashRoute(r, protocol)%buckets], 1)
	}
	ip.Encode(&header.IPv4Fields{
		IHL:         header.IPv4MinimumSize,
		TotalLength: length,
		ID:          uint16(id),
		TTL:         ttl,
		Protocol:    uint8(protocol),
		SrcAddr:     r.LocalAddress,
		DstAddr:     r.RemoteAddress,
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	if loop&stack.PacketLoop != 0 {
		views := make([]buffer.View, 1, 1+len(payload.Views()))
		views[0] = hdr.View()
		views = append(views, payload.Views()...)
		vv := buffer.NewVectorisedView(len(views[0])+payload.Size(), views)
		e.HandlePacket(r, vv)
	}
	if loop&stack.PacketOut == 0 {
		return nil
	}

	r.Stats().IP.PacketsSent.Increment()
	return e.linkEP.WritePacket(r, gso, hdr, payload, ProtocolNumber)
}

// HandlePacket is called by the link layer when new ipv4 packets arrive for
// this endpoint.
func (e *endpoint) HandlePacket(r *stack.Route, vv buffer.VectorisedView) {
	headerView := vv.First()
	h := header.IPv4(headerView)
	if !h.IsValid(vv.Size()) {
		return
	}

	hlen := int(h.HeaderLength())
	tlen := int(h.TotalLength())
	vv.TrimFront(hlen)
	vv.CapLength(tlen - hlen)

	more := (h.Flags() & header.IPv4FlagMoreFragments) != 0
	if more || h.FragmentOffset() != 0 {
		// The packet is a fragment, let's try to reassemble it.
		last := h.FragmentOffset() + uint16(vv.Size()) - 1
		var ready bool
		vv, ready = e.fragmentation.Process(hash.IPv4FragmentHash(h), h.FragmentOffset(), last, more, vv)
		if !ready {
			return
		}
	}
	p := h.TransportProtocol()
	if p == header.ICMPv4ProtocolNumber {
		headerView.CapLength(hlen)
		e.handleICMP(r, headerView, vv)
		return
	}
	r.Stats().IP.PacketsDelivered.Increment()
	e.dispatcher.DeliverTransportPacket(r, p, headerView, vv)
}

// Close cleans up resources associated with the endpoint.
func (e *endpoint) Close() {}

type protocol struct{}

// NewProtocol creates a new protocol ipv4 protocol descriptor. This is exported
// only for tests that short-circuit the stack. Regular use of the protocol is
// done via the stack, which gets a protocol descriptor from the init() function
// below.
func NewProtocol() stack.NetworkProtocol {
	return &protocol{}
}

// Number returns the ipv4 protocol number.
func (p *protocol) Number() tcpip.NetworkProtocolNumber {
	return ProtocolNumber
}

// MinimumPacketSize returns the minimum valid ipv4 packet size.
func (p *protocol) MinimumPacketSize() int {
	return header.IPv4MinimumSize
}

// ParseAddresses implements NetworkProtocol.ParseAddresses.
func (*protocol) ParseAddresses(v buffer.View) (src, dst tcpip.Address) {
	h := header.IPv4(v)
	return h.SourceAddress(), h.DestinationAddress()
}

// SetOption implements NetworkProtocol.SetOption.
func (p *protocol) SetOption(option interface{}) *tcpip.Error {
	return tcpip.ErrUnknownProtocolOption
}

// Option implements NetworkProtocol.Option.
func (p *protocol) Option(option interface{}) *tcpip.Error {
	return tcpip.ErrUnknownProtocolOption
}

// calculateMTU calculates the network-layer payload MTU based on the link-layer
// payload mtu.
func calculateMTU(mtu uint32) uint32 {
	if mtu > MaxTotalSize {
		mtu = MaxTotalSize
	}
	return mtu - header.IPv4MinimumSize
}

// hashRoute calculates a hash value for the given route. It uses the source &
// destination address, the transport protocol number, and a random initial
// value (generated once on initialization) to generate the hash.
func hashRoute(r *stack.Route, protocol tcpip.TransportProtocolNumber) uint32 {
	t := r.LocalAddress
	a := uint32(t[0]) | uint32(t[1])<<8 | uint32(t[2])<<16 | uint32(t[3])<<24
	t = r.RemoteAddress
	b := uint32(t[0]) | uint32(t[1])<<8 | uint32(t[2])<<16 | uint32(t[3])<<24
	return hash.Hash3Words(a, b, uint32(protocol), hashIV)
}

var (
	ids    []uint32
	hashIV uint32
)

func init() {
	ids = make([]uint32, buckets)

	// Randomly initialize hashIV and the ids.
	r := hash.RandN32(1 + buckets)
	for i := range ids {
		ids[i] = r[i]
	}
	hashIV = r[buckets]

	stack.RegisterNetworkProtocolFactory(ProtocolName, func() stack.NetworkProtocol {
		return &protocol{}
	})
}
