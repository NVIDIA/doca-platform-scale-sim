# mock-ovs

`mock-ovs` replaces `ovsdb-server` and `ovs-vswitchd` on a SimFleet VM. It
serves the same sockets in `--run-dir` (default `/var/run/openvswitch`):

- `db.sock`: an OVSDB server (libovsdb `server` with the `ovsmodel` schema
  and an in-memory database).
- `<bridge>.mgmt` for every bridge: an OpenFlow 1.3/1.4/1.5 switch that keeps
  flows and meters and answers flow-mod, bundles, flow stats, table features
  and barriers. It does not forward packets.
- `ovsdb-server.pid` and `ovs-vswitchd.pid`, which sfc-controller watches.

An in-process vswitchd emulator seeds the root row and the `--bridges`, and
for every interface fills `ofport`, `link_state`, `admin_state`, `error` and
`cur_cfg` the way `ovs-vswitchd` does: bridge-internal interfaces get 65534,
`--physical-ports` get 1..N, a free `ofport_request` is honored, and the rest
get the lowest free number.

```sh
mock-ovs --bridges=br-sfc,br-hbn,br-ovn --physical-ports=p0,p1 \
  --ofport-delay=2s --fail-interfaces='pf1vf*,en3f1pf1sf3'
```

- `--ofport-delay` holds back `ofport` and `link_state=up` for new interfaces,
  to exercise dpf-ovs-cni's `waitLinkUp` and sfc-controller retries.
- `--fail-interfaces` takes glob patterns; matching interfaces get
  `ofport=-1`, `link_state=down` and
  `error="could not open network device X (No such device)"`.
- `-v` turns on verbose logging, including the libovsdb server logs. Without
  it, only the ready message and errors are logged.

## Architecture

`mock-ovs` is one process. `mockovs.Run` starts three parts, each in its own
package, and a client sees them as the two daemons it expects.

```text
   clients                         mock-ovs process                 run dir (--run-dir)
 (sfc-controller,
  dpf-ovs-cni,
  ovs-vsctl, ...)
                         +--------------------------------------+
  OVSDB (JSON-RPC) ----> | ovsdbserver                          |   db.sock
                         |   compat proxy  (compat.go)          | - - - - - - - - - -
                         |     rewrites what libovsdb rejects   |
                         |   libovsdb server, in-memory DB      |
                         |     Open_vSwitch schema (ovsmodel)   |
                         +-------------------+------------------+
                                             ^
                          OVSDB client:      | monitor all tables,
                          writes ofport,     | one transaction per pass
                          link_state, ...    v
                         +--------------------------------------+
                         | vswitchd (emulator)                  |   ovs-vswitchd.pid
                         |   seeds root row and --bridges       |   ovsdb-server.pid
                         |   reconcile: ofport, link_state,     | - - - - - - - - - -
                         |   admin_state, error, cur_cfg        |
                         +-------------------+------------------+
                                             | bridge names changed:
                                             | Manager.Sync(bridges)
                                             v
                         +--------------------------------------+
  OpenFlow 1.3/1.4/1.5   | ofswitch.Manager                     |   br-sfc.mgmt
  (Antrea OFBridge,  --> |   one Switch per bridge in the DB    |   br-hbn.mgmt
   ovs-ofctl)            |   flow table, meters, bundles,       |   ...
                         |   stats, table features, barriers    |
                         |   (control plane only, no packets)   |
                         +--------------------------------------+
```

- **`ovsdbserver`** serves the `Open_vSwitch` database on `db.sock`. libovsdb
  provides the server; a small proxy in front of it fixes the protocol
  differences the OVS C tools hit (see [Clients](#clients)).
- **`vswitchd`** is an ordinary OVSDB client of that server, so it needs no
  private access to the database. It plays the part of `ovs-vswitchd`: it
  creates the root row and the `--bridges` at startup, then reconciles the
  whole database on every change. Interfaces that are not ready yet are
  retried when their `--ofport-delay` expires.
- **`ofswitch`** exposes a `<bridge>.mgmt` socket for every row in the `Bridge`
  table. `vswitchd` reports the bridge names, so a bridge added with
  `ovs-vsctl add-br` gets a switch and a removed bridge loses it. Flows survive
  client reconnects.

Adding a port shows how they fit together:

1. A client (sfc-controller, `ovs-vsctl add-port`) adds `Port` and `Interface`
   rows to a bridge over `db.sock`.
2. `vswitchd` sees the change and picks an `ofport`:
   - the bridge's own interface gets 65534;
   - a `--physical-ports` name gets its position, so `p0` is 1 and `p1` is 2;
   - a free `ofport_request` is honored;
   - otherwise the lowest free number is used, skipping the numbers reserved
     for `--physical-ports`.

   It then sets `link_state=up` and `admin_state=up` in one transaction. With
   `--fail-interfaces` it writes `ofport=-1`, `link_state=down` and an `error`
   instead.
3. The client reads the `ofport` back from the database and uses it in
   OpenFlow rules on `<bridge>.mgmt`. `ofswitch` stores the flows and answers
   dumps and stats. Nothing forwards packets.

The emulator's own writes trigger one more pass. That pass finds nothing to
change and writes nothing, so the emulator settles.

## Clients

The tests use the clients the product uses: libovsdb through `ovsutils`, with
the connection steps of dpf-ovs-cni's connector copied into the test; Antrea's
`OFBridge` (connect, add a table, dump flows, delete by cookie); and `ovs-ofctl`
through `pkg/openflow` (`--bundle add-flows`, `add-meter`). The OVS C tools are
tested as well: `ovs-vsctl` on the OVSDB socket and `ovs-ofctl -O OpenFlow13` on
the bridge sockets. The switch offers only OpenFlow 1.3 to 1.5 in its hello, so
`ovs-ofctl` without `-O`, which starts at OpenFlow 1.0, is not one of the
versions it negotiates.

The OVS C IDL needs a few protocol fixes over libovsdb v0.7.0, done by a
proxy in front of the server (`ovsdbserver/compat.go`): monitor requests
given as arrays are merged, `comment` operations and the IDL's optimistic
`wait` operations are replaced by no-op selects, and updates are sent as
notifications named `update`/`update2`/`update3`.

## Run

`pkg/mock-ovs` is its own Go module with its own Makefile. The repository root
`make test` and `make lint` run it too; `./...` from the root does not reach it.

```sh
make -C pkg/mock-ovs build
make -C pkg/mock-ovs test
make -C pkg/mock-ovs lint
```

`build` writes the repository's `bin/mock-ovs` (linux/amd64, static). The tests
that drive `ovs-vsctl` and `ovs-ofctl` skip when those tools are missing, and
they fail instead when `CI` is set, so CI must install Open vSwitch.

On a SimFleet VM, `images/simfleet-node/customize-disk.sh` installs the binary
in `/usr/local/bin` and the unit
[guest/mock-ovs.service](../../images/simfleet-node/guest/mock-ovs.service).
The node-in-a-pod image runs the same binary and unit.
