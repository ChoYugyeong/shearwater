# Two control planes: a standby and a witness

**English** · [한국어](../ha.md)

Through alpha.16, a Shearwater cluster had one control plane. You could attach multiple workers, but if the control-plane server stopped, the API and Pod rescheduling stopped with it (apps already running on workers kept running). This guide explains how **two control planes with a witness** work. For the design and rationale, see [section 3 of the design document](../design-agent-first-ha.md#3-컨트롤-플레인-고가용성) (in Korean).


## How it works

```
        kubectl · sw · workers (know both addresses)
              │
   ┌──────────▼─────────┐  write batches   ┌────────────────────┐
   │ Control plane A    │ ───────────────▶ │ Control plane B    │
   │ Leader: API · ctrl │ ◀─────────────── │ Standby: DB copy   │
   │                    │      ack         │ + runs apps as     │
   └──────────┬─────────┘                  │   a worker         │
              │    heartbeats · votes      └─────────┬──────────┘
              └────────────▶ Witness W ◀─────────────┘
                        (no data, one vote)
```

- **Leader** serves the API. While the standby is in sync, the leader sends every write transaction to the standby and waits until the standby confirms it committed that batch to its own SQLite database before the leader commits locally and returns success. A confirmed write therefore survives an abrupt leader crash. A newly started leader begins in solo mode until the standby catches up and a write brings it back into sync with the witness.
- **Standby** normally runs apps as a worker of the leader and forwards incoming TCP connections on port 6443 straight to the leader (TLS and all), so `kubectl` or workers can connect to either address while the standby is following the leader.
- **Witness** holds no cluster data and casts one tie-breaking vote. When the leader disappears, the witness decides whether the standby may take over (confirming that the leader has really gone silent and that the standby is in sync). Two servers alone cannot tell "the peer is dead" from "the link between us broke," so a third vote is required. The witness can run on an existing worker or any small Linux machine and does not need root or containerd.

## Setup

Requirements: three reachable Linux servers (two control planes + one witness; the witness may be an existing worker).
Required ports: control plane TCP **6443**, witness TCP **6445**, VXLAN UDP **8472** between nodes, and TCP **10250** between servers for `kubectl logs` on Pods running on workers and the standby.

```bash
# 1. Control plane A (as before)
sudo sw up --advertise-address 10.0.0.10

# 2. On A: print one join command for the second control plane and one for the witness
sudo sw node token --control-plane
sudo sw node token --witness

# 3. Control plane B: run the control-plane join command from step 2, then start
sudo sw join https://10.0.0.10:6443 --token swj_... --ca-hash sha256:... --control-plane
sudo sw up                                   # starts as standby

# 4. Witness W: run the witness join command from step 2, then start the witness (a service is recommended)
sw join https://10.0.0.10:6443 --token swj_... --ca-hash sha256:... --witness --config-dir ~/.config/shearwater-witness
sw ha witness --config-dir ~/.config/shearwater-witness

# Status
sudo sw ha status
```


Running `sw node token --control-plane` the first time enables high availability on A (writing `ha.json` and turning on replication-log recording). Until then, A runs as a single control plane just as before.

## How clients know both addresses

- **Workers** keep the control-plane address list from the join in `node.json`, move to the other address when one stops answering (or answers 503 `NOT_LEADER`), and learn membership changes from the `X-Shearwater-Control-Planes` header of node API responses.
- **kubectl**: a kubeconfig cluster entry holds only one `server` URL. Because the standby forwards TCP connections on port 6443 to the leader, pointing `kubectl` at the **standby's IP address** works during normal operation and keeps working if that standby becomes leader. On startup, the control plane ensures its serving certificate covers both control-plane IP addresses (as well as loopback). Note that the server certificate currently includes only IP SANs and `DNSNames: ["localhost"]` — pointing `kubectl` at a custom DNS or `/etc/hosts` name will fail TLS verification until DNS SAN support is added.

## When something fails

| Situation | What happens |
| --- | --- |
| Leader dies | After 6 s of silence (`FailAfter`), the standby asks the witness for a vote. The witness grants the next epoch only after it has not heard a leader heartbeat for 7 s (`VoteAfter`) and confirms the standby is in sync. The standby re-execs in place (same PID) as the new leader. Measured in QA: 7.4 s. |
| Standby dies | The next write waits up to 2 s (`SyncTimeout`), then asks the witness to take the standby out of the sync set and continues writing in solo mode (one copy while the standby is away). |
| Witness dies | Both control planes keep running in synchronous mode. If the leader also dies afterward, automatic takeover cannot happen. |
| Leader loses contact with both standby and witness | While the standby is in sync, the leader loses its 5 s lease (`LeaseFor`), fences writes, and restarts as a standby to avoid split brain. (If the leader was already in solo mode after the witness desynced the standby, the witness will not elect the standby, so the leader continues writing alone.) |
| Old leader returns | It starts as a standby and takes a fresh snapshot from the new leader. Its previous database files are kept beside the new one as `state.db.replaced-<UTC-timestamp>` (along with `-wal` and `-shm`), never deleted. |

When a cluster has two control planes and a witness, a restarting server starts as a standby, except that if it was the current epoch's leader it immediately asks the witness for the next epoch (`CampaignNow`) without waiting for `VoteAfter`, so planned restarts do not trigger a failover. When a control plane is alone (fewer than two members) or is the leader without a witness (and the peer is not leading a later epoch), it starts directly as leader.

**With two control planes and no witness**, automatic takeover is off and there is no leader lease. Only when you have verified that the old leader is truly stopped should you run `sudo sw ha promote --force` on the standby. Running it while the old leader is still alive causes both servers to accept writes independently (split brain).

## What is guaranteed and what is not

- **Guaranteed:** If any **one** of the leader, standby, or witness crashes or disconnects, writes that received a success response are not lost. While the witness and one in-sync control plane are alive, failover happens without human intervention, and apps running on workers keep running during the switch.
- **Not supported:** Simultaneous loss of two nodes, writes on the minority side of a network partition (refused), read scaling on the standby, three or more control planes, or automatic member replacement.
- **Ambiguous writes:** A write whose response was lost during a switch (HTTP 503 `NOT_LEADER retry=unsafe` or a dropped connection) may or may not have been applied on the standby. Check the object's state before retrying.
- **Transport security:** Replication between control planes and communication with the witness use HTTPS verified by the cluster CA and a shared cluster HA bearer token stored in `ha.json` (mode `0600`, readable only by its owner).

## QA results

In the QA distribution's three-server simulation (each node its own network namespace, containerd and
cgroup; cp1 leads, cp2 is the standby, w1 is a worker that also runs the witness), one client kept writing
Secrets through cp2's address and a probe asked the app's Service every 0.2 s while the leader process was
killed with `SIGKILL` (2026-11-25, run `mnq-ha6`; harness and raw data in
`maintenance/20261125-agent-first-ha/qa/`). This is one run; the times move by a few seconds with the
heartbeat timing (the earlier run `mnq-ha5` had a 5.35 s write gap).

| Check | Result |
| --- | --- |
| From killing the leader until cp2 led epoch 2 | 7.4 s |
| Write attempts / success answers (201) / failures | 1,903 / 1,868 / 35 (connections cut during the switch) |
| Writes answered with success but missing on the new leader | **0** |
| Longest time without a successful write | 7.26 s (from the kill) |
| kubectl (kubeconfig with cp2's address) and the worker | carried on; three nodes Ready |
| App probe every 0.2 s | 160 successes, 0 failures |
| Agent records (`sw changes --actor`, delegated task evidence) | intact on the new leader |
| The old leader's `sw up` | came back as the standby from a snapshot and got back in sync |
| Killing the standby | the next write succeeded after 2.1 s (alone, with the witness's consent) |
| Then killing the witness | the leader kept writing |
