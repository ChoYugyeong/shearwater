# 클러스터 DNS

Shearwater는 별도의 CoreDNS 파드 없이 각 노드의 `sw` 프로세스 안에서 클러스터 DNS 서버를 직접 실행한다.

## 동작 방식

- **수신 주소**: 각 노드의 Pod 네트워크 게이트웨이 주소(`Node.spec.podCIDR`의 첫 주소, 컨트롤 플레인은 `10.244.0.1`)의 **UDP/TCP 53번 포트**. 브리지 주소가 아직 준비되지 않았으면 생길 때까지 잠시 기다렸다가 다시 바인드한다.
- **상태 읽기**: `store.Reader`를 통해 Service·EndpointSlice·Pod를 읽고, 변경이 생길 때만 메모리 안의 레코드 표를 다시 만든다. 질의마다 저장소를 읽지 않는다. 워커 노드에서도 같은 코드가 노드 API를 통해 동작한다.
- **TTL**: 모든 클러스터 응답은 짧은 TTL(5초)을 사용한다.

## 제공하는 레코드 (`cluster.local`)

| 레코드 | 종류 | 값 |
|---|---|---|
| `<svc>.<ns>.svc.cluster.local` | `A` | 일반 Service는 ClusterIP. Headless Service(`clusterIP: None`)는 EndpointSlice 기준 Ready Pod IP 목록 |
| `_<port>._<proto>.<svc>.<ns>.svc.cluster.local` | `SRV` | 이름이 붙은 Service 포트(ClusterIP Service는 Service FQDN, Headless Service는 Ready Pod FQDN과 대상 포트) |
| `<a-b-c-d>.<ns>.pod.cluster.local` | `A` | 실행 중인 Pod의 IPv4 주소 |
| `<d>.<c>.<b>.<a>.in-addr.arpa` | `PTR` | Service IP 또는 Pod IP에 해당하는 클러스터 FQDN |

- 존재하지 않는 `cluster.local` 이름(또는 클러스터 대역 안의 미할당 역방향 주소)은 `NXDOMAIN`으로 답한다.
- 존재하는 클러스터 이름에 대한 `AAAA` 질의는 빈 응답(`NOERROR`)을 돌려준다.
- `cluster.local` 밖의 이름은 호스트의 업스트림 DNS로 전달한다. `/run/systemd/resolve/resolv.conf`가 있으면 우선 읽고, 없으면 `/etc/resolv.conf`에서 `127.0.0.0/8`과 `::1`을 제외한 `nameserver`를 사용한다. 사용할 업스트림이 없으면 `SERVFAIL`로 답한다.

## Pod `resolv.conf` (`dnsPolicy`와 `dnsConfig`)

Pod를 처음 만들 때 `spec.dnsPolicy`와 `spec.dnsConfig`에 따라 `/etc/resolv.conf`를 한 번 생성한다. 이미 생성된 Pod의 `resolv.conf`는 다시 쓰지 않는다.

| `dnsPolicy` | 생성 내용 |
|---|---|
| `ClusterFirst` (기본값), `ClusterFirstWithHostNet` | `nameserver <노드 게이트웨이>`, `search <ns>.svc.cluster.local svc.cluster.local cluster.local`, `options ndots:5`. `dnsConfig`가 있으면 추가·병합 |
| `Default` | 호스트의 `/etc/resolv.conf`를 그대로 복사(`dnsConfig`가 있으면 병합) |
| `None` | `spec.dnsConfig`에 적은 `nameservers`, `searches`, `options`만 사용 |

## 제한

- IPv4 전용이다(`AAAA`는 클러스터 이름에 대해 빈 응답).
- `ExternalName` Service와 Pod `subdomain`/`hostname` 기반 DNS 레코드는 구현하지 않았다.
- 이미 만들어진 Pod의 `/etc/resolv.conf`는 설정이 바뀌어도 다시 쓰지 않는다.
