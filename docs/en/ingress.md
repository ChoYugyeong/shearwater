# Ingress (`networking.k8s.io/v1`)

*Translated from the [Korean original](../ingress.md). If they differ, the Korean version is current.*

Shearwater handles Kubernetes `Ingress` and `IngressClass` resources directly in its existing [public entry](../public-entry.md) (in Korean) (`sw edge`, `internal/edge`), without a separate Ingress controller Pod.

## Default IngressClass

On startup, if the `IngressClass` `shearwater` (controller `shearwater.io/ingress`) does not exist, the server creates it automatically and adds the default class annotation (`ingressclass.kubernetes.io/is-default-class: "true"`).

Shearwater routes only Ingresses whose `spec.ingressClassName` is empty or `shearwater`. Ingresses that specify any other class name are ignored.

## Path Matching Rules

You can define multiple path rules under a single host, following Kubernetes Ingress rules.

- **`Exact`**: Must match the URL path exactly, case-sensitively (`/foo` matches only `/foo`, not `/foo/`).
- **`Prefix`**: Matches by path elements separated by `/`.
  - `/` matches all paths.
  - `/foo` matches `/foo`, `/foo/`, and `/foo/bar`, but not `/foobar`.
  - `/foo/` matches `/foo/` and `/foo/bar`, but not `/foo`.
- **`ImplementationSpecific`**: Handled the same way as `Prefix`.
- **Precedence**:
  1. Rules with a matching `host` take precedence over hostless rules (empty `host`).
  2. Among matching paths, the longest path wins.
  3. When path lengths are equal, `Exact` takes precedence over `Prefix`.
  4. If no rule matches, traffic goes to `spec.defaultBackend`.
- Backend ports support both `service.port.number` and `service.port.name`.
- The existing Service annotation method (`shearwater.io/public-host`, `shearwater.io/public-port`) continues to work alongside Ingress.

## TLS Certificates

When HTTPS is enabled on the public entry (`sw edge enable --agree-tos ...`):

- If you set `spec.tls[].secretName`, Shearwater uses the certificate from the `kubernetes.io/tls` Secret (`tls.crt`, `tls.key`) in the same namespace. Replacing the Secret takes effect without a restart.
- Hosts without `secretName`, or without a `spec.tls` entry, continue to obtain and renew certificates automatically through ACME (HTTP-01).

## Host Ownership (Multi-Namespace Isolation)

- If you try to create or update an Ingress or a Service annotation (`shearwater.io/public-host`) in another namespace with a host name already used by an Ingress or Service in a different namespace, admission rejects it with **409 Conflict**.
- Within the same namespace, multiple Ingresses can share the same host name (for example, splitting `/api` and `/web` into separate Ingresses).
- In the routing table as well, the object created first takes precedence on conflict.

## Status and `kubectl` Output

- While the public entry is running, Shearwater writes the address it listens on to each Ingress's `status.loadBalancer.ingress[].ip`.
- `kubectl get ingress` displays the `NAME CLASS HOSTS ADDRESS PORTS AGE` columns.
- `kubectl create ingress` (Protobuf request body) is supported.

## Limitations

- Wildcard hosts (`*.example.com`) and `resource` backends are not supported.
- URL rewrites, rate limiting, and custom nginx annotations are not supported.
