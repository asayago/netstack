// automatically generated by stateify.

package buffer

import (
	"github.com/asayago/netstack/state"
)

func (vv *VectorisedView) StateTypeName() string {
	return "pkg/tcpip/buffer.VectorisedView"
}

func (vv *VectorisedView) StateFields() []string {
	return []string{
		"views",
		"size",
	}
}

func (vv *VectorisedView) beforeSave() {}

// +checklocksignore
func (vv *VectorisedView) StateSave(stateSinkObject state.Sink) {
	vv.beforeSave()
	stateSinkObject.Save(0, &vv.views)
	stateSinkObject.Save(1, &vv.size)
}

func (vv *VectorisedView) afterLoad() {}

// +checklocksignore
func (vv *VectorisedView) StateLoad(stateSourceObject state.Source) {
	stateSourceObject.Load(0, &vv.views)
	stateSourceObject.Load(1, &vv.size)
}

func init() {
	state.Register((*VectorisedView)(nil))
}
