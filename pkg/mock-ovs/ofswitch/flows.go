/*
Copyright 2026 NVIDIA

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ofswitch

import (
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
)

const (
	flowModAdd          = 0
	flowModModify       = 1
	flowModModifyStrict = 2
	flowModDelete       = 3
	flowModDeleteStrict = 4

	allTables = 0xff
	// flowModMatchOffset is the offset of ofp_match in OFPT_FLOW_MOD for OpenFlow 1.3 to 1.5.
	flowModMatchOffset = 48
)

// Flow is one stored flow entry. Match and Instructions are kept as raw wire bytes.
type Flow struct {
	Table        uint8
	Priority     uint16
	IdleTimeout  uint16
	HardTimeout  uint16
	Flags        uint16
	Importance   uint16
	Cookie       uint64
	Match        []byte // ofp_match including header and padding
	Instructions []byte
}

type flowKey struct {
	table    uint8
	priority uint16
	match    string
}

func (f *Flow) key() flowKey {
	return flowKey{table: f.Table, priority: f.Priority, match: string(f.Match)}
}

// FlowMod is a parsed OFPT_FLOW_MOD.
type FlowMod struct {
	Flow
	CookieMask uint64
	Command    uint8
}

// matchHasFields reports whether an ofp_match carries any OXM field.
func matchHasFields(m []byte) bool {
	return len(m) >= 4 && binary.BigEndian.Uint16(m[2:4]) > 4
}

// ParseFlowMod parses an OpenFlow 1.3/1.4/1.5 OFPT_FLOW_MOD message, header included.
func ParseFlowMod(msg []byte) (*FlowMod, error) {
	if len(msg) < flowModMatchOffset+4 {
		return nil, fmt.Errorf("flow mod too short: %d bytes", len(msg))
	}
	fm := &FlowMod{}
	fm.Cookie = binary.BigEndian.Uint64(msg[8:])
	fm.CookieMask = binary.BigEndian.Uint64(msg[16:])
	fm.Table = msg[24]
	fm.Command = msg[25]
	fm.IdleTimeout = binary.BigEndian.Uint16(msg[26:])
	fm.HardTimeout = binary.BigEndian.Uint16(msg[28:])
	fm.Priority = binary.BigEndian.Uint16(msg[30:])
	fm.Flags = binary.BigEndian.Uint16(msg[44:])
	if msg[0] >= 5 {
		fm.Importance = binary.BigEndian.Uint16(msg[46:])
	}
	mlen := int(binary.BigEndian.Uint16(msg[flowModMatchOffset+2:]))
	padded := (mlen + 7) / 8 * 8
	if mlen < 4 || flowModMatchOffset+padded > len(msg) {
		return nil, fmt.Errorf("bad match length %d", mlen)
	}
	fm.Match = append([]byte(nil), msg[flowModMatchOffset:flowModMatchOffset+padded]...)
	fm.Instructions = append([]byte(nil), msg[flowModMatchOffset+padded:]...)
	return fm, nil
}

// FlowTable is a cookie-indexed flow store keyed by table, priority and raw match.
type FlowTable struct {
	sync.Mutex
	flows  map[flowKey]*Flow
	meters map[uint32][]byte
}

// NewFlowTable returns an empty table.
func NewFlowTable() *FlowTable {
	return &FlowTable{flows: map[flowKey]*Flow{}, meters: map[uint32][]byte{}}
}

// Apply applies one flow mod.
func (t *FlowTable) Apply(fm *FlowMod) {
	t.Lock()
	defer t.Unlock()
	switch fm.Command {
	case flowModAdd:
		f := fm.Flow
		t.flows[f.key()] = &f
	case flowModModify, flowModModifyStrict:
		for k, f := range t.flows {
			if t.selected(f, fm, fm.Command == flowModModifyStrict) {
				f.Instructions = fm.Instructions
				t.flows[k] = f
			}
		}
	case flowModDelete, flowModDeleteStrict:
		for k, f := range t.flows {
			if t.selected(f, fm, fm.Command == flowModDeleteStrict) {
				delete(t.flows, k)
			}
		}
	}
}

func (t *FlowTable) selected(f *Flow, fm *FlowMod, strict bool) bool {
	if fm.Table != allTables && f.Table != fm.Table {
		return false
	}
	if f.Cookie&fm.CookieMask != fm.Cookie&fm.CookieMask {
		return false
	}
	if strict {
		return f.Priority == fm.Priority && string(f.Match) == string(fm.Match)
	}
	if matchHasFields(fm.Match) {
		return string(f.Match) == string(fm.Match)
	}
	return true
}

// SetMeter stores a meter mod body by meter id; duplicates are accepted.
func (t *FlowTable) SetMeter(id uint32, body []byte) {
	t.Lock()
	defer t.Unlock()
	t.meters[id] = body
}

// Flows returns the flows in table (or all for 0xff) whose cookie matches under mask,
// sorted by table, then priority descending.
func (t *FlowTable) Flows(table uint8, cookie, mask uint64) []Flow {
	t.Lock()
	defer t.Unlock()
	out := make([]Flow, 0, len(t.flows))
	for _, f := range t.flows {
		if table != allTables && f.Table != table {
			continue
		}
		if f.Cookie&mask != cookie&mask {
			continue
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return string(out[i].Match) < string(out[j].Match)
	})
	return out
}

// Len returns the number of stored flows.
func (t *FlowTable) Len() int {
	t.Lock()
	defer t.Unlock()
	return len(t.flows)
}

// ofpStats is the OpenFlow 1.5 ofp_stats OVS itself sends: zero duration,
// idle_time, packet_count and byte_count (length 52, padded to 56). Both OVS
// and Antrea's libOpenflow mis-parse other layouts: OVS rejects an empty
// ofp_stats and libOpenflow always adds padding, even to an aligned length.
var ofpStats = func() []byte {
	b := []byte{0, 0, 0, 52}
	for _, field := range []byte{0, 1, 4, 5} {
		b = append(b, 0x80, 0x02, field<<1, 8, 0, 0, 0, 0, 0, 0, 0, 0)
	}
	return append(b, 0, 0, 0, 0)
}()

// encodeFlowStats encodes one flow for a flow stats reply in the given OpenFlow version:
// ofp_flow_stats for 1.3/1.4, ofp_flow_desc for 1.5.
func encodeFlowStats(version uint8, f *Flow) []byte {
	var b []byte
	if version >= 6 {
		b = make([]byte, 24, 24+len(f.Match)+len(ofpStats)+len(f.Instructions))
		b[4] = f.Table
		binary.BigEndian.PutUint16(b[6:], f.Priority)
		binary.BigEndian.PutUint16(b[8:], f.IdleTimeout)
		binary.BigEndian.PutUint16(b[10:], f.HardTimeout)
		binary.BigEndian.PutUint16(b[12:], f.Flags)
		binary.BigEndian.PutUint16(b[14:], f.Importance)
		binary.BigEndian.PutUint64(b[16:], f.Cookie)
		b = append(b, f.Match...)
		b = append(b, ofpStats...)
	} else {
		b = make([]byte, 48, 48+len(f.Match)+len(f.Instructions))
		b[2] = f.Table
		binary.BigEndian.PutUint16(b[12:], f.Priority)
		binary.BigEndian.PutUint16(b[14:], f.IdleTimeout)
		binary.BigEndian.PutUint16(b[16:], f.HardTimeout)
		binary.BigEndian.PutUint16(b[18:], f.Flags)
		if version == 5 {
			binary.BigEndian.PutUint16(b[20:], f.Importance)
		}
		binary.BigEndian.PutUint64(b[24:], f.Cookie)
		b = append(b, f.Match...)
	}
	instructions := f.Instructions
	if version >= 6 {
		instructions = meterInstructionToAction(instructions)
	}
	b = append(b, instructions...)
	binary.BigEndian.PutUint16(b[0:], uint16(len(b)))
	return b
}

// meterInstructionToAction rewrites the OpenFlow 1.3/1.4 meter instruction
// into the OpenFlow 1.5 meter action, which OVS refuses to decode otherwise.
// The reverse direction is not needed: no client installs 1.5 meter actions.
func meterInstructionToAction(in []byte) []byte {
	const (
		instApplyActions = 4
		instMeter        = 6
		actionMeter      = 29
	)
	var meter, rest []byte
	for off := 0; off+4 <= len(in); {
		typ := binary.BigEndian.Uint16(in[off:])
		l := int(binary.BigEndian.Uint16(in[off+2:]))
		if l < 4 || off+l > len(in) {
			return in
		}
		if typ == instMeter && l == 8 {
			meter = make([]byte, 16)
			binary.BigEndian.PutUint16(meter[0:], instApplyActions)
			binary.BigEndian.PutUint16(meter[2:], 16)
			binary.BigEndian.PutUint16(meter[8:], actionMeter)
			binary.BigEndian.PutUint16(meter[10:], 8)
			copy(meter[12:], in[off+4:off+8])
		} else {
			rest = append(rest, in[off:off+l]...)
		}
		off += l
	}
	if meter == nil {
		return in
	}
	return append(meter, rest...)
}
