# 롤링 업데이트와 서버 재시작 때 TCP 연결 유지하기

이 문서는 **v0.1.0-alpha.5부터 제공하는 연결 유지 동작**을 설명해. alpha.4 이하의 실행 파일은 프로세스 내부 TCP 프록시를 사용해. 개발 단계 실측과 릴리스 파일의 검증은 [검증 기록](connection-verification.md)과 [릴리스 기록](releases.md)을 구분해 봐.

## 왜 주소는 같은데 요청이 실패했을까

Service IP는 앱의 고정된 접속 주소야. TCP 연결은 클라이언트와 특정 Pod 사이에서 이미 오가던 통신이야. 주소가 같아도 기존 연결의 상대를 중간에 바꿀 수는 없어. HTTP keep-alive는 연결 하나로 여러 요청을 보내는 기능이고, WebSocket이나 큰 파일 다운로드도 연결이 오래 지속되는 예야.

alpha.4에서는 `sw server` 프로세스가 연결을 직접 받아 Pod와 데이터를 복사했어. sw를 종료하면 그 소켓도 닫혔어. 롤링 교체에서는 이전 Pod에 연결이 남아 있는지 확인하지 않고 종료 신호를 보냈어. 그래서 준비된 다른 Pod가 있어도 기존 연결의 요청은 실패할 수 있었어. `maxUnavailable: 0`만으로 해결되는 문제는 아니야.

## 새 전달 방식: 커널이 연결을 기억해

sw는 여전히 하나의 바이너리와 하나의 서버 프로세스야. 별도 상주 프록시나 CNI 플러그인을 설치하지 않아. 서버는 SQLite와 EndpointSlice를 읽고 **Linux 커널에 전달 규칙을 설정**해. 실제 데이터는 커널을 통해 흘러.

1. 새 TCP 연결이 Service IP와 포트로 들어와.
2. DNAT가 목적지를 준비된 Pod의 IP와 포트로 바꿔. DNAT는 ‘목적지 주소 변환’이야.
3. conntrack이라는 커널 연결 기록이 이번 연결에서 선택한 Pod를 기억해. 이후 패킷은 같은 Pod로 가.
4. sw를 정상 종료하거나 SIGKILL로 강제 종료해도 이 규칙과 기록은 남아. 살아 있는 Pod로 기존 연결과 새 연결을 전달할 수 있어.
5. sw가 돌아오면 같은 SQLite·Pod·IP를 확인하고 필요한 규칙을 다시 맞춰.

전달에 사용하는 체인은 `SHEARWATER-SERVICES`, `SHEARWATER-SVC-SNAT`, `SHEARWATER-SVC-INPUT`이야. 이름뿐 아니라 소유 표식을 확인해. 관련 없는 체인을 비우지 않고, `iptables-restore --noflush`로 지정한 전용 체인의 규칙만 한 테이블 트랜잭션으로 바꿔. [iptables-restore 설명](https://man7.org/linux/man-pages/man8/iptables-restore.8.html)

준비된 대상이 0개가 되어도 옛 연결 기록을 유지해야 해. iptables-nft에서 마지막 DNAT 규칙을 없앨 때 기존 연결이 끊기는 경계가 실제 실험에서 나타났어. 그래서 무조건 RETURN하는 규칙 **뒤에**, 실행될 수 없는 DNAT 규칙 하나를 남겨 커널의 전달 기능을 유지해. 대상이 없는 새 연결은 별도의 INPUT 규칙으로 거부해. 우연히 같은 포트를 쓰는 호스트 앱으로 보내지 않아.

Pod가 자신의 Service에 접속하는 경우도 지원해. 반환 패킷이 올바른 경로로 돌아오도록 SNAT도 적용해. 이 방식은 원래 클라이언트 IP를 앱에 그대로 전달하는 기능을 제공하지 않아. 준비된 Pod들에는 새 연결을 순번대로 분산해. 전달 규칙이 다시 설치되면 커널의 순번 카운터는 처음부터 시작할 수 있어.

자기 Pod로 되돌아오는 패킷은 들어온 브리지 포트로 다시 나가야 하므로, Shearwater 소유의 Pod veth 포트에만 hairpin 모드를 켜. netlink로 직접 설정하며 호스트 공통 sysctl이나 다른 앱의 인터페이스는 바꾸지 않아. 기존 Pod를 서버 재시작 뒤 이어받을 때도 설정을 확인해. [Linux 브리지 설명](https://docs.kernel.org/networking/bridge.html)

## 롤링 교체: 연결을 비운 다음 종료해

ReplicaSet이 Pod 수를 줄일 때 다음 순서로 처리해. Deployment의 롤링 업데이트도 이 경로를 사용해.

1. 이전 Pod에 `shearwater.io/connection-drain`이라는 내부 annotation을 기록해. 값은 이번 종료 대기 요청의 고유 번호야. 사용자가 수동으로 넣는 설정이 아니야.
2. EndpointSlice와 새 연결 대상에서 이 Pod를 제외해. 앱 프로세스는 계속 실행해.
3. 전달 규칙 변경이 성공했다는 확인을 받아. 단순히 DB에 표시했다고 바로 종료하지 않아.
4. conntrack을 읽어 이 Pod로 향하는 **기존 Service TCP 연결**을 세어. 읽기에 실패하면 ‘0개’로 취급하지 않아.
5. 연결이 모두 닫히면 Pod 삭제를 시작해. 연결이 계속 남으면 규칙 적용 확인부터 **최대 30초** 기다린 뒤 삭제를 시작해.
6. 그때부터 원래 `terminationGracePeriodSeconds`가 적용돼. 예를 들어 1초로 설정했어도 연결 종료 대기 30초와 별도야.

종료 대기 중인 Pod도 실제 Pod 수와 `maxSurge` 한도에 포함해. 새 Pod를 무한히 늘리지 않아. 대신 연결을 기다리는 만큼 전체 롤아웃 시간이 늘어날 수 있어. 중간에 다시 확장해 이 Pod가 필요해지면 종료 대기를 취소하고 대상으로 복귀시켜. 이후 다시 줄일 때는 새로운 요청 번호를 써서 이전 확인을 재사용하지 않아.

서버가 종료 대기 중 재시작되면 annotation은 DB에 남아. 전달 규칙과 연결 수를 다시 확인한 후 기다려. 이전 프로세스가 받았던 확인은 재사용하지 않으며, 중단 시간 때문에 확인 없이 바로 앱을 죽이지 않아.

## 보장 범위와 종료 경계

| 상황 | 동작 |
|---|---|
| 같은 호스트에서 `sw down` → `sw up` | 살아 있는 Pod의 Service TCP 전달 유지. API·컨트롤러·상태 검사는 중단 시간 동안 동작하지 않음 |
| sw 프로세스 SIGKILL 후 재시작 | 커널 전달 유지. PID 소유 검증을 거친 기존 시작 절차로 서버 복구 |
| 롤링 업데이트·ReplicaSet 축소 | 새 연결 제외 후 기존 Service 연결을 최대 30초 기다리고 Pod 유예 삭제 |
| 무한 WebSocket, 30초보다 오래 유지되는 연결 | 대기 한도 뒤 종료될 수 있음. 영구 무중단이나 자동 연결 이동을 보장하지 않음 |
| Pod IP에 직접 연결 | Service 연결 대기 대상으로 집계하지 않음. 앱의 종료 처리가 필요함 |
| 명시적인 Pod 강제 삭제, 소유자 삭제, `sw down --all` | 운영자의 종료 요청을 수행. 롤링 종료 대기 보장 대상이 아님 |
| 컨테이너 충돌·OOM·노드 또는 WSL 재부팅 | TCP 상대 또는 커널 자체가 없어짐. 기존 연결 유지 보장 대상이 아님 |
| 서버가 꺼진 동안 백엔드 장애 | 마지막 규칙이 남으므로 새 연결도 실패한 Pod를 선택할 수 있음. 서버가 복구되어 상태를 반영해야 함 |
| alpha.4에서 새 구현으로 최초 교체 | 이미 alpha.4 프로세스가 소유한 소켓을 커널로 옮기지 못함. 최초 전환 자체의 무중단은 보장하지 않음 |

실패한 요청을 다른 Pod에 자동 재전송하지 않아. 특히 결제·글 작성 같은 변경 요청은 응답을 못 받았더라도 서버에서 실행됐을 수 있어서 무조건 재시도하면 중복 처리될 수 있어. 앱 수준의 요청 식별자·재연결 정책도 필요해.

## IP가 재사용되어도 옛 연결이 새 앱에 가지 않도록

Pod IP는 유한해서 나중에 다른 Pod가 재사용할 수 있어. 이전 Pod의 전달 규칙이나 연결 기록이 남으면 다른 앱으로 바이트가 흘러갈 수 있지. 이를 막으려고 주소를 반납하기 전에 다음 장벽을 둬.

- 실제 Pod UID와 SQLite IP 예약을 확인해. 오래된 EndpointSlice만 믿지 않아.
- Service·EndpointSlice·Pod·IP 예약·종료 대기 표시는 같은 SQLite 읽기 트랜잭션에서 가져와. 규칙을 설치하던 중 새로 생긴 종료 대기를 이전 규칙의 확인으로 착각하지 않아.
- 규칙 갱신과 주소 반납을 같은 잠금으로 직렬화해. 오래된 스냅샷이 반납 뒤 다시 설치되지 않게 해.
- 옛 Pod IP에 연결된 Service TCP 기록만 제거해. 전체 conntrack 테이블을 비우지 않아.
- 삭제 요청은 원래 연결의 주소·포트 조합과 커널 ID를 같이 지정해. 그 사이 같은 클라이언트 포트를 재사용한 다른 연결을 지우지 않도록 해.
- Service UID가 바뀌는 경우에도 이전 UID의 기록을 정리한 다음 새 목적지를 활성화해. 정리 실패·프로세스 중단 시에는 다음 시도에서도 이전 UID 정리가 필요하다는 표식을 유지해.

Service를 없앨 때는 먼저 전달 규칙과 해당 연결 기록을 정리하고, 마지막에 `sw-svc0`의 주소를 떼어. 주소만 먼저 없애면 짧은 동안 남은 DNAT 규칙이 계속 요청을 전달할 수 있기 때문이야.

연결 기록은 직접 작성한 netlink 코드로 읽어. Linux의 [conntrack UAPI](https://github.com/torvalds/linux/blob/master/include/uapi/linux/netfilter/nfnetlink_conntrack.h)와 [TCP 상태 정의](https://github.com/torvalds/linux/blob/master/include/uapi/linux/netfilter/nf_conntrack_tcp.h)를 사용해. 잘린 덤프나 중간에 끊긴 덤프는 오류이며, 종료 대기 완료로 취급하지 않아.

## 실제 시험 재현

먼저 운영 앱이 없는 **별도 Linux/WSL 환경과 사설 네트워크 네임스페이스**를 준비해. WSL 배포판끼리 외부 네트워크를 공유할 수 있으므로 배포판 이름이 다르다는 이유만으로 같은 기본 네트워크에서 실행하면 안 돼. 아래 스크립트는 containerd의 `shearwater` 네임스페이스가 비어 있는지 확인하지만, 외부 호스트 네트워크 격리까지 자동으로 만들어 주지는 않아.

격리 네트워크의 기본 경로도 확인해야 해. QA와 운영 클러스터가 같은 IP 대역을 쓰면, QA에서 삭제한 Service 주소로 보낸 요청이 기본 경로를 따라 운영 앱에 도달할 수 있어. 이번 실측에서는 QA 네임스페이스 안에서만, 외부 인터페이스로 나가려는 Pod·Service 대역 패킷을 차단했어. 정상 DNAT 패킷은 QA의 Pod 브리지로 가므로 이 차단에 걸리지 않아. 실제 검증 조건과 수치는 [검증 기록](connection-verification.md)을 봐.

일반 사용자로 소스에서 빌드해.

```bash
. hack/env.sh
bash hack/t.sh
go build -o /tmp/sw-connection-test ./cmd/sw
```

아래는 이미 준비한 격리 네트워크 안에서 root로 실행할 예야. 모든 경로를 자기 환경에 맞춰 넣고, `--root`와 `--run`에는 **존재하지 않는 새 경로**를 써. SQLite는 `/mnt/c` 대신 Linux ext4에 두어야 해. 이미지 `docker.io/library/nginx:1.27`은 그 전용 runtime에 미리 준비해. 스크립트는 도구나 패키지를 설치하지 않아.

```bash
python3 hack/connection-check.py \
  --sw /tmp/sw-connection-test --kubectl /usr/local/bin/kubectl \
  --containerd-socket /run/shearwater/qa-containerd.sock \
  --root /var/lib/shearwater/check/connections-example \
  --run /run/shearwater/connections-example \
  --out /var/lib/shearwater/check/connections-results --port 16463
```

`--quick`은 짧은 재시작·빈 Endpoint 시험이야. 기본 시험은 8개 연결·초당 80요청으로 120초 동안 확장·롤링·재시작을 수행하는 과정을 3번 반복하고, 32개 연결·초당 1,000요청의 롤링도 실행해. 요청마다 1KiB 본문을 비교하고 소켓은 32회 요청 후 교체해. HTTP 자동 재시도는 0회야. 재시작 시험에서는 일부러 sw가 없는 시간을 5초 넣어서 단순히 재시작이 빠르기 때문에 성공한 결과와 구분해.

이어서 1MiB 파일을 초당 64KiB로 천천히 받으며 롤링과 sw 재시작을 겹쳐 실행해. 연결을 계속 유지하는 경우에는 약 30초의 대기 한도 뒤 닫히는지 확인하고, Service IP 재사용과 3분간 반복 재시작도 검사해. `--extended-only`는 앞의 반복 부하를 생략하고 이 경계 시험부터 실행해. `--soak-seconds`로 마지막 반복 재시작 구간의 길이를 바꿀 수 있어. 연결 대기 한도 시험의 예상 종료는 별도 항목에 기록하고 정상 요청 오류와 섞지 않아.

시간은 Linux의 `CLOCK_MONOTONIC`으로 측정해. 이는 sw가 경과 시간을 재는 기준과 같아. WSL에서 보정되지 않은 `CLOCK_MONOTONIC_RAW`와 속도가 달라지면 두 시계를 섞어 30초 한도를 판정할 수 없어. 결과 JSON의 `clock`에는 측정 기준과 시작·종료 때 두 시계의 값도 남겨. 느린 다운로드와 종료 한도 검사는 이전 롤링이 끝나고 현재 Ready Pod 하나만 남은 뒤 시작해.

`results.json`에 실패 시각·오류·단계·바이너리 SHA-256을 남겨. 성공 결과의 p95는 성공 요청 95%가 끝난 시간이고 실패 횟수를 대신하지 않아. 종료 때 자기 시험 노드의 `sw down --all`을 실행하되 DB·로그·hostPath 파일은 보존해. 실패 기록을 지우거나 실패를 성공 요청으로 바꾸지 않아.

## 확인할 명령과 코드

```bash
sw get pods -n default
sw events -n default
sw why pod/<Pod이름> -n default
kubectl get pods -o json
iptables -t nat -S SHEARWATER-SERVICES
```

종료 대기 annotation이 남은 Pod는 연결을 비우는 중일 수 있어. 새 대상 목록은 EndpointSlice에서 확인해. 네트워크 규칙 또는 conntrack 오류는 서버 로그의 `service forwarding` 메시지를 봐. `sw doctor`는 읽기 전용이며 이 규칙을 제거하지 않아. 커널 방식에는 Service TCP 리스너 프로세스가 없으므로 `ss -ltn`만으로 장애라고 판단하지 마.

Pod 이벤트의 `ConnectionDraining`은 연결 종료 대기의 시작, `ConnectionDrainTimeout`은 30초 뒤에도 연결이 남아 있어 종료 유예를 시작했다는 뜻이야. 후자는 연결이 끊길 수 있는 경계이므로, 앱의 연결 수명과 재연결 동작을 확인해. 커널 연결 덤프는 64MiB를 넘으면 불완전한 결과로 사용하지 않고 오류로 처리해.

현재 Pod 이벤트는 해당 Pod가 삭제되면 함께 정리돼. 롤아웃이 끝난 뒤 `sw events`를 조회하면 이전 Pod의 경고가 이미 없을 수 있어. 원인을 기록하려면 변경 **전에** `kubectl get events -n <네임스페이스> --watch`를 켜 두고 출력을 보존해. 부하 스크립트도 이 방식으로 실제 종료 경고를 기록해.

읽는 순서는 `internal/controller/replicaset.go` → `internal/drain/drain.go` → `internal/proxy/kernel.go` → `internal/runtime/services_linux.go` → `internal/runtime/serviceconn_linux.go`야. 각 파일에 순서와 실패 처리가 필요한 이유를 주석으로 적었어.
