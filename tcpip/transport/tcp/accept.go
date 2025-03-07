// Copyright 2018 The gVisor Authors.
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

package tcp

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"sync/atomic"
	"time"

	"github.com/asayago/netstack/sleep"
	"github.com/asayago/netstack/sync"
	"github.com/asayago/netstack/tcpip"
	"github.com/asayago/netstack/tcpip/header"
	"github.com/asayago/netstack/tcpip/ports"
	"github.com/asayago/netstack/tcpip/seqnum"
	"github.com/asayago/netstack/tcpip/stack"
	"github.com/asayago/netstack/waiter"
)

const (
	// tsLen is the length, in bits, of the timestamp in the SYN cookie.
	tsLen = 8

	// tsMask is a mask for timestamp values (i.e., tsLen bits).
	tsMask = (1 << tsLen) - 1

	// tsOffset is the offset, in bits, of the timestamp in the SYN cookie.
	tsOffset = 24

	// hashMask is the mask for hash values (i.e., tsOffset bits).
	hashMask = (1 << tsOffset) - 1

	// maxTSDiff is the maximum allowed difference between a received cookie
	// timestamp and the current timestamp. If the difference is greater
	// than maxTSDiff, the cookie is expired.
	maxTSDiff = 2
)

var (
	// mssTable is a slice containing the possible MSS values that we
	// encode in the SYN cookie with two bits.
	mssTable = []uint16{536, 1300, 1440, 1460}
)

func encodeMSS(mss uint16) uint32 {
	for i := len(mssTable) - 1; i > 0; i-- {
		if mss >= mssTable[i] {
			return uint32(i)
		}
	}
	return 0
}

// listenContext is used by a listening endpoint to store state used while
// listening for connections. This struct is allocated by the listen goroutine
// and must not be accessed or have its methods called concurrently as they
// may mutate the stored objects.
type listenContext struct {
	stack *stack.Stack

	// rcvWnd is the receive window that is sent by this listening context
	// in the initial SYN-ACK.
	rcvWnd seqnum.Size

	// nonce are random bytes that are initialized once when the context
	// is created and used to seed the hash function when generating
	// the SYN cookie.
	nonce [2][sha1.BlockSize]byte

	// listenEP is a reference to the listening endpoint associated with
	// this context. Can be nil if the context is created by the forwarder.
	listenEP *endpoint

	// hasherMu protects hasher.
	hasherMu sync.Mutex
	// hasher is the hash function used to generate a SYN cookie.
	hasher hash.Hash

	// v6Only is true if listenEP is a dual stack socket and has the
	// IPV6_V6ONLY option set.
	v6Only bool

	// netProto indicates the network protocol(IPv4/v6) for the listening
	// endpoint.
	netProto tcpip.NetworkProtocolNumber

	// pendingMu protects pendingEndpoints. This should only be accessed
	// by the listening endpoint's worker goroutine.
	//
	// Lock Ordering: listenEP.workerMu -> pendingMu
	pendingMu sync.Mutex
	// pending is used to wait for all pendingEndpoints to finish when
	// a socket is closed.
	pending sync.WaitGroup
	// pendingEndpoints is a map of all endpoints for which a handshake is
	// in progress.
	pendingEndpoints map[stack.TransportEndpointID]*endpoint
}

// timeStamp returns an 8-bit timestamp with a granularity of 64 seconds.
func timeStamp() uint32 {
	return uint32(time.Now().Unix()>>6) & tsMask
}

// newListenContext creates a new listen context.
func newListenContext(stk *stack.Stack, listenEP *endpoint, rcvWnd seqnum.Size, v6Only bool, netProto tcpip.NetworkProtocolNumber) *listenContext {
	l := &listenContext{
		stack:            stk,
		rcvWnd:           rcvWnd,
		hasher:           sha1.New(),
		v6Only:           v6Only,
		netProto:         netProto,
		listenEP:         listenEP,
		pendingEndpoints: make(map[stack.TransportEndpointID]*endpoint),
	}

	for i := range l.nonce {
		if _, err := io.ReadFull(stk.SecureRNG(), l.nonce[i][:]); err != nil {
			panic(err)
		}
	}

	return l
}

// cookieHash calculates the cookieHash for the given id, timestamp and nonce
// index. The hash is used to create and validate cookies.
func (l *listenContext) cookieHash(id stack.TransportEndpointID, ts uint32, nonceIndex int) uint32 {

	// Initialize block with fixed-size data: local ports and v.
	var payload [8]byte
	binary.BigEndian.PutUint16(payload[0:], id.LocalPort)
	binary.BigEndian.PutUint16(payload[2:], id.RemotePort)
	binary.BigEndian.PutUint32(payload[4:], ts)

	// Feed everything to the hasher.
	l.hasherMu.Lock()
	l.hasher.Reset()

	// Per hash.Hash.Writer:
	//
	// It never returns an error.
	l.hasher.Write(payload[:])
	l.hasher.Write(l.nonce[nonceIndex][:])
	l.hasher.Write([]byte(id.LocalAddress))
	l.hasher.Write([]byte(id.RemoteAddress))

	// Finalize the calculation of the hash and return the first 4 bytes.
	h := l.hasher.Sum(nil)
	l.hasherMu.Unlock()

	return binary.BigEndian.Uint32(h[:])
}

// createCookie creates a SYN cookie for the given id and incoming sequence
// number.
func (l *listenContext) createCookie(id stack.TransportEndpointID, seq seqnum.Value, data uint32) seqnum.Value {
	ts := timeStamp()
	v := l.cookieHash(id, 0, 0) + uint32(seq) + (ts << tsOffset)
	v += (l.cookieHash(id, ts, 1) + data) & hashMask
	return seqnum.Value(v)
}

// isCookieValid checks if the supplied cookie is valid for the given id and
// sequence number. If it is, it also returns the data originally encoded in the
// cookie when createCookie was called.
func (l *listenContext) isCookieValid(id stack.TransportEndpointID, cookie seqnum.Value, seq seqnum.Value) (uint32, bool) {
	ts := timeStamp()
	v := uint32(cookie) - l.cookieHash(id, 0, 0) - uint32(seq)
	cookieTS := v >> tsOffset
	if ((ts - cookieTS) & tsMask) > maxTSDiff {
		return 0, false
	}

	return (v - l.cookieHash(id, cookieTS, 1)) & hashMask, true
}

func (l *listenContext) useSynCookies() bool {
	var alwaysUseSynCookies tcpip.TCPAlwaysUseSynCookies
	if err := l.stack.TransportProtocolOption(header.TCPProtocolNumber, &alwaysUseSynCookies); err != nil {
		panic(fmt.Sprintf("TransportProtocolOption(%d, %T) = %s", header.TCPProtocolNumber, alwaysUseSynCookies, err))
	}
	return bool(alwaysUseSynCookies) || (l.listenEP != nil && l.listenEP.synRcvdBacklogFull())
}

// createConnectingEndpoint creates a new endpoint in a connecting state, with
// the connection parameters given by the arguments.
func (l *listenContext) createConnectingEndpoint(s *segment, rcvdSynOpts *header.TCPSynOptions, queue *waiter.Queue) (*endpoint, tcpip.Error) {
	// Create a new endpoint.
	netProto := l.netProto
	if netProto == 0 {
		netProto = s.netProto
	}

	route, err := l.stack.FindRoute(s.nicID, s.dstAddr, s.srcAddr, s.netProto, false /* multicastLoop */)
	if err != nil {
		return nil, err
	}

	n := newEndpoint(l.stack, netProto, queue)
	n.ops.SetV6Only(l.v6Only)
	n.TransportEndpointInfo.ID = s.id
	n.boundNICID = s.nicID
	n.route = route
	n.effectiveNetProtos = []tcpip.NetworkProtocolNumber{s.netProto}
	n.ops.SetReceiveBufferSize(int64(l.rcvWnd), false /* notify */)
	n.amss = calculateAdvertisedMSS(n.userMSS, n.route)
	n.setEndpointState(StateConnecting)

	n.maybeEnableTimestamp(rcvdSynOpts)
	n.maybeEnableSACKPermitted(rcvdSynOpts)

	n.initGSO()

	// Bootstrap the auto tuning algorithm. Starting at zero will result in
	// a large step function on the first window adjustment causing the
	// window to grow to a really large value.
	n.rcvQueueInfo.RcvAutoParams.PrevCopiedBytes = n.initialReceiveWindow()

	return n, nil
}

// startHandshake creates a new endpoint in connecting state and then sends
// the SYN-ACK for the TCP 3-way handshake. It returns the state of the
// handshake in progress, which includes the new endpoint in the SYN-RCVD
// state.
//
// On success, a handshake h is returned with h.ep.mu held.
//
// Precondition: if l.listenEP != nil, l.listenEP.mu must be locked.
func (l *listenContext) startHandshake(s *segment, opts *header.TCPSynOptions, queue *waiter.Queue, owner tcpip.PacketOwner) (*handshake, tcpip.Error) {
	// Create new endpoint.
	irs := s.sequenceNumber
	isn := generateSecureISN(s.id, l.stack.Seed())
	ep, err := l.createConnectingEndpoint(s, opts, queue)
	if err != nil {
		return nil, err
	}

	// Lock the endpoint before registering to ensure that no out of
	// band changes are possible due to incoming packets etc till
	// the endpoint is done initializing.
	ep.mu.Lock()
	ep.owner = owner

	// listenEP is nil when listenContext is used by tcp.Forwarder.
	deferAccept := time.Duration(0)
	if l.listenEP != nil {
		if l.listenEP.EndpointState() != StateListen {

			// Ensure we release any registrations done by the newly
			// created endpoint.
			ep.mu.Unlock()
			ep.Close()

			return nil, &tcpip.ErrConnectionAborted{}
		}
		l.addPendingEndpoint(ep)

		// Propagate any inheritable options from the listening endpoint
		// to the newly created endpoint.
		l.listenEP.propagateInheritableOptionsLocked(ep)

		if !ep.reserveTupleLocked() {
			ep.mu.Unlock()
			ep.Close()

			l.removePendingEndpoint(ep)

			return nil, &tcpip.ErrConnectionAborted{}
		}

		deferAccept = l.listenEP.deferAccept
	}

	// Register new endpoint so that packets are routed to it.
	if err := ep.stack.RegisterTransportEndpoint(
		ep.effectiveNetProtos,
		ProtocolNumber,
		ep.TransportEndpointInfo.ID,
		ep,
		ep.boundPortFlags,
		ep.boundBindToDevice,
	); err != nil {
		ep.mu.Unlock()
		ep.Close()

		if l.listenEP != nil {
			l.removePendingEndpoint(ep)
		}

		ep.drainClosingSegmentQueue()

		return nil, err
	}

	ep.isRegistered = true

	// Initialize and start the handshake.
	h := ep.newPassiveHandshake(isn, irs, opts, deferAccept)
	h.listenEP = l.listenEP
	h.start()
	return h, nil
}

// performHandshake performs a TCP 3-way handshake. On success, the new
// established endpoint is returned with e.mu held.
//
// Precondition: if l.listenEP != nil, l.listenEP.mu must be locked.
func (l *listenContext) performHandshake(s *segment, opts *header.TCPSynOptions, queue *waiter.Queue, owner tcpip.PacketOwner) (*endpoint, tcpip.Error) {
	h, err := l.startHandshake(s, opts, queue, owner)
	if err != nil {
		return nil, err
	}
	ep := h.ep

	if err := h.complete(); err != nil {
		ep.stack.Stats().TCP.FailedConnectionAttempts.Increment()
		ep.stats.FailedConnectionAttempts.Increment()
		l.cleanupFailedHandshake(h)
		return nil, err
	}
	l.cleanupCompletedHandshake(h)
	return ep, nil
}

func (l *listenContext) addPendingEndpoint(n *endpoint) {
	l.pendingMu.Lock()
	l.pendingEndpoints[n.TransportEndpointInfo.ID] = n
	l.pending.Add(1)
	l.pendingMu.Unlock()
}

func (l *listenContext) removePendingEndpoint(n *endpoint) {
	l.pendingMu.Lock()
	delete(l.pendingEndpoints, n.TransportEndpointInfo.ID)
	l.pending.Done()
	l.pendingMu.Unlock()
}

func (l *listenContext) closeAllPendingEndpoints() {
	l.pendingMu.Lock()
	for _, n := range l.pendingEndpoints {
		n.notifyProtocolGoroutine(notifyClose)
	}
	l.pendingMu.Unlock()
	l.pending.Wait()
}

// Precondition: h.ep.mu must be held.
func (l *listenContext) cleanupFailedHandshake(h *handshake) {
	e := h.ep
	e.mu.Unlock()
	e.Close()
	e.notifyAborted()
	if l.listenEP != nil {
		l.removePendingEndpoint(e)
	}
	e.drainClosingSegmentQueue()
	e.h = nil
}

// cleanupCompletedHandshake transfers any state from the completed handshake to
// the new endpoint.
//
// Precondition: h.ep.mu must be held.
func (l *listenContext) cleanupCompletedHandshake(h *handshake) {
	e := h.ep
	if l.listenEP != nil {
		l.removePendingEndpoint(e)
	}
	e.isConnectNotified = true

	// Update the receive window scaling. We can't do it before the
	// handshake because it's possible that the peer doesn't support window
	// scaling.
	e.rcv.RcvWndScale = e.h.effectiveRcvWndScale()

	// Clean up handshake state stored in the endpoint so that it can be GCed.
	e.h = nil
}

// deliverAccepted delivers the newly-accepted endpoint to the listener. If the
// listener has transitioned out of the listen state (accepted is the zero
// value), the new endpoint is reset instead.
func (e *endpoint) deliverAccepted(n *endpoint, withSynCookie bool) {
	e.mu.Lock()
	e.pendingAccepted.Add(1)
	e.mu.Unlock()
	defer e.pendingAccepted.Done()

	// Drop the lock before notifying to avoid deadlock in user-specified
	// callbacks.
	delivered := func() bool {
		e.acceptMu.Lock()
		defer e.acceptMu.Unlock()
		for {
			if e.accepted == (accepted{}) {
				return false
			}
			if e.accepted.endpoints.Len() == e.accepted.cap {
				e.acceptCond.Wait()
				continue
			}

			e.accepted.endpoints.PushBack(n)
			if !withSynCookie {
				atomic.AddInt32(&e.synRcvdCount, -1)
			}
			return true
		}
	}()
	if delivered {
		e.waiterQueue.Notify(waiter.ReadableEvents)
	} else {
		n.notifyProtocolGoroutine(notifyReset)
	}
}

// propagateInheritableOptionsLocked propagates any options set on the listening
// endpoint to the newly created endpoint.
//
// Precondition: e.mu and n.mu must be held.
func (e *endpoint) propagateInheritableOptionsLocked(n *endpoint) {
	n.userTimeout = e.userTimeout
	n.portFlags = e.portFlags
	n.boundBindToDevice = e.boundBindToDevice
	n.boundPortFlags = e.boundPortFlags
	n.userMSS = e.userMSS
}

// reserveTupleLocked reserves an accepted endpoint's tuple.
//
// Preconditions:
// * propagateInheritableOptionsLocked has been called.
// * e.mu is held.
func (e *endpoint) reserveTupleLocked() bool {
	dest := tcpip.FullAddress{
		Addr: e.TransportEndpointInfo.ID.RemoteAddress,
		Port: e.TransportEndpointInfo.ID.RemotePort,
	}
	portRes := ports.Reservation{
		Networks:     e.effectiveNetProtos,
		Transport:    ProtocolNumber,
		Addr:         e.TransportEndpointInfo.ID.LocalAddress,
		Port:         e.TransportEndpointInfo.ID.LocalPort,
		Flags:        e.boundPortFlags,
		BindToDevice: e.boundBindToDevice,
		Dest:         dest,
	}
	if !e.stack.ReserveTuple(portRes) {
		e.stack.Stats().TCP.FailedPortReservations.Increment()
		return false
	}

	e.isPortReserved = true
	e.boundDest = dest
	return true
}

// notifyAborted wakes up any waiters on registered, but not accepted
// endpoints.
//
// This is strictly not required normally as a socket that was never accepted
// can't really have any registered waiters except when stack.Wait() is called
// which waits for all registered endpoints to stop and expects an EventHUp.
func (e *endpoint) notifyAborted() {
	e.waiterQueue.Notify(waiter.EventHUp | waiter.EventErr | waiter.ReadableEvents | waiter.WritableEvents)
}

// handleSynSegment is called in its own goroutine once the listening endpoint
// receives a SYN segment. It is responsible for completing the handshake and
// queueing the new endpoint for acceptance.
//
// A limited number of these goroutines are allowed before TCP starts using SYN
// cookies to accept connections.
//
// Precondition: if ctx.listenEP != nil, ctx.listenEP.mu must be locked.
func (e *endpoint) handleSynSegment(ctx *listenContext, s *segment, opts *header.TCPSynOptions) tcpip.Error {
	defer s.decRef()

	h, err := ctx.startHandshake(s, opts, &waiter.Queue{}, e.owner)
	if err != nil {
		e.stack.Stats().TCP.FailedConnectionAttempts.Increment()
		e.stats.FailedConnectionAttempts.Increment()
		atomic.AddInt32(&e.synRcvdCount, -1)
		return err
	}

	go func() {
		if err := h.complete(); err != nil {
			e.stack.Stats().TCP.FailedConnectionAttempts.Increment()
			e.stats.FailedConnectionAttempts.Increment()
			ctx.cleanupFailedHandshake(h)
			atomic.AddInt32(&e.synRcvdCount, -1)
			return
		}
		ctx.cleanupCompletedHandshake(h)
		h.ep.startAcceptedLoop()
		e.stack.Stats().TCP.PassiveConnectionOpenings.Increment()
		e.deliverAccepted(h.ep, false /*withSynCookie*/)
	}()

	return nil
}

func (e *endpoint) synRcvdBacklogFull() bool {
	e.acceptMu.Lock()
	acceptedCap := e.accepted.cap
	e.acceptMu.Unlock()
	// The capacity of the accepted queue would always be one greater than the
	// listen backlog. But, the SYNRCVD connections count is always checked
	// against the listen backlog value for Linux parity reason.
	// https://github.com/torvalds/linux/blob/7acac4b3196/include/net/inet_connection_sock.h#L280
	//
	// We maintain an equality check here as the synRcvdCount is incremented
	// and compared only from a single listener context and the capacity of
	// the accepted queue can only increase by a new listen call.
	return int(atomic.LoadInt32(&e.synRcvdCount)) == acceptedCap-1
}

func (e *endpoint) acceptQueueIsFull() bool {
	e.acceptMu.Lock()
	full := e.accepted != (accepted{}) && e.accepted.endpoints.Len() == e.accepted.cap
	e.acceptMu.Unlock()
	return full
}

// handleListenSegment is called when a listening endpoint receives a segment
// and needs to handle it.
//
// Precondition: if ctx.listenEP != nil, ctx.listenEP.mu must be locked.
func (e *endpoint) handleListenSegment(ctx *listenContext, s *segment) tcpip.Error {
	e.rcvQueueInfo.rcvQueueMu.Lock()
	rcvClosed := e.rcvQueueInfo.RcvClosed
	e.rcvQueueInfo.rcvQueueMu.Unlock()
	if rcvClosed || s.flagsAreSet(header.TCPFlagSyn|header.TCPFlagAck) {
		// If the endpoint is shutdown, reply with reset.
		//
		// RFC 793 section 3.4 page 35 (figure 12) outlines that a RST
		// must be sent in response to a SYN-ACK while in the listen
		// state to prevent completing a handshake from an old SYN.
		return replyWithReset(e.stack, s, e.sendTOS, e.ttl)
	}

	switch {
	case s.flags == header.TCPFlagSyn:
		if e.acceptQueueIsFull() {
			e.stack.Stats().TCP.ListenOverflowSynDrop.Increment()
			e.stats.ReceiveErrors.ListenOverflowSynDrop.Increment()
			e.stack.Stats().DroppedPackets.Increment()
			return nil
		}

		opts := parseSynSegmentOptions(s)
		if !ctx.useSynCookies() {
			s.incRef()
			atomic.AddInt32(&e.synRcvdCount, 1)
			return e.handleSynSegment(ctx, s, &opts)
		}
		route, err := e.stack.FindRoute(s.nicID, s.dstAddr, s.srcAddr, s.netProto, false /* multicastLoop */)
		if err != nil {
			return err
		}
		defer route.Release()

		// Send SYN without window scaling because we currently
		// don't encode this information in the cookie.
		//
		// Enable Timestamp option if the original syn did have
		// the timestamp option specified.
		//
		// Use the user supplied MSS on the listening socket for
		// new connections, if available.
		synOpts := header.TCPSynOptions{
			WS:    -1,
			TS:    opts.TS,
			TSVal: tcpTimeStamp(time.Now(), timeStampOffset()),
			TSEcr: opts.TSVal,
			MSS:   calculateAdvertisedMSS(e.userMSS, route),
		}
		cookie := ctx.createCookie(s.id, s.sequenceNumber, encodeMSS(opts.MSS))
		fields := tcpFields{
			id:     s.id,
			ttl:    e.ttl,
			tos:    e.sendTOS,
			flags:  header.TCPFlagSyn | header.TCPFlagAck,
			seq:    cookie,
			ack:    s.sequenceNumber + 1,
			rcvWnd: ctx.rcvWnd,
		}
		if err := e.sendSynTCP(route, fields, synOpts); err != nil {
			return err
		}
		e.stack.Stats().TCP.ListenOverflowSynCookieSent.Increment()
		return nil

	case (s.flags & header.TCPFlagAck) != 0:
		if e.acceptQueueIsFull() {
			// Silently drop the ack as the application can't accept
			// the connection at this point. The ack will be
			// retransmitted by the sender anyway and we can
			// complete the connection at the time of retransmit if
			// the backlog has space.
			e.stack.Stats().TCP.ListenOverflowAckDrop.Increment()
			e.stats.ReceiveErrors.ListenOverflowAckDrop.Increment()
			e.stack.Stats().DroppedPackets.Increment()
			return nil
		}

		iss := s.ackNumber - 1
		irs := s.sequenceNumber - 1

		// Since SYN cookies are in use this is potentially an ACK to a
		// SYN-ACK we sent but don't have a half open connection state
		// as cookies are being used to protect against a potential SYN
		// flood. In such cases validate the cookie and if valid create
		// a fully connected endpoint and deliver to the accept queue.
		//
		// If not, silently drop the ACK to avoid leaking information
		// when under a potential syn flood attack.
		//
		// Validate the cookie.
		data, ok := ctx.isCookieValid(s.id, iss, irs)
		if !ok || int(data) >= len(mssTable) {
			e.stack.Stats().TCP.ListenOverflowInvalidSynCookieRcvd.Increment()
			e.stack.Stats().DroppedPackets.Increment()

			// When not using SYN cookies, as per RFC 793, section 3.9, page 64:
			// Any acknowledgment is bad if it arrives on a connection still in
			// the LISTEN state.  An acceptable reset segment should be formed
			// for any arriving ACK-bearing segment.  The RST should be
			// formatted as follows:
			//
			//  <SEQ=SEG.ACK><CTL=RST>
			//
			// Send a reset as this is an ACK for which there is no
			// half open connections and we are not using cookies
			// yet.
			//
			// The only time we should reach here when a connection
			// was opened and closed really quickly and a delayed
			// ACK was received from the sender.
			return replyWithReset(e.stack, s, e.sendTOS, e.ttl)
		}
		e.stack.Stats().TCP.ListenOverflowSynCookieRcvd.Increment()
		// Create newly accepted endpoint and deliver it.
		rcvdSynOptions := &header.TCPSynOptions{
			MSS: mssTable[data],
			// Disable Window scaling as original SYN is
			// lost.
			WS: -1,
		}

		// When syn cookies are in use we enable timestamp only
		// if the ack specifies the timestamp option assuming
		// that the other end did in fact negotiate the
		// timestamp option in the original SYN.
		if s.parsedOptions.TS {
			rcvdSynOptions.TS = true
			rcvdSynOptions.TSVal = s.parsedOptions.TSVal
			rcvdSynOptions.TSEcr = s.parsedOptions.TSEcr
		}

		n, err := ctx.createConnectingEndpoint(s, rcvdSynOptions, &waiter.Queue{})
		if err != nil {
			return err
		}

		n.mu.Lock()

		// Propagate any inheritable options from the listening endpoint
		// to the newly created endpoint.
		e.propagateInheritableOptionsLocked(n)

		if !n.reserveTupleLocked() {
			n.mu.Unlock()
			n.Close()

			e.stack.Stats().TCP.FailedConnectionAttempts.Increment()
			e.stats.FailedConnectionAttempts.Increment()
			return nil
		}

		// Register new endpoint so that packets are routed to it.
		if err := n.stack.RegisterTransportEndpoint(
			n.effectiveNetProtos,
			ProtocolNumber,
			n.TransportEndpointInfo.ID,
			n,
			n.boundPortFlags,
			n.boundBindToDevice,
		); err != nil {
			n.mu.Unlock()
			n.Close()

			e.stack.Stats().TCP.FailedConnectionAttempts.Increment()
			e.stats.FailedConnectionAttempts.Increment()
			return err
		}

		n.isRegistered = true

		// clear the tsOffset for the newly created
		// endpoint as the Timestamp was already
		// randomly offset when the original SYN-ACK was
		// sent above.
		n.TSOffset = 0

		// Switch state to connected.
		n.isConnectNotified = true
		n.transitionToStateEstablishedLocked(&handshake{
			ep:          n,
			iss:         iss,
			ackNum:      irs + 1,
			rcvWnd:      seqnum.Size(n.initialReceiveWindow()),
			sndWnd:      s.window,
			rcvWndScale: e.rcvWndScaleForHandshake(),
			sndWndScale: rcvdSynOptions.WS,
			mss:         rcvdSynOptions.MSS,
		})

		// Do the delivery in a separate goroutine so
		// that we don't block the listen loop in case
		// the application is slow to accept or stops
		// accepting.
		//
		// NOTE: This won't result in an unbounded
		// number of goroutines as we do check before
		// entering here that there was at least some
		// space available in the backlog.

		// Start the protocol goroutine.
		n.startAcceptedLoop()
		e.stack.Stats().TCP.PassiveConnectionOpenings.Increment()
		go e.deliverAccepted(n, true /*withSynCookie*/)
		return nil

	default:
		return nil
	}
}

// protocolListenLoop is the main loop of a listening TCP endpoint. It runs in
// its own goroutine and is responsible for handling connection requests.
func (e *endpoint) protocolListenLoop(rcvWnd seqnum.Size) {
	e.mu.Lock()
	v6Only := e.ops.GetV6Only()
	ctx := newListenContext(e.stack, e, rcvWnd, v6Only, e.NetProto)

	defer func() {
		// Mark endpoint as closed. This will prevent goroutines running
		// handleSynSegment() from attempting to queue new connections
		// to the endpoint.
		e.setEndpointState(StateClose)

		// Close any endpoints in SYN-RCVD state.
		ctx.closeAllPendingEndpoints()

		// Do cleanup if needed.
		e.completeWorkerLocked()

		if e.drainDone != nil {
			close(e.drainDone)
		}
		e.mu.Unlock()

		e.drainClosingSegmentQueue()

		// Notify waiters that the endpoint is shutdown.
		e.waiterQueue.Notify(waiter.ReadableEvents | waiter.WritableEvents | waiter.EventHUp | waiter.EventErr)
	}()

	var s sleep.Sleeper
	s.AddWaker(&e.notificationWaker, wakerForNotification)
	s.AddWaker(&e.newSegmentWaker, wakerForNewSegment)
	for {
		e.mu.Unlock()
		index, _ := s.Fetch(true)
		e.mu.Lock()
		switch index {
		case wakerForNotification:
			n := e.fetchNotifications()
			if n&notifyClose != 0 {
				return
			}
			if n&notifyDrain != 0 {
				for !e.segmentQueue.empty() {
					s := e.segmentQueue.dequeue()
					// TODO(gvisor.dev/issue/4690): Better handle errors instead of
					// silently dropping.
					_ = e.handleListenSegment(ctx, s)
					s.decRef()
				}
				close(e.drainDone)
				e.mu.Unlock()
				<-e.undrain
				e.mu.Lock()
			}

		case wakerForNewSegment:
			// Process at most maxSegmentsPerWake segments.
			mayRequeue := true
			for i := 0; i < maxSegmentsPerWake; i++ {
				s := e.segmentQueue.dequeue()
				if s == nil {
					mayRequeue = false
					break
				}

				// TODO(gvisor.dev/issue/4690): Better handle errors instead of
				// silently dropping.
				_ = e.handleListenSegment(ctx, s)
				s.decRef()
			}

			// If the queue is not empty, make sure we'll wake up
			// in the next iteration.
			if mayRequeue && !e.segmentQueue.empty() {
				e.newSegmentWaker.Assert()
			}
		}
	}
}
