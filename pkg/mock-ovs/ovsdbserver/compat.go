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

package ovsdbserver

import (
	"encoding/json"
	"net"
	"sort"
	"sync"
)

// proxy relays one client connection to the libovsdb server and rewrites client
// requests that libovsdb cannot decode.
//
// libovsdb rejects the "comment" operation that every ovs-vsctl transaction
// carries, and its "wait" compares set columns order-sensitively, so the
// optimistic-concurrency waits the OVS IDL prepends (until "==", timeout 0)
// fail on any multi-element set. Both are replaced by a select that matches no
// row, which keeps the result array aligned with the operations. Waits with
// other forms, such as ovsutils' ofport_request guard, are kept.
//
// RFC 7047 lets a monitor request map a table to one <monitor-request> or to an
// array of them; libovsdb v0.7.0 only accepts the single form, while ovs-vsctl
// and ovsdb-client always send arrays. Arrays are merged into one request
// (union of columns, no where clause), which can only widen what is monitored,
// and a missing "select" is filled in.
//
// In the other direction libovsdb sends updates as requests named "update2":
// a monitor_cond_since update (three params) is renamed "update3", a plain
// monitor update (rows with "old"/"new") is renamed "update", and all are
// delivered as notifications, as RFC 7047 and the OVS IDL expect.
func proxy(client net.Conn, backend string) {
	defer func() { _ = client.Close() }()
	srv, err := net.Dial("unix", backend)
	if err != nil {
		return
	}
	defer func() { _ = srv.Close() }()

	var mu sync.Mutex
	toServer := json.NewEncoder(srv)
	send := func(msg any) error {
		mu.Lock()
		defer mu.Unlock()
		return toServer.Encode(msg)
	}

	go func() {
		defer func() { _ = client.Close() }()
		dec := json.NewDecoder(srv)
		enc := json.NewEncoder(client)
		for {
			var msg map[string]json.RawMessage
			if err := dec.Decode(&msg); err != nil {
				return
			}
			if id, ok := rewriteUpdate(msg); ok {
				if err := send(map[string]any{"id": id, "result": []any{}, "error": nil}); err != nil {
					return
				}
			}
			if err := enc.Encode(msg); err != nil {
				return
			}
		}
	}()

	dec := json.NewDecoder(client)
	for {
		var msg map[string]json.RawMessage
		if err := dec.Decode(&msg); err != nil {
			return
		}
		rewriteMonitor(msg)
		rewriteTransact(msg)
		if err := send(msg); err != nil {
			return
		}
	}
}

func rewriteMonitor(msg map[string]json.RawMessage) {
	var method string
	if err := json.Unmarshal(msg["method"], &method); err != nil {
		return
	}
	switch method {
	case "monitor", "monitor_cond", "monitor_cond_since":
	default:
		return
	}
	var params []json.RawMessage
	if err := json.Unmarshal(msg["params"], &params); err != nil || len(params) < 3 {
		return
	}
	var reqs map[string]json.RawMessage
	if err := json.Unmarshal(params[2], &reqs); err != nil {
		return
	}
	changed := false
	for table, raw := range reqs {
		var list []map[string]json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			var one map[string]json.RawMessage
			if json.Unmarshal(raw, &one) != nil {
				continue
			}
			if _, ok := one["select"]; ok {
				continue
			}
			list = []map[string]json.RawMessage{one}
		}
		merged, err := json.Marshal(mergeMonitorRequests(list))
		if err != nil {
			return
		}
		reqs[table] = merged
		changed = true
	}
	if !changed {
		return
	}
	if b, err := json.Marshal(reqs); err == nil {
		params[2] = b
		if p, err := json.Marshal(params); err == nil {
			msg["params"] = p
		}
	}
}

func mergeMonitorRequests(list []map[string]json.RawMessage) map[string]any {
	cols := map[string]bool{}
	all := len(list) == 0
	for _, r := range list {
		raw, ok := r["columns"]
		if !ok {
			all = true
			continue
		}
		var c []string
		if json.Unmarshal(raw, &c) != nil {
			all = true
			continue
		}
		for _, name := range c {
			cols[name] = true
		}
	}
	// libovsdb dereferences "select" unconditionally, so it is always sent.
	out := map[string]any{"select": map[string]bool{"initial": true, "insert": true, "delete": true, "modify": true}}
	if !all {
		names := make([]string, 0, len(cols))
		for n := range cols {
			names = append(names, n)
		}
		sort.Strings(names)
		out["columns"] = names
	}
	return out
}

var noRowSelect = json.RawMessage(`{"op":"select","table":"Open_vSwitch","columns":["_uuid"],` +
	`"where":[["_uuid","==",["uuid","00000000-0000-0000-0000-000000000000"]]]}`)

func rewriteTransact(msg map[string]json.RawMessage) {
	var method string
	if err := json.Unmarshal(msg["method"], &method); err != nil || method != "transact" {
		return
	}
	var params []json.RawMessage
	if err := json.Unmarshal(msg["params"], &params); err != nil {
		return
	}
	changed := false
	for i := 1; i < len(params); i++ {
		var op struct {
			Op      string `json:"op"`
			Until   string `json:"until"`
			Timeout *int   `json:"timeout"`
		}
		if json.Unmarshal(params[i], &op) != nil {
			continue
		}
		idlWait := op.Op == "wait" && op.Until == "==" && op.Timeout != nil && *op.Timeout == 0
		if op.Op == "comment" || idlWait {
			params[i] = noRowSelect
			changed = true
		}
	}
	if changed {
		if p, err := json.Marshal(params); err == nil {
			msg["params"] = p
		}
	}
}

// rewriteUpdate fixes the method name of a libovsdb update and turns it into a
// notification. It returns the original request id, which the proxy answers on
// the client's behalf: libovsdb blocks the transact reply until every update
// call is answered, and the OVS IDL ignores updates that are not notifications.
func rewriteUpdate(msg map[string]json.RawMessage) (json.RawMessage, bool) {
	var method string
	if err := json.Unmarshal(msg["method"], &method); err != nil {
		return nil, false
	}
	switch method {
	case "update", "update2", "update3":
	default:
		return nil, false
	}
	var params []json.RawMessage
	if err := json.Unmarshal(msg["params"], &params); err != nil {
		return nil, false
	}
	rename := method
	switch {
	case len(params) == 3:
		rename = "update3"
	case len(params) == 2 && isLegacyUpdate(params[1]):
		rename = "update"
	}
	if m, err := json.Marshal(rename); err == nil {
		msg["method"] = m
	}
	id := msg["id"]
	msg["id"] = json.RawMessage("null")
	return id, len(id) > 0 && string(id) != "null"
}

func isLegacyUpdate(raw json.RawMessage) bool {
	var tables map[string]map[string]map[string]json.RawMessage
	if json.Unmarshal(raw, &tables) != nil {
		return false
	}
	for _, rows := range tables {
		for _, row := range rows {
			_, hasNew := row["new"]
			_, hasOld := row["old"]
			return hasNew || hasOld
		}
	}
	return false
}
