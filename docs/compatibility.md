# 1단계 호환 범위

Shearwater는 Kubernetes API의 일부를 구현한 단일 노드 오케스트레이터야. API discovery에 표시되는 자원·동작과 아래 표가 계약 범위야. Kubernetes 전체 동작이나 CNCF 호환성 인증을 의미하지 않아.

## 필드를 받았다는 것과 실행한다는 것

| 범위 | 실제로 수행하는 일 | 제한 |
|---|---|---|
| Pod | containerd 실행, IPv4, 로그·직전 로그, 재시작 정책, 유예 삭제 | 단일 노드, CNI/CRI 미사용 |
| Deployment / ReplicaSet | 복제본 유지, RollingUpdate/Recreate, `/scale`, 이력·되돌리기 | `minReadySeconds`, `progressDeadlineSeconds`는 저장만 함 |
| Service / EndpointSlice | IPv4 ClusterIP 예약, Ready Pod 선택, 이름 포트, Linux 커널 TCP 전달, 내장 클러스터 DNS(`cluster.local` A·SRV·PTR·headless, Pod `resolv.conf`와 `dnsPolicy`/`dnsConfig`) | UDP·NodePort·LoadBalancer·ExternalName 미구현. [클러스터 DNS](dns.md) |
| 공개 진입점 / Ingress | `networking.k8s.io/v1` Ingress·IngressClass(`shearwater`), 경로별 연결(`Exact`/`Prefix`/`ImplementationSpecific`, `defaultBackend`), TLS Secret과 ACME HTTP-01 자동 발급·갱신, 네임스페이스 간 호스트 소유권 거절(409), 기존 Service 주석 방식 호환 | 와일드카드 호스트·`resource` 백엔드·커스텀 리라이트 미지원. 격리 QA 엔진에서 Pebble로 확인, 실제 Let's Encrypt 서버에서는 미검증. [공개 진입점](public-entry.md), [Ingress](ingress.md) |
| 리소스 | CPU/메모리 limits 적용 | requests는 저장만 하고 배치에 쓰지 않음 |
| 볼륨 | emptyDir, hostPath와 type 검사·OrCreate 생성 | PVC/CSI 같은 영속 볼륨 체계 미구현 |
| Secret | `data`·`stringData`(쓰기 전용, `data`로 합침)·`type`·`immutable`, 키·1MiB 크기 검사, `kubernetes.io/tls`의 `tls.crt`·`tls.key` 확인. 컨테이너 `env[].valueFrom.secretKeyRef`와 `envFrom[].secretRef`(prefix·optional 포함)를 컨테이너를 만들 때 읽음 | **암호화하지 않음**(Kubernetes 기본값처럼 base64로 저장) — DB는 root만 읽게 둬. Secret 볼륨·ConfigMap·downward API 미구현. 값이 바뀌어도 이미 실행 중인 컨테이너는 다시 만들어질 때 새 값을 읽어. 표 출력과 개발용 trace에는 값이 나오지 않아 |
| readiness/liveness/startup probe | HTTP/HTTPS·TCP·exec 검사, Ready/Service 연동, 유예 종료·재시작 | alpha.4. gRPC·host override 미지원. [상태 검사](health-probes.md) |
| securityContext | 필드를 저장하고 경고 | 실행은 런타임 기본 보안 설정. 정책이 적용되지 않음 |
| nodeSelector, affinity, tolerations, priority | 필드를 저장하고 경고 | 노드 한 개, 해당 배치 정책 평가 없음 |
| 인증·인가 | 자체 CA 기반 TLS, 관리자·사용자 토큰, 네임스페이스별 RBAC, `kubectl auth can-i`·`whoami` | 사용자 토큰 발급은 관리자 전용. 서비스 계정 토큰 발급과 ClusterRole 자동 집계는 지원하지 않아. [접근 제어](access-control.md) |
| 패치 | JSON Patch, JSON Merge Patch, Strategic Merge Patch | Server-Side Apply 거절(415) |
| kubectl 출력 | JSON, Table, wide, watch와 셀렉터 전이 | API의 일부만 구현. 모든 kubectl 하위 명령을 지원하는 것은 아님 |
| OpenAPI | 지원하는 종류의 `fieldValidation` 정보를 담은 v3 문서. `kind: List` 파일을 kubectl이 클라이언트에서 검증할 때 읽는 v2는 정의가 없는 빈 문서 | 그래서 List 항목의 필드 검사는 서버가 해. 각 객체를 `fieldValidation=Strict`로 받아 검증해 |
| Protocol Buffers | 확인한 kubectl 요청의 일부 디코딩 | Protobuf 응답·전체 스키마 미지원. 미지원 요청은 거절 |

`dnsPolicy: ClusterFirst`를 저장해도 DNS 서버는 생기지 않아. Service를 만든 뒤 시작하는 Pod에는 `WEB_SERVICE_HOST` 같은 환경변수가 제공돼. 이미 시작한 컨테이너의 환경변수를 Service 변경 때 다시 쓰지는 않아. `sessionAffinity`도 저장만 하므로 연결마다 같은 Pod를 고르는 정책으로 쓰지 마.

실제로 확인한 kubectl은 Linux v1.36.6과 Windows v1.36.0이야. Linux API 검사 91개에 Windows 표본 12개를 더한 M1 결과가 103개야. Windows에서 Linux 검사 전체를 반복했다는 의미는 아니야. `exec`, `attach`, `port-forward`에 의존하지 않고 Service IP를 WSL에서 직접 확인해.

## 운영 경계

- **대역은 고정**: Pod `10.244.0.0/24`, Service `10.96.0.0/16`. 기존 경로와 겹치면 시작을 거절해. 전용 장치는 `sw-br0`, `sw-svc0`이고 컨테이너 네임스페이스는 `shearwater`야.
- **한 네트워크 공간에 한 인스턴스**: 다른 WSL 배포판도 같은 호스트 네트워크를 볼 수 있어. 두 번 설치해도 별도 클러스터가 되는 것은 아니야.
- **SQLite는 Linux 파일시스템**: 기본 `/var/lib/shearwater`. `/mnt/c`에서 데이터베이스를 운영하지 마. 소스 자체는 `/mnt/c`에 둘 수 있어.
- **재시작**: 기본은 `sw up`. alpha.4의 선택적 `sw service install`로 WSL 시작과 sw 오류 종료 후 자동 복구를 등록할 수 있어. Windows 로그인 때 WSL 자체를 시작하는 기능은 별도야.
- **서버 중단**: 실행 중인 앱 컨테이너는 계속 돌 수 있지만 API·상태 검사·자동 조정은 멈춰. alpha.5 이후에는 같은 호스트에서 sw만 재시작할 때 기존 커널 Service 전달 규칙과 연결 기록을 유지해. 중단 중 Pod가 실패하면 마지막 규칙이 그 Pod로 새 연결을 보낼 수 있어. 앱 충돌·Pod 강제 삭제·WSL/노드 재부팅이나 30초 종료 대기 한도를 넘는 연결까지 보존한다는 뜻은 아니야. [연결 유지 조건](connection-continuity.md)을 함께 봐. `sw down --all`은 앱도 내리고 DB·볼륨·로그는 보존해.

이 문서의 저장만 하는 필드 목록은 `internal/apis/grades.go`, 명령별 차이는 [운영 안내](operations.md), 구현 결정은 `design-stage1.md`에 근거해. 새 기능이 들어오면 코드·경고·표를 함께 바꿔야 해.

후속 기능은 [백업·복원 범위](backup-restore.md)와 [MCP·배포 계획의 범위](agent-mcp.md)를 봐.
MCP는 기본 조회 전용이고 로컬 관리자의 OS 권한을 격리하지 않아. 계획 배포는 JSON 생성/전체 교체이며
로컬에서 정확한 계획을 승인해야 해. 객체 여러 개의 변경을 한 트랜잭션으로 묶지는 않아.
공개 alpha.3의 동작과 alpha.4의 프로브 동작을 혼동하지 마.
