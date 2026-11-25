# Run Shearwater on multiple servers

*Translated from the [original guide](../multi-node.md) (in Korean).*

You can run Shearwater on one server or add workers to a single control plane. Apps can run across several servers. If a worker stops, its Deployment can start replacement Pods on another node. You can reach the app through the same Service name, DNS, and Ingress wherever its Pods run.

## Architecture

```
                kubectl, sw (admins and users)
                         │ HTTPS 6443
          ┌──────────────▼──────────────┐
          │ Control plane (sw up)       │  SQLite, API, scheduler, node lifecycle, public entry point
          │ Local node: 10.244.0.0/24   │  Can also run apps on its own node
          └───┬─────────────────────┬───┘
     Node API │ HTTPS 6443          │      VXLAN UDP 8472 (Pod network between all servers)
       ┌──────▼──────┐       ┌──────▼──────┐
       │ Worker w1   │       │ Worker w2   │  Logs: TCP 10250 (control plane → worker)
       │10.244.1.0/24│       │10.244.2.0/24│  Node agent, Service proxy, DNS, network policy
       └─────────────┘       └─────────────┘
```

- The cluster has **one control-plane server**. It stores cluster state in SQLite. Workers have no database. Each worker uses its node credentials to read and write state through the control plane's node API. It can write only to resources assigned to its node.
- If the control plane stops, the API, scheduling, and rescheduling stop. Apps already running on workers keep running, and workers continue forwarding Service traffic. With a second control plane (a standby) and a witness, the standby takes over when the leader dies: [Two control planes](ha.md).
- Each server runs a single long-lived Shearwater process, `sw`.

## Requirements

- You need Linux, root access, and containerd on each server. See the [installation guide](install.md).
- Allow TCP **6443** from workers and kubectl clients to the control plane, UDP **8472** between all servers for VXLAN, and TCP **10250** from the control plane to workers for `kubectl logs`.
- **VXLAN traffic is unencrypted.** Use a private network such as a LAN or cloud VPC, or a VPN such as WireGuard.
- The servers need Layer 3 connectivity. They can be on different Layer 2 networks.

## Get started

1. Start the control plane with an address the other servers can reach. Shearwater saves this address in the configuration directory for subsequent runs of `sw up`.

   ```bash
   sudo sw up --advertise-address 192.168.1.10
   ```

   Local tools continue to use `127.0.0.1:6443`. Other servers and remote kubectl clients connect to `192.168.1.10:6443`. The server certificate includes the advertised address.

2. Get the join command.

   ```bash
   sudo sw node token            # Valid for 24 hours by default. Use --ttl 2h to change it (maximum 7 days).
   ```

   The output includes a one-line command like `sudo sw join https://192.168.1.10:6443 --token swj_... --ca-hash sha256:...`.

3. Run that line on the worker.

   ```bash
   sudo sw join https://192.168.1.10:6443 --token swj_... --ca-hash sha256:...
   ```

   The worker first fetches the control plane's CA certificate and compares its public key hash with `--ca-hash`, as kubeadm does. A mismatch stops the join.

   The node name defaults to the hostname (`--node-name`). The node IP defaults to the local address used to reach the control plane (`--node-ip`). When the worker joins, it saves the name, IP, assigned `/24` Pod subnet, node credentials, and node certificate in its configuration directory, then starts as it would with `sw up`. After that, use `sudo sw up` and `sudo sw down` to start and stop it.

4. Check the nodes from the control plane.

   ```bash
   kubectl get nodes -o wide
   sudo sw node list
   ```

## Scheduling

The scheduler considers Ready nodes. It checks whether a node is cordoned, whether the Pod tolerates its taints, and whether its labels match the Pod's `nodeSelector` and required `nodeAffinity`. It also checks total CPU and memory requests (using the corresponding limit when a request is missing), the limit of 110 Pods per node, and the location of any required volume.

Among eligible nodes, it favors those with more free capacity, fewer Pods with the same owner (the ReplicaSet for a Deployment's Pods), and a match for preferred `nodeAffinity`. If no node qualifies, the Pod condition gives the reasons in Kubernetes format.

```
0/3 nodes are available: 1 node(s) had untolerated taint {gpu: true}, 2 Insufficient cpu.
```

Use `kubectl cordon NODE` to stop new placements on a node and `kubectl taint` to add a taint.

## When a server stops

- Workers send a heartbeat every 10 seconds. The control plane records heartbeats in memory without writing them to disk. After **40 seconds** of silence, the control plane marks the node `NotReady` (`Ready=Unknown`), adds the `node.kubernetes.io/unreachable:NoExecute` taint, and immediately removes the node's Pods from Service endpoints.
- **300 seconds** after the taint is added, the control plane marks Pods without a matching toleration for deletion. Their Deployments create replacements on other nodes. To change the wait, give the Pod a toleration for `node.kubernetes.io/unreachable` and set `tolerationSeconds`.
- When the node returns, the control plane removes the taint and the node agent cleans up leftover containers.
- If the server will not return, run `sudo sw node remove NAME`. This removes the Node object, the Pods bound to it, their IP allocations, and the node credentials.
- A Pod that uses a `local-path` PVC can run only on the node that holds its data. If that node does not return, the Pod stays Pending.

## Networking

- Each node gets a `/24` Pod subnet from `10.244.0.0/16`. Traffic between nodes uses the VXLAN device `sw-vxlan0`. Each node publishes its device's MAC address in the `shearwater.io/vtep-mac` Node annotation. MAC learning is disabled, so the device communicates only with peers announced by the control plane.
- In a multi-server cluster, the Pod interface MTU is 50 bytes smaller to allow for the VXLAN header. Existing Pods get the new MTU when they are recreated.
- From any node, a Service ClusterIP distributes connections to backend Pods across all nodes. Connections from Pods retain their source IP address, so NetworkPolicy still applies.
- Cluster DNS answers on each node at that node's gateway (`10.244.N.1:53`).
- The public entry point (Ingress) runs on the control plane and can reach Pods on other nodes. It reuses backend connections, so successive requests can reach the same Pod. Concurrent requests can reach different Pods.

## Logs

For `kubectl logs` on a worker Pod, the control plane relays the request to port 10250 on the worker running the Pod. The worker accepts only the control plane's client certificate signed by the cluster CA.

## What was tested

On 2026-11-24, the QA environment simulated three servers. Each had its own network namespace, containerd instance, and cgroup. They shared a private underlay network, `192.168.77.0/24`. The records and test harness are in `maintenance/20261124-multinode-tenancy/qa/`. The three runs after the PVC step was added (`mnq-7`, `mnq-8` and `mnq-9`) passed all nine steps; `mnq-8` and `mnq-9` tested the PVC on worker `w1`.

1. The control plane started and issued a join token.
2. Two workers joined and all three nodes became Ready.
3. One Deployment's six replicas spread evenly across the nodes, two per node.
4. From a worker Pod, DNS resolved, the Service reached all six Pods across the three nodes, and a direct connection to a Pod IP on another node succeeded. Log relay also worked.
5. Ingress served requests and returned 404 for an unknown host. From the control plane, the ClusterIP reached Pods on all three nodes.
6. Tenant isolation returned HTTP 403 for access to another namespace. Pod Security rejected a `hostPath` volume, and quota admission rejected a request over quota.
7. NetworkPolicy blocked access from another namespace across nodes.
8. PVC data stayed on the node where it was written (worker `w1` in `mnq-8` and `mnq-9`). The next Pod using the PVC ran on that same node. The data was removed only after PVC deletion.
9. After a worker lost network connectivity, it became NotReady and replacement Pods with `tolerationSeconds: 20` became Ready on other nodes. Across the QA runs (`mnq-6` to `mnq-9`), the node became NotReady 40–46 seconds after disconnection and the replacements were Ready after 65–68 seconds. The taint cleared after recovery.

The simulated servers shared one kernel. Testing has yet to cover separate physical servers, cloud networks, or firewalls. The timings apply to this environment.
