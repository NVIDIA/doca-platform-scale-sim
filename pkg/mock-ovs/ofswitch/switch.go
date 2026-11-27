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

// Package ofswitch is a control-plane-only OpenFlow 1.3/1.4/1.5 switch that
// listens on <run-dir>/<bridge>.mgmt, keeps a flow table and answers the
// Antrea OpenFlow client and ovs-ofctl.
package ofswitch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"sync"

	"github.com/go-logr/logr"
)

// OpenFlow message types, multipart types, bundle operations, error codes and
// header limits shared by OpenFlow 1.3, 1.4 and 1.5. Validate them against the
// switch specifications:
//   - OpenFlow Switch Specification 1.3.5: https://opennetworking.org/wp-content/uploads/2014/10/openflow-switch-v1.3.5.pdf
//   - OpenFlow Switch Specification 1.4.1: https://opennetworking.org/wp-content/uploads/2014/10/openflow-switch-v1.4.1.pdf
//   - OpenFlow Switch Specification 1.5.1: https://opennetworking.org/wp-content/uploads/2014/10/openflow-switch-v1.5.1.pdf
const (
	typeHello            = 0
	typeError            = 1
	typeEchoRequest      = 2
	typeEchoReply        = 3
	typeExperimenter     = 4
	typeFeaturesRequest  = 5
	typeFeaturesReply    = 6
	typeGetConfigRequest = 7
	typeGetConfigReply   = 8
	typeSetConfig        = 9
	typePacketOut        = 13
	typeFlowMod          = 14
	typeGroupMod         = 15
	typePortMod          = 16
	typeTableMod         = 17
	typeMultipartRequest = 18
	typeMultipartReply   = 19
	typeBarrierRequest   = 20
	typeBarrierReply     = 21
	typeRoleRequest      = 24
	typeRoleReply        = 25
	typeSetAsync         = 28
	typeMeterMod         = 29
	typeBundleControl    = 33
	typeBundleAdd        = 34

	mpDesc          = 0
	mpFlow          = 1
	mpAggregate     = 2
	mpMeterFeatures = 11
	mpTableFeatures = 12

	bundleOpenRequest   = 0
	bundleCommitRequest = 4

	errBadRequest = 1
	errBadType    = 1

	headerLen = 8
	maxMsgLen = 0xffff
	// helloBitmap is the OFPHET_VERSIONBITMAP payload sent in the hello. Bit n
	// is set when the switch supports the OpenFlow version whose wire number is
	// n: 4 is 1.3, 5 is 1.4 and 6 is 1.5. latestVersion, the highest of these,
	// is also the version in the hello's own header.
	helloBitmap   = 1<<4 | 1<<5 | 1<<6
	latestVersion = 6
	numTables     = 254
)

// Switch serves one bridge's management socket.
type Switch struct {
	Bridge string
	Path   string
	Flows  *FlowTable
	dpid   uint64
	log    logr.Logger

	ln net.Listener
	sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// NewSwitch listens on path for bridge. Flows survive reconnects of clients.
func NewSwitch(bridge, path string, log logr.Logger) (*Switch, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(bridge))
	s := &Switch{
		Bridge: bridge,
		Path:   path,
		Flows:  NewFlowTable(),
		dpid:   h.Sum64() & 0x0000ffffffffffff,
		log:    log.WithValues("bridge", bridge),
		ln:     ln,
		conns:  map[net.Conn]struct{}{},
	}
	s.wg.Go(s.accept)
	return s, nil
}

// Close stops listening, drops clients and removes the socket.
func (s *Switch) Close() {
	_ = s.ln.Close()
	s.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.Unlock()
	s.wg.Wait()
	_ = os.Remove(s.Path)
}

func (s *Switch) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.Lock()
		s.conns[c] = struct{}{}
		s.Unlock()
		s.wg.Go(func() {
			defer func() {
				s.Lock()
				delete(s.conns, c)
				s.Unlock()
				_ = c.Close()
			}()
			if err := s.serve(c); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.log.V(1).Info("openflow connection closed", "err", err.Error())
			}
		})
	}
}

type conn struct {
	net.Conn
	sync.Mutex
	bundles map[uint32][][]byte
}

func (c *conn) send(version, typ uint8, xid uint32, body []byte) error {
	msg := make([]byte, headerLen+len(body))
	msg[0] = version
	msg[1] = typ
	binary.BigEndian.PutUint16(msg[2:], uint16(len(msg)))
	binary.BigEndian.PutUint32(msg[4:], xid)
	copy(msg[headerLen:], body)
	c.Lock()
	defer c.Unlock()
	_, err := c.Write(msg)
	return err
}

func readMsg(r io.Reader) ([]byte, error) {
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[2:]))
	if n < headerLen {
		return nil, fmt.Errorf("bad openflow length %d", n)
	}
	msg := make([]byte, n)
	copy(msg, hdr)
	if _, err := io.ReadFull(r, msg[headerLen:]); err != nil {
		return nil, err
	}
	return msg, nil
}

// serve runs one client connection until it fails. It sends the hello first,
// advertising OpenFlow 1.3, 1.4 and 1.5 in the version bitmap, then reads and
// handles messages one at a time, so replies go out in request order. It
// returns when a read fails, which includes the client hanging up, or when
// handle returns an error, such as a failed write. The caller closes the
// connection either way.
func (s *Switch) serve(nc net.Conn) error {
	c := &conn{Conn: nc, bundles: map[uint32][][]byte{}}
	hello := make([]byte, 8)
	binary.BigEndian.PutUint16(hello[0:], 1) // OFPHET_VERSIONBITMAP
	binary.BigEndian.PutUint16(hello[2:], 8)
	binary.BigEndian.PutUint32(hello[4:], helloBitmap)
	if err := c.send(latestVersion, typeHello, 0, hello); err != nil {
		return err
	}
	for {
		msg, err := readMsg(c)
		if err != nil {
			return err
		}
		if err := s.handle(c, msg); err != nil {
			return err
		}
	}
}

// handle answers one complete message from readMsg. Replies reuse the request's
// version and xid, which is how the client matches them up. Messages the switch
// accepts without an answer, such as hello, errors from the client, and the
// set-config, group-mod, port-mod and table-mod messages it ignores, return
// nil. Unknown types get an OFPET_BAD_REQUEST/OFPBRC_BAD_TYPE error that echoes
// up to 64 bytes of the request, and the connection stays open. A returned
// error is a write failure, and it closes the connection. Flow and meter mods
// are applied to the flow table by apply, and multipart requests and bundles
// are handled by their own functions.
func (s *Switch) handle(c *conn, msg []byte) error {
	version, typ := msg[0], msg[1]
	xid := binary.BigEndian.Uint32(msg[4:])
	switch typ {
	case typeHello, typeError, typeEchoReply, typeExperimenter, typeSetConfig, typeSetAsync,
		typePacketOut, typeGroupMod, typePortMod, typeTableMod:
		return nil
	case typeEchoRequest:
		return c.send(version, typeEchoReply, xid, msg[headerLen:])
	case typeFeaturesRequest:
		body := make([]byte, 24)
		binary.BigEndian.PutUint64(body[0:], s.dpid)
		body[12] = numTables
		binary.BigEndian.PutUint32(body[16:], 0x4f)
		return c.send(version, typeFeaturesReply, xid, body)
	case typeGetConfigRequest:
		return c.send(version, typeGetConfigReply, xid, []byte{0, 0, 0xff, 0xff})
	case typeBarrierRequest:
		return c.send(version, typeBarrierReply, xid, nil)
	case typeRoleRequest:
		return c.send(version, typeRoleReply, xid, msg[headerLen:])
	case typeFlowMod, typeMeterMod:
		return s.apply(c, msg)
	case typeMultipartRequest:
		return s.multipart(c, msg)
	case typeBundleControl:
		return s.bundleControl(c, msg)
	case typeBundleAdd:
		if len(msg) < 16+headerLen {
			return s.sendError(c, msg)
		}
		id := binary.BigEndian.Uint32(msg[8:])
		c.bundles[id] = append(c.bundles[id], append([]byte(nil), msg[16:]...))
		return nil
	default:
		return s.sendError(c, msg)
	}
}

func (s *Switch) sendError(c *conn, msg []byte) error {
	data := msg
	if len(data) > 64 {
		data = data[:64]
	}
	body := make([]byte, 4, 4+len(data))
	binary.BigEndian.PutUint16(body[0:], errBadRequest)
	binary.BigEndian.PutUint16(body[2:], errBadType)
	body = append(body, data...)
	return c.send(msg[0], typeError, binary.BigEndian.Uint32(msg[4:]), body)
}

func (s *Switch) apply(c *conn, msg []byte) error {
	switch msg[1] {
	case typeFlowMod:
		fm, err := ParseFlowMod(msg)
		if err != nil {
			return s.sendError(c, msg)
		}
		s.Flows.Apply(fm)
	case typeMeterMod:
		if len(msg) >= 16 {
			s.Flows.SetMeter(binary.BigEndian.Uint32(msg[12:]), msg[headerLen:])
		}
	}
	return nil
}

func (s *Switch) bundleControl(c *conn, msg []byte) error {
	if len(msg) < 16 {
		return s.sendError(c, msg)
	}
	id := binary.BigEndian.Uint32(msg[8:])
	ctl := binary.BigEndian.Uint16(msg[12:])
	switch ctl {
	case bundleOpenRequest:
		c.bundles[id] = nil
	case bundleCommitRequest:
		for _, inner := range c.bundles[id] {
			if len(inner) >= headerLen {
				_ = s.apply(c, inner)
			}
		}
		delete(c.bundles, id)
	default:
		delete(c.bundles, id)
	}
	reply := append([]byte(nil), msg[headerLen:16]...)
	binary.BigEndian.PutUint16(reply[4:], ctl+1)
	return c.send(msg[0], typeBundleControl, binary.BigEndian.Uint32(msg[4:]), reply)
}

func (s *Switch) multipart(c *conn, msg []byte) error {
	if len(msg) < 16 {
		return s.sendError(c, msg)
	}
	version := msg[0]
	xid := binary.BigEndian.Uint32(msg[4:])
	mpType := binary.BigEndian.Uint16(msg[8:])
	var entries [][]byte
	switch mpType {
	case mpFlow:
		table, cookie, mask := uint8(allTables), uint64(0), uint64(0)
		if len(msg) >= 16+32 {
			table = msg[16]
			cookie = binary.BigEndian.Uint64(msg[32:])
			mask = binary.BigEndian.Uint64(msg[40:])
		}
		for _, f := range s.Flows.Flows(table, cookie, mask) {
			entries = append(entries, encodeFlowStats(version, &f))
		}
	case mpDesc:
		desc := make([]byte, 1056)
		copy(desc[0:], "NVIDIA")
		copy(desc[256:], "mock-ovs")
		copy(desc[512:], "3.3.0-mock")
		copy(desc[768:], "None")
		copy(desc[800:], "None")
		entries = [][]byte{desc}
	case mpAggregate:
		agg := make([]byte, 24)
		binary.BigEndian.PutUint32(agg[16:], uint32(s.Flows.Len()))
		entries = [][]byte{agg}
	case mpTableFeatures:
		// ovs-ofctl asks for table features to resolve table names before add-flows.
		for t := 0; t < numTables; t++ {
			// 64-byte header plus the eight properties OVS requires, all empty:
			// instructions, next_tables, write/apply_actions, match,
			// wildcards and write/apply_setfield.
			tf := make([]byte, 64+8*8)
			binary.BigEndian.PutUint16(tf[0:], uint16(len(tf)))
			for i := 0; i < 8; i++ {
				binary.BigEndian.PutUint16(tf[64+8*i:], uint16(2*i))
				binary.BigEndian.PutUint16(tf[64+8*i+2:], 4)
			}
			tf[2] = uint8(t)
			binary.BigEndian.PutUint64(tf[40:], 0xffffffffffffffff)
			binary.BigEndian.PutUint64(tf[48:], 0xffffffffffffffff)
			binary.BigEndian.PutUint32(tf[60:], 1000000)
			entries = append(entries, tf)
		}
	case mpMeterFeatures:
		mf := make([]byte, 16)
		binary.BigEndian.PutUint32(mf[0:], 0xffffffff)
		binary.BigEndian.PutUint32(mf[4:], 0x3)
		binary.BigEndian.PutUint32(mf[8:], 0x1)
		entries = [][]byte{mf}
	}
	return sendMultipart(c, version, xid, mpType, entries)
}

// sendMultipart splits entries over as many replies as needed, setting OFPMPF_REPLY_MORE
// on all but the last.
func sendMultipart(c *conn, version uint8, xid uint32, mpType uint16, entries [][]byte) error {
	const limit = maxMsgLen - headerLen - 8
	for {
		body := make([]byte, 8, 1024)
		binary.BigEndian.PutUint16(body[0:], mpType)
		i := 0
		for i < len(entries) && len(body)-8+len(entries[i]) <= limit {
			body = append(body, entries[i]...)
			i++
		}
		if i == 0 && len(entries) > 0 {
			return fmt.Errorf("multipart entry too large: %d bytes", len(entries[0]))
		}
		entries = entries[i:]
		if len(entries) > 0 {
			binary.BigEndian.PutUint16(body[2:], 1)
		}
		if err := c.send(version, typeMultipartReply, xid, body); err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
	}
}
