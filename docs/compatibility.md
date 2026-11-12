# 1단계 호환 범위

Shearwater는 Kubernetes API의 일부를 구현한 단일 노드 오케스트레이터야. API discovery에 표시되는 자원·동작과 아래 표가 계약 범위야. Kubernetes 전체 동작이나 CNCF 호환성 인증을 의미하지 않아.

## 필드를 받았다는 것과 실행한다는 것

| 범위 | 실제로 수행하는 일 | 제한 |
|---|---|---|
| Pod | containerd 실행, IPv4, 로그·직전 로그, 재시작 정책, 유예 삭제 | 단일 노드, CNI/CRI 미사용 |
| Deployment / ReplicaSet | 복제본 유지, RollingUpdate/Recreate, `/scale`, 이력·되돌리기 | `minReadySeconds`, `progressDeadlineSeconds`는 저장만 함 |
| Service / EndpointSlice | IPv4 ClusterIP 예약, Ready Pod 선택, 이름 포트, TCP 프록시 | DNS·UDP·NodePort·LoadBalancer 미구현, headless DNS 미제공 |
| 리소스 | CPU/메모리 limits 적용 | requests는 저장만 하고 배치에 쓰지 않음 |
| 볼륨 | emptyDir, hostPath와 type 검사·OrCreate 생성 | PVC/CSI 같은 영속 볼륨 체계 미구현 |
| readiness/liveness/startup probe | 필드를 저장하고 경고 | 프로브 실행 없음. Ready = 컨테이너 실행 중 |
| securityContext | 필드를 저장하고 경고 | 실행은 런타임 기본 보안 설정. 정책이 적용되지 않음 |
| nodeSelector, affinity, tolerations, priority | 필드를 저장하고 경고 | 노드 한 개, 해당 배치 정책 평가 없음 |
| 인증 | 자체 CA 기반 TLS, 로컬 관리자 토큰 | RBAC·사용자/서비스 계정 분리 없음 |
| 패치 | JSON Patch, JSON Merge Patch, Strategic Merge Patch | Server-Side Apply 거절(415) |
| kubectl 출력 | JSON, Table, wide, watch와 셀렉터 전이 | API의 일부만 구현. 모든 kubectl 하위 명령을 지원하는 것은 아님 |
| Protocol Buffers | 확인한 kubectl 요청의 일부 디코딩 | Protobuf 응답·전체 스키마 미지원. 미지원 요청은 거절 |

`dnsPolicy: ClusterFirst`를 저장해도 DNS 서버는 생기지 않아. Service를 만든 뒤 시작하는 Pod에는 `WEB_SERVICE_HOST` 같은 환경변수가 제공돼. 이미 시작한 컨테이너의 환경변수를 Service 변경 때 다시 쓰지는 않아. `sessionAffinity`도 저장만 하므로 연결마다 같은 Pod를 고르는 정책으로 쓰지 마.

실제로 확인한 kubectl은 Linux v1.36.6과 Windows v1.36.0이야. Linux API 검사 91개에 Windows 표본 12개를 더한 M1 결과가 103개야. Windows에서 Linux 검사 전체를 반복했다는 의미는 아니야. `exec`, `attach`, `port-forward`에 의존하지 않고 Service IP를 WSL에서 직접 확인해.

## 운영 경계

- **대역은 고정**: Pod `10.244.0.0/24`, Service `10.96.0.0/16`. 기존 경로와 겹치면 시작을 거절해. 전용 장치는 `sw-br0`, `sw-svc0`이고 컨테이너 네임스페이스는 `shearwater`야.
- **한 네트워크 공간에 한 인스턴스**: 다른 WSL 배포판도 같은 호스트 네트워크를 볼 수 있어. 두 번 설치해도 별도 클러스터가 되는 것은 아니야.
- **SQLite는 Linux 파일시스템**: 기본 `/var/lib/shearwater`. `/mnt/c`에서 데이터베이스를 운영하지 마. 소스 자체는 `/mnt/c`에 둘 수 있어.
- **재시작은 수동**: 부팅 뒤 containerd가 준비되면 같은 경로와 옵션으로 `sw up`을 실행해. OS 부팅 시 자동 실행 서비스 설치는 포함하지 않아.
- **서버 중단**: 실행 중인 앱 컨테이너는 계속 돌 수 있지만 API·Service 프록시·자동 조정은 멈춰. `sw down --all`은 앱도 내리고 DB·볼륨·로그는 보존해.

이 문서의 저장만 하는 필드 목록은 `internal/apis/grades.go`, 명령별 차이는 [운영 안내](operations.md), 구현 결정은 `design-stage1.md`에 근거해. 새 기능이 들어오면 코드·경고·표를 함께 바꿔야 해.
