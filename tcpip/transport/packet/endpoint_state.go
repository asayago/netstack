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

package packet

import (
	"github.com/asayago/netstack/tcpip"
	"github.com/asayago/netstack/tcpip/buffer"
	"github.com/asayago/netstack/tcpip/stack"
)

// saveData saves packet.data field.
func (p *packet) saveData() buffer.VectorisedView {
	// We cannot save p.data directly as p.data.views may alias to p.views,
	// which is not allowed by state framework (in-struct pointer).
	return p.data.Clone(nil)
}

// loadData loads packet.data field.
func (p *packet) loadData(data buffer.VectorisedView) {
	// NOTE: We cannot do the p.data = data.Clone(p.views[:]) optimization
	// here because data.views is not guaranteed to be loaded by now. Plus,
	// data.views will be allocated anyway so there really is little point
	// of utilizing p.views for data.views.
	p.data = data
}

// beforeSave is invoked by stateify.
func (ep *endpoint) beforeSave() {
	ep.freeze()
}

// afterLoad is invoked by stateify.
func (ep *endpoint) afterLoad() {
	ep.thaw()
	ep.stack = stack.StackFromEnv
	ep.ops.InitHandler(ep, ep.stack, tcpip.GetStackSendBufferLimits, tcpip.GetStackReceiveBufferLimits)

	// TODO(gvisor.dev/173): Once bind is supported, choose the right NIC.
	if err := ep.stack.RegisterPacketEndpoint(0, ep.netProto, ep); err != nil {
		panic(err)
	}
}
