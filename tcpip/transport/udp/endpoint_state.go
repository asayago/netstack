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

package udp

import (
	"github.com/asayago/netstack/tcpip"
	"github.com/asayago/netstack/tcpip/buffer"
	"github.com/asayago/netstack/tcpip/header"
	"github.com/asayago/netstack/tcpip/stack"
)

// saveData saves udpPacket.data field.
func (u *udpPacket) saveData() buffer.VectorisedView {
	// We cannot save u.data directly as u.data.views may alias to u.views,
	// which is not allowed by state framework (in-struct pointer).
	return u.data.Clone(nil)
}

// loadData loads udpPacket.data field.
func (u *udpPacket) loadData(data buffer.VectorisedView) {
	// NOTE: We cannot do the u.data = data.Clone(u.views[:]) optimization
	// here because data.views is not guaranteed to be loaded by now. Plus,
	// data.views will be allocated anyway so there really is little point
	// of utilizing u.views for data.views.
	u.data = data
}

// afterLoad is invoked by stateify.
func (e *endpoint) afterLoad() {
	stack.StackFromEnv.RegisterRestoredEndpoint(e)
}

// beforeSave is invoked by stateify.
func (e *endpoint) beforeSave() {
	e.freeze()
}

// Resume implements tcpip.ResumableEndpoint.Resume.
func (e *endpoint) Resume(s *stack.Stack) {
	e.thaw()

	e.mu.Lock()
	defer e.mu.Unlock()

	e.stack = s
	e.ops.InitHandler(e, e.stack, tcpip.GetStackSendBufferLimits, tcpip.GetStackReceiveBufferLimits)

	for m := range e.multicastMemberships {
		if err := e.stack.JoinGroup(e.NetProto, m.nicID, m.multicastAddr); err != nil {
			panic(err)
		}
	}

	state := e.EndpointState()
	if state != StateBound && state != StateConnected {
		return
	}

	netProto := e.effectiveNetProtos[0]
	// Connect() and bindLocked() both assert
	//
	//     netProto == header.IPv6ProtocolNumber
	//
	// before creating a multi-entry effectiveNetProtos.
	if len(e.effectiveNetProtos) > 1 {
		netProto = header.IPv6ProtocolNumber
	}

	var err tcpip.Error
	if state == StateConnected {
		e.route, err = e.stack.FindRoute(e.RegisterNICID, e.ID.LocalAddress, e.ID.RemoteAddress, netProto, e.ops.GetMulticastLoop())
		if err != nil {
			panic(err)
		}
	} else if len(e.ID.LocalAddress) != 0 && !e.isBroadcastOrMulticast(e.RegisterNICID, netProto, e.ID.LocalAddress) { // stateBound
		// A local unicast address is specified, verify that it's valid.
		if e.stack.CheckLocalAddress(e.RegisterNICID, netProto, e.ID.LocalAddress) == 0 {
			panic(&tcpip.ErrBadLocalAddress{})
		}
	}

	// Our saved state had a port, but we don't actually have a
	// reservation. We need to remove the port from our state, but still
	// pass it to the reservation machinery.
	id := e.ID
	e.ID.LocalPort = 0
	e.ID, e.boundBindToDevice, err = e.registerWithStack(e.effectiveNetProtos, id)
	if err != nil {
		panic(err)
	}
}
