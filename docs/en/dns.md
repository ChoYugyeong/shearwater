# Cluster DNS

*Translated from the [Korean original](../dns.md). If they differ, the Korean version is current.*

Shearwater runs a cluster DNS server directly inside the `sw` process on each node, without a separate CoreDNS Pod.

## How It Works

- **Listen address**: **UDP and TCP port 53** on each node's Pod network gateway address (the first address of `Node.spec.podCIDR`; `10.244.0.1` on the control plane). If the bridge address is not ready yet, the server waits briefly until it appears and binds again.
- **Reading state**: Reads Services, EndpointSlices, and Pods through `store.Reader` and rebuilds the in-memory record table only when changes occur. It does not query the store on each DNS request. The same code runs on worker nodes through the node API.
- **TTL**: Every cluster answer uses a short TTL (5 seconds).

## Records Served (`cluster.local`)

| Record | Type | Value |
|---|---|---|
| `<svc>.<ns>.svc.cluster.local` | `A` | ClusterIP for a regular Service; list of Ready Pod IPs from EndpointSlices for a Headless Service (`clusterIP: None`) |
| `_<port>._<proto>.<svc>.<ns>.svc.cluster.local` | `SRV` | Named Service ports (Service FQDN for a ClusterIP Service; Ready Pod FQDNs and target ports for a Headless Service) |
| `<a-b-c-d>.<ns>.pod.cluster.local` | `A` | IPv4 address of a running Pod |
| `<d>.<c>.<b>.<a>.in-addr.arpa` | `PTR` | Cluster FQDN matching a Service IP or Pod IP |

- Non-existent `cluster.local` names (and unassigned reverse addresses inside the cluster ranges) return `NXDOMAIN`.
- `AAAA` queries for an existing cluster name return an empty `NOERROR` response.
- Names outside `cluster.local` are forwarded to the host's upstream DNS. Shearwater reads `/run/systemd/resolve/resolv.conf` first if present; otherwise it reads `/etc/resolv.conf`, skipping `nameserver` entries in `127.0.0.0/8` and `::1`. If no upstream is available, it returns `SERVFAIL`.

## Pod `resolv.conf` (`dnsPolicy` and `dnsConfig`)

When a Pod is first created, Shearwater generates `/etc/resolv.conf` once according to `spec.dnsPolicy` and `spec.dnsConfig`. It does not rewrite `resolv.conf` for an already created Pod.

| `dnsPolicy` | Generated contents |
|---|---|
| `ClusterFirst` (default), `ClusterFirstWithHostNet` | `nameserver <node gateway>`, `search <ns>.svc.cluster.local svc.cluster.local cluster.local`, `options ndots:5`. If `dnsConfig` is set, its entries are added and merged |
| `Default` | Copies the host's `/etc/resolv.conf` as is (merged with `dnsConfig` if set) |
| `None` | Uses only the `nameservers`, `searches`, and `options` listed in `spec.dnsConfig` |

## Limitations

- IPv4 only (`AAAA` returns an empty response for cluster names).
- `ExternalName` Services and DNS records based on Pod `subdomain`/`hostname` are not implemented.
- An existing Pod's `/etc/resolv.conf` is not rewritten even if configuration changes.
