> 공개 준비 과정에서 개인 경로·내부 세션 식별자·로컬 앱 이름을 일반화했어. 측정값과 당시 검증 범위는 유지했고, `<private-evidence>`는 공개 저장소에 없는 원본 기록을 뜻해.

# Shearwater 1단계 설계

- 작성: 2026-11-10 · 상태: **3차 초안. 결정 6개와 검토 회의 1차(`docs/meetings/2026-11-10-design-review.md`) 반영, 구현은 사용자 동의 뒤에 시작**
- 범위: 1단계(단일 노드에서 kubectl로 Deployment가 뜨고, 죽으면 다시 뜨는 데까지)
- 이름: 프로젝트 `Shearwater`, 바이너리 `sw`. 폴더는 아직 `<workspace>/shearwater`다(세션 중에 작업 폴더 이름을 바꾸면 세션이 끊겨서 다음 세션 시작 전에 바꾼다).
- 표기: 출처는 `[S1]`처럼 달고 맨 아래에 모았다. `[R1]`은 검토 회의 1차에서 나온 것으로, 코덱스가 쿠버네티스 v1.36.6 소스를 읽고 제시했거나 딥시크가 지적한 내용이다. 클로드가 그 소스를 직접 확인한 것은 아니라서 구현할 때 다시 본다. 공식 문서로 확인하지 못하고 기억에 의존한 항목은 **(미검증)** 으로 표시하고 확인 방법을 적었다.

## 0. 한 줄 정의와 한계

AI 에이전트가 직접 운영하는 단일 바이너리 컨테이너 오케스트레이터. 쿠버네티스 API의 작은 부분(Pod, Deployment, Service)과 호환된다.

1~3단계의 결과물은 **"한 대에서 잘 죽지 않는 서비스"** 다. 노드가 하나라서 그 노드나 디스크가 죽으면 서비스도 죽는다. 고가용성이라는 말은 4단계(Raft 다중 노드)를 끝내기 전에는 쓰지 않는다.

## 0.1 핵심 원칙: 경량화와 사용자 편의성

사용자가 정한 프로젝트의 핵심이다(2026-11-10). 설계에서 갈리는 선택은 이 둘로 판단한다. "더 가벼운 k8s"라는 문구로 다른 제품과 경쟁하지 않는다는 방침은 그대로이고, 경량화는 광고 문구가 아니라 설계 제약으로 쓴다.

**경량화** (규칙)

- 유휴 상태에서는 아무것도 하지 않는다. 주기 작업을 최소로 두고, 상태가 바뀌지 않으면 저장소에 쓰지 않는다. 내용이 같은 status는 쓰지 않고, 북마크 때문에 리비전을 새로 만들지 않고, 변경 로그 정리는 실제 쓰기에 묶어서 한다. 주기적으로 도는 DB 작업을 따로 두지 않는다 [R1].
- Shearwater가 추가로 띄우는 상주 프로세스는 `sw server` 하나다. etcd, 별도 프록시 데몬, Pod마다 붙는 pause 컨테이너가 없다.
- 사용자가 따로 설치해야 하는 것을 늘리지 않는다. 의존 모듈도 늘리지 않는다.
- 수치는 재서 적는다. 재지 않은 수치는 문서에 쓰지 않는다.

**경량화** (목표. M6의 WSL 실측값과 측정 조건은 [README](../README.md#실제-리소스-사용량)에 있다)

| 항목 | 목표 |
|---|---|
| 유휴 1분 동안 `sw server`의 디스크 쓰기 | 0회. 시작 직후 정리가 끝난 안정 상태에서, API 호출 수가 아니라 DB·WAL·로그 파일에 실제로 쓴 횟수로 잰다 [R1] |
| 유휴 CPU | 0에 가깝게(측정값 기록) |
| 메모리(RSS), Pod 10개를 돌리는 유휴 상태 | 50MB 이하 |
| 바이너리 크기 | 40MB 이하 |
| 시작부터 `/readyz`까지(이미지 받기 제외) | 1초 이내 |
| Shearwater가 Pod마다 추가하는 프로세스 | 0개 |

**사용자 편의성** (사용자는 둘이다: 운영하는 에이전트와, 주인인 사람)

- 설치는 바이너리 하나, 시작은 명령 하나(`sw up`)다. 인증서, kubeconfig, 네트워크 설정, 디렉터리는 `sw up`이 만든다. 설정 파일 없이 기본값으로 돈다.
- 준비가 안 된 것이 있으면 무엇이 없고 어떻게 고치는지 한 줄씩 알려 준다(`sw doctor`). 추측해서 진행하지 않는다.
- 사람은 평소 쓰던 kubectl을 그대로 쓴다. `sw up`이 kubeconfig 경로와 쓰는 법을 한 줄로 출력한다.
- 지원하지 않는 것은 조용히 무시하지 않고, 무엇이 안 되고 대신 무엇을 쓰면 되는지 알려 준다(3.3).
- 오류는 코드, 다시 시도해도 되는지, 다음에 할 일을 같이 준다(8.3).
- 지우는 법도 명령 하나다. Shearwater가 만든 경로 목록을 `sw doctor`가 보여 준다.
- README의 시작하기는 명령 5줄 이내로 끝낸다.

이 원칙으로 기존 설계를 점검해서 바꾼 것:

| 바꾼 것 | 전 | 후 | 이유 |
|---|---|---|---|
| 노드 하트비트 | 10초마다 저장소에 기록 | 상태가 바뀔 때만 기록 | 10초마다 디스크 쓰기와 watch 이벤트가 생긴다. 단일 노드에는 필요 없다 |
| 컨트롤러 전체 재확인 | 30초 | 5분 | watch가 프로세스 안에서 돌아 이벤트를 잃지 않는다. 안전망 용도면 충분하다 |
| 노드 에이전트의 전체 재확인 | 5초 | 30초 | 종료는 containerd 알림으로 바로 안다. 폴링은 안전망이다 |
| 시작 방법 | `sw server`만 | `sw up` / `sw down` 추가 | 명령 하나로 시작한다 |
| Pod 네트워크 | CNI 플러그인 호출 | 직접 구현(결정 6) | 사용자가 따로 설치할 것이 containerd뿐이 된다. Pod를 띄울 때 플러그인 실행 파일을 여러 번 부르지 않는다 |

알고도 남겨 둔 것: 서비스 프록시는 사용자 공간에서 돈다. 유휴 비용은 0이지만 트래픽이 많으면 커널 NAT보다 무겁다. 규칙을 만들고 지울 일이 없어 단순하고 관찰하기 쉬워서 1단계에서는 유지한다. M6에서 처리량을 재서 적는다.

## 1. 환경 확인 결과 (2026-11-10 실측)

| 항목 | 상태 |
|---|---|
| WSL | Ubuntu 24.04, WSL2, systemd 켜짐, cgroup v2, 12코어, 메모리 3.9GB, 디스크 여유 946GB |
| Go | **없음** (apt 후보는 1.22로 오래됨) |
| containerd | 2.3.3 있음. Docker가 쓰는 인스턴스(systemd 서비스, 소켓 `/run/containerd/containerd.sock`, root 전용). CRI 플러그인은 꺼져 있다 |
| runc | 1.4.3 있음 |
| kubectl | WSL에는 **없음**. 윈도우에 v1.36.0(chocolatey) |
| CNI 플러그인 | **없음** (`/opt/cni/bin` 없음) |
| 기타 | git 2.43 있음. gcc·make·sqlite3 CLI 없음. iptables·nft·ip 있음 |
| 권한 | 사용자 `developer`. `sudo`는 비밀번호가 필요해서 에이전트가 못 쓴다 |

설계에 미치는 영향:

- gcc가 없으니 cgo를 쓰지 않는다. SQLite는 순수 Go 드라이버(`modernc.org/sqlite`)를 쓴다. 정적 바이너리 하나라는 목표와도 맞는다.
- SQLite 파일은 WSL의 ext4(`/var/lib/shearwater`)에 둔다. `/mnt/c`(9p)는 파일 잠금이 불안정하다. 소스만 윈도우 쪽에 둔다.
- 컨테이너·네트워크 조작에는 root가 필요하다(결정 3).

## 2. 결정 기록 (2026-11-10, 사용자 선택)

| # | 결정 | 선택 | 버린 안 |
|---|---|---|---|
| 1 | API 타입과 패치 | **직접 작성.** `k8s.io/*` 라이브러리를 가져오지 않는다 | `k8s.io/api`·`apimachinery` 사용 |
| 2 | containerd 연동 | **네이티브 Go 클라이언트로 Docker의 containerd를 같이 쓴다.** containerd 네임스페이스 `shearwater`로 분리 | CRI + 전용 containerd 인스턴스 |
| 3 | root와 설치 | **`wsl.exe -d Ubuntu-24.04 -u root` 허용, 설치 승인.** root는 CNI 플러그인 설치와 `sw server` 실행에만 쓴다 | 사용자가 매번 sudo 실행 |
| 4 | 이름 | **Shearwater** | agentkube, coxswain, pilotboat |
| 5 | `kubectl create deployment`의 Protobuf 본문 | **요청 본문용 Protobuf 디코더를 직접 작성** (kubectl 실측 뒤에 추가된 결정) | 415로 거절하고 매니페스트만 지원 |
| 6 | Pod 네트워크 | **직접 구현.** CNI 플러그인을 쓰지 않는다 (핵심 원칙이 나온 뒤에 추가된 결정) | CNI 플러그인(bridge 등) 호출 |

결정 1·2·5·6으로 직접 만들 것이 늘었다. 무엇이 늘었는지는 3.6절과 7절에 적었고, 구현 순서(10절)의 M1·M2가 그만큼 커졌다.

설치 목록(승인됨. safe-ops 규칙: host-lock을 잡고 한 번에 하나씩, 삭제 없음, 기존 파일 덮어쓰지 않음):

| # | 무엇 | 어디에 | root | 비고 |
|---|---|---|---|---|
| 1 | Go 최신 안정판 linux-amd64 tarball | `~/.local/go` | 아니오 | go.dev/dl에서 버전 확인, sha256 검증. `~/.profile`은 고치지 않고 `hack/env.sh`에서 PATH 지정 |
| 2 | kubectl v1.36.x linux 바이너리 | `~/.local/bin/kubectl` | 아니오 | dl.k8s.io, sha256 검증. e2e 스크립트용 |
| 3 | CNI 플러그인(containernetworking/plugins 릴리스) | `/opt/shearwater/cni/bin` | **예** | 시스템 공용 `/opt/cni/bin`이 아니라 전용 경로. sha256 검증. **결정 6으로 쓰지 않게 됐다** |

**설치 결과 (2026-11-10 완료, 실측):** Go 1.27.3, kubectl v1.36.6, CNI 플러그인 v1.10.0. 셋 다 sha256 검증을 통과했다. 내려받은 원본은 `~/.cache/shearwater-dl`에 남겨 뒀다. 확인한 것: 임시 모듈 `go run` 성공, `kubectl version --client`, `bridge` 플러그인의 `CNI_COMMAND=VERSION` 응답(1.1.0까지 지원), root로 `ctr -n shearwater version`(서버 2.3.3 응답). 설치 전 containerd 네임스페이스는 `moby`, `moby_history` 둘이었다. 빌드할 때는 `. hack/env.sh`로 PATH를 잡는다.

CNI 플러그인은 결정 6 이전에 설치한 것이라 `/opt/shearwater/cni/bin`에 남아 있다. Shearwater는 이 경로를 쓰지 않는다. 지우지 않고 그대로 뒀고, 치울지는 사용자가 정한다(치울 때는 safe-ops 격리로).

apt 설치는 없다. Docker와 containerd의 설정 파일은 고치지 않는다. 서버가 실행 중에 만드는 경로는 `/var/lib/shearwater`, `/run/shearwater`, `/etc/shearwater`, `/var/log/shearwater`다. 로그 상위 경로가 쓰기 권한 검사에 맞지 않으면 기본 로그를 `/var/lib/shearwater/log`에 둔다.

## 3. 지원할 API

호환 기준 클라이언트: kubectl v1.36.0. 문서는 v1.37 기준으로 확인했다.

### 3.1 엔드포인트

| 종류 | 경로 | 비고 |
|---|---|---|
| 버전·상태 | `/version`, `/healthz`, `/readyz`, `/livez` | |
| discovery | `/api`, `/api/v1`, `/apis`, `/apis/apps/v1`, `/apis/discovery.k8s.io/v1` | 비집계 형식. `Accept` 헤더로 집계 형식을 따로 요청하지 않으면 비집계 문서가 기본 응답이다 [S2]. 집계 형식은 1단계에서 안 한다 |
| OpenAPI | `/openapi/v3`, `/openapi/v2` | 최소 문서. 3.5 참고 |
| core/v1 | `namespaces`, `nodes`, `pods`, `pods/status`, `pods/log`, `services`, `events` | |
| apps/v1 | `deployments`, `deployments/status`, `deployments/scale`, `replicasets`, `replicasets/status`, `replicasets/scale` | `/scale`은 GET·PUT·PATCH. discovery의 `*/scale` 항목에는 kind `Scale`과 함께 group `autoscaling`, version `v1`을 적는다 [R1] |
| 전체 네임스페이스 | `/api/v1/pods`, `/api/v1/services`, `/api/v1/events`, `/apis/apps/v1/deployments`, `/apis/apps/v1/replicasets`, `/apis/discovery.k8s.io/v1/endpointslices` | list·watch만(`kubectl get -A`) [R1] |
| discovery.k8s.io/v1 | `endpointslices` | 읽기 전용(컨트롤러가 씀) |

### 3.2 동사

| 리소스 | get | list | watch | create | update | patch | delete |
|---|---|---|---|---|---|---|---|
| pods, services, deployments, replicasets | O | O | O | O | O | O | O |
| namespaces | O | O | O | O | - | - | 비어 있을 때만(아니면 409) |
| nodes, events, endpointslices | O | O | O | - | - | - | - |

- **list**: `labelSelector`(등호·집합 모두), `fieldSelector`는 `metadata.name`, `metadata.namespace`, `spec.nodeName`, `status.phase`, `involvedObject.*`만. `limit`은 무시하고 전체를 돌려주며 `continue`는 빈 값이다.
- **watch**: `?watch=1`과 `?watch=true`를 모두 받는다 [R1]. `resourceVersion=N`, `resourceVersionMatch`, `allowWatchBookmarks`, `timeoutSeconds`, `sendInitialEvents`. 셀렉터가 걸린 watch의 이벤트 종류는 5절의 전이 규칙을 따른다. 스트림 도중에 보관 범위를 벗어나면 `ERROR` 이벤트(Status 410)를 보내고 끝낸다 [R1]. 보관 범위보다 오래된 버전을 요청하면 `410 Gone`을 돌려준다. 클라이언트는 410을 받으면 다시 list한 뒤 watch해야 한다 [S1].
- **update(PUT)**: 요청의 `resourceVersion`이 낡았으면 `409 Conflict` [S1].
- **patch**: `application/merge-patch+json`, `application/strategic-merge-patch+json`, `application/json-patch+json`. `application/apply-patch+yaml`(server-side apply)은 `415`로 거절한다 [S1][S10]. `kubectl apply`의 기본은 클라이언트 쪽 apply이고 `last-applied-configuration` 주석과 패치로 동작한다 [S12].
- **delete**: 옵션은 본문의 DeleteOptions에서 읽는다. `gracePeriodSeconds`(기본 30초 [S3]), `propagationPolicy`는 `Background`(기본)·`Foreground`·`Orphan` [S7], 전제조건 `uid`·`resourceVersion`은 삭제와 같은 트랜잭션에서 검사한다. 삭제가 시작되면 MODIFIED(`deletionTimestamp`), 끝나면 DELETED 이벤트가 나간다. 응답에 객체의 `uid`가 들어 있어야 kubectl의 삭제 대기가 같은 이름으로 다시 만든 객체를 기다리지 않는다 [R1].
- **logs**: JSON이 아니라 텍스트 스트림이다. `container`, `follow`, `previous`, `tailLines`, `limitBytes`를 지원한다. `timestamps`, `sinceSeconds`, `sinceTime`은 로그에 시각이 없어서 1단계에서 지원하지 않고, 조용히 무시하지 않고 거절하면서 `tailLines`를 쓰라고 알려 준다 [R1].
- **status 쓰기**: `/status`로 쓰는 것은 `generation`을 올리지 않는다 [R1].
- **스케일**: Deployment·ReplicaSet의 `/scale`은 `autoscaling/v1 Scale`로 원하는 수, 현재 수, 라벨 셀렉터를 보여 준다. PUT·PATCH는 부모의 `spec.replicas`만 바꾼다. `uid`·`resourceVersion` 전제조건 검사와 저장을 같은 트랜잭션에서 한다. 수가 그대로면 리비전과 generation도 그대로다.
- **그 밖의 쿼리**: `fieldManager`는 받기만 하고 저장하지 않는다(kubectl이 보낸다, 실측). Table의 `includeObject`는 `Metadata`(기본)·`Object`·`None`을 받는다 [R1].
- **공통 쿼리**: `dryRun=All`은 모든 쓰기 동사에서 지원한다(저장만 건너뛰고 최종 객체를 돌려준다 [S1]). `fieldValidation`은 `Strict`·`Warn`·`Ignore`를 지킨다 [S1].
- **표 출력**: `Accept: application/json;as=Table;g=meta.k8s.io;v=v1`이면 Table로 응답한다 [S1]. kubectl의 `READY`, `STATUS`, `RESTARTS`, `AGE` 같은 열은 서버가 만든다. pods, deployments, replicasets, services, nodes, namespaces, events, endpointslices에 구현한다.
- **직렬화**: 응답은 JSON만. 요청은 JSON과, `kubectl create <종류>`가 보내는 Protobuf 본문(결정 5, 3.6)을 받는다.
- **오류**: 모든 오류는 `kind: Status` 본문(`reason`, `code`, `details`)으로 돌려준다.
- 하지 않는 동사: `deletecollection`, `exec`, `attach`, `port-forward`, `proxy`.

### 3.3 필드 범위: 네 가지 경우

조용히 무시되는 필드가 가장 위험하다. 그래서 모든 필드는 아래 넷 중 하나로 처리된다.

- **지원**: 동작한다.
- **저장만**: 받아서 저장하지만 효과가 없다. 응답에 `Warning` 헤더를 달아 kubectl과 `sw`가 보여 준다. M1 구현: 생성할 때, 그리고 수정·패치가 그 필드를 새로 넣거나 값을 바꿀 때만 경고한다(안 건드린 필드로 매번 경고하지 않는다). kubectl이 스스로 넣는 기본값(`dnsPolicy: ClusterFirst`, `sessionAffinity: None`)은 경고하지 않는다.
- **거부**: 쿠버네티스에는 있지만 Shearwater가 지원하지 않아서, 그대로 받으면 기대와 다르게 도는 필드. `422`와 함께 어느 필드가 왜 안 되는지 알려 준다.
- **모르는 필드**: 위 세 목록 어디에도 없는 필드. `fieldValidation` 규칙대로 한다. `Strict`면 400으로 거절, `Warn`이면 경고하고 버림, `Ignore`면 버림 [S1]. 쿼리가 없으면 서버 기본값은 `Warn`이다 [S1]. kubectl의 `create -f`·`apply -f`는 `Strict`를 붙여 보내고, `run`·`expose`는 쿼리를 안 보낸다(실측).
- **두 번 나온 필드**(같은 JSON 키의 중복)도 같은 규칙이다. `Strict`면 400(`duplicate field "spec.replicas"`), `Warn`이면 경고하고 마지막 값을 쓴다, `Ignore`면 조용히 마지막 값을 쓴다. 타입과 무관하게 본문 전체를 보므로 통째로 저장하는 값(affinity, 프로브) 안쪽도 본다. POST·PUT 본문, PATCH의 패치 문서(세 형식 모두), `/status` 쓰기의 `status` 아래에 적용한다. `fieldValidation` 값 자체가 Strict·Warn·Ignore가 아니면 본문과 무관하게 400이다.

| 리소스 | 지원 | 저장만(경고) | 거부(422) |
|---|---|---|---|
| Pod | `containers[]`의 `name, image, command, args, env(value), ports, workingDir, imagePullPolicy, resources.limits(cpu·memory)`, `restartPolicy`, `terminationGracePeriodSeconds`, `nodeName`, `volumes`의 `emptyDir`·`hostPath`와 `volumeMounts` | `resources.requests`, `dnsPolicy`, `livenessProbe`·`readinessProbe`·`startupProbe`(3단계에서 지원), `affinity`, `tolerations`, `nodeSelector`, `priorityClassName`, `securityContext` | `initContainers`, `hostNetwork: true`, ConfigMap·Secret을 참조하는 `env.valueFrom`·`envFrom`·`volumes`, 그 밖의 볼륨 종류, `serviceAccountName` 지정 |
| Deployment | `replicas`, `selector`(matchLabels·matchExpressions), `template`, `strategy`(RollingUpdate의 `maxSurge`·`maxUnavailable`, Recreate), `revisionHistoryLimit`, `paused` | `minReadySeconds`, `progressDeadlineSeconds` | 생성 뒤 `selector` 변경(apps/v1에서 불변 [S4]), `selector`가 `template.metadata.labels`와 안 맞는 경우 [S4] |
| Service | `type: ClusterIP`, `selector`, `ports[]`(`port`, `targetPort` 숫자·이름, `protocol: TCP`), `clusterIP` 지정 | `sessionAffinity`, `clusterIP: None`(DNS가 없는 1단계에서는 쓸 곳이 없다. 3단계에서 지원) [R1] | `type: NodePort`·`LoadBalancer`·`ExternalName`(3단계에서 NodePort), `protocol: UDP`, 범위를 벗어난 `clusterIP`(쿠버네티스도 422 [S6]) |

status 필드:

- Pod: `phase`(Pending·Running·Succeeded·Failed의 정의는 [S3]를 따른다), `conditions`(PodScheduled, Initialized, ContainersReady, Ready), `containerStatuses`(`state`, `lastState`, `restartCount`, `ready`, `image`, `containerID`), `podIP`, `hostIP`, `startTime`.
- Deployment: `observedGeneration`, `replicas`, `updatedReplicas`, `readyReplicas`, `availableReplicas`, `unavailableReplicas`, `conditions`(Available, Progressing). `kubectl rollout status`가 읽는 값이다 [S4].
- 1단계에서 "Ready"는 **컨테이너가 실행 중**이라는 뜻이다. 프로브가 없으므로 앱이 실제로 요청을 받는지는 모른다. 문서와 출력에 그렇게 적는다.

### 3.4 메타데이터 규칙

- `resourceVersion`: 클러스터 전체에서 하나씩 늘어나는 정수. 클라이언트는 불투명한 문자열로 다룬다 [S1].
- `uid`, `creationTimestamp`, `generation`(spec이 바뀔 때 증가. **Deployment만 예외로 `metadata.annotations`가 바뀌어도 증가한다**: Deployment의 주석은 ReplicaSet으로 복사되므로 컨트롤러가 변경을 알아야 한다. labels만 바뀌면 안 오르고, `/status` 쓰기는 올리지 않는다. ReplicaSet·Pod·Service는 spec만 본다. Deployment의 규칙은 코덱스가 쿠버네티스 v1.36.6 `pkg/registry/apps/deployment/strategy.go`의 `PrepareForUpdate`와 대조해 확인했다(2026-11-11). 다른 종류의 규칙은 소스로 확인하지 않았다 **(미검증)**. 내용이 같으면 저장소가 쓰지 않는 규칙은 그대로다), `generateName`(뒤에 임의 5글자), `ownerReferences`, `finalizers`, `deletionTimestamp`, `labels`, `annotations`.
- `managedFields`는 만들지 않는다(server-side apply를 안 하므로).

### 3.5 kubectl 호환에서 걸리는 곳

2026-11-10에 kubectl v1.36.6을 가짜 서버에 붙여 실제 요청을 기록했다. 전체 기록은 `docs/kubectl-trace.md`. 아래에서 **(실측)** 은 그 기록으로 확인한 것이다.

1. **OpenAPI가 없으면 `apply -f`와 `create -f`가 실패한다 (실측).** kubectl은 `/openapi/v3`와 그룹버전별 문서를 읽어, 해당 종류의 patch 연산에 `fieldValidation` 파라미터가 있는지 본다. 있으면 `fieldValidation=Strict`를 붙여 요청한다. 그래서 `/openapi/v3`에 지원하는 종류마다 이 정보만 담은 최소 문서를 낸다. `/openapi/v2`는 404여도 된다 (실측).
2. **`rollout status`는 watch-list를 쓴다 (실측).** `sendInitialEvents=true&resourceVersionMatch=NotOlderThan`로 watch를 연다. 현재 객체를 ADDED로 보내고, 끝 표시 북마크를 보낸 뒤 이어서 스트림을 보낸다 [S1]. 끝 표시 북마크의 정확한 모양은 구현할 때 [S1]의 예시와 실제 kubectl로 확인한다.
3. **Table을 안 주면** kubectl은 이름과 나이만 보여 준다. `get`은 Table을 먼저 요청한다 (실측).
4. **`kubectl delete`는 `{"propagationPolicy":"Background"}`를 본문으로 보내고 (실측) 기본으로 삭제 완료를 기다린다.** 객체가 실제로 사라질 때 DELETED 이벤트가 나가야 한다.
5. **`kubectl scale`은 `/scale` 하위 리소스에 merge patch를 보낸다 (실측).** 본문은 `{"spec":{"replicas":3}}`. `--current-replicas`를 붙이면 GET으로 개수를 읽고 `uid`·`resourceVersion`을 넣은 JSON PUT을 보낸다(M4 실측).
6. **평문 HTTP에서는 kubectl이 토큰을 보내지 않는다 (실측).** 자체 서명 CA로 TLS를 켜고 토큰을 넣은 kubeconfig를 만들어 준다(`sw kubeconfig`).
7. **`apply`의 두 번째부터는 strategic merge patch다 (실측).** `$setElementOrder/...` 지시어가 실제로 온다. 3.6 참고.
8. **`kubectl create deployment`는 Protobuf 본문을 보내고, 415를 받아도 JSON으로 바꾸지 않는다 (실측).** `run`과 `expose`는 JSON이다 (실측). 결정 5에 따라 요청 본문용 Protobuf 디코더를 직접 짠다(3.6). 응답은 JSON으로 준다. 요청의 Accept에 JSON이 들어 있다 (실측).
9. **`run`과 `expose`의 본문에는 `dnsPolicy`, `resources: {}`, `status: {}`가 들어 있다 (실측).** `dnsPolicy`는 저장만 등급으로 받고, 생성 요청의 `status`는 버린다.
10. **`describe`는 events를 `involvedObject.name`, `involvedObject.namespace`, `involvedObject.uid`로 거른다 (실측).** 응답은 JSON만으로 된다 (실측).

### 3.6 직접 구현하는 것 (결정 1)

`k8s.io/*`를 가져오지 않으므로 아래를 전부 직접 짠다.

- **타입**: 지원·저장만 등급의 필드만 가진 자체 구조체. 디코드할 때 구조체에 없는 필드를 경로와 함께 모아서 3.3의 "모르는 필드" 규칙에 넘긴다.
- **기본값 채우기**: `restartPolicy: Always`, `terminationGracePeriodSeconds: 30` [S3], `imagePullPolicy`, `protocol: TCP`, Deployment의 `replicas: 1`·`strategy: RollingUpdate 25%/25%`·`revisionHistoryLimit: 10` [S4], Service의 `type: ClusterIP`·`targetPort = port` [S6].
- **값 형식**: 라벨 셀렉터 문법(등호·집합), 수량(`500m`, `256Mi`), 정수 또는 문자열(`maxSurge: 1` / `"25%"`), 시간(RFC 3339).
- **JSON merge patch**(RFC 7386)와 **JSON patch**(RFC 6902).
- **strategic merge patch**: 리스트를 통째로 바꿀지, 키로 병합할지를 필드마다 정한 규칙 [S11].
  - 병합 키 표(우리가 지원하는 필드만): `containers` → `name` [S11], `containers[].env` → `name`, `containers[].ports` → `containerPort`, `containers[].volumeMounts` → `mountPath`, `volumes` → `name`(병합에 더해 `retainKeys`: 볼륨 종류를 바꾸면 이전 종류를 지운다), Service `ports` → `port`, `ownerReferences` → `uid`, `conditions` → `type`, `metadata.finalizers` → 키 없는 값 목록의 병합. `tolerations`는 통째로 교체 [S11]. ports의 키는 protocol과 묶은 복합키가 아니다. 코덱스가 소스와 대조해 맞다고 확인했고 `finalizers`와 `volumes`의 `retainKeys`를 추가했다 [R1]. 구현할 때 쿠버네티스 소스의 구조체 태그(`patchStrategy`, `patchMergeKey`)를 직접 읽어 다시 맞춘다.
  - 지시어: `$patch: delete|replace`, `$setElementOrder/<필드>`, `$deleteFromPrimitiveList/<필드>`, `$retainKeys`. 뜻 [R1]: `$setElementOrder`는 순서만 정한다. 패치에 없는 서버 쪽 원소를 지우라는 뜻이 아니다. `$retainKeys`는 목록 밖의 기존 키를 지우고, 패치에 든 키가 목록 안에 있는지 검증한다. `$patch: merge`처럼 원본에 없는 지시어는 만들지 않는다.
  - 적용 순서 [R1]: 서버는 3-way 비교를 다시 하지 않는다. kubectl이 보낸 패치를 현재 객체에 적용한다. 읽기 → 패치 → 기본값·전체 검증 → 조건부 커밋을 한 트랜잭션 경계로 묶는다. 지시어를 처리한 뒤에 "모르는 필드" 검사를 한다.
  - 검증: 테스트 자료를 구현보다 먼저 고정한다 [R1]. null·누락·빈 목록 구분, 추가+삭제+순서 변경을 한 요청에, 서버가 넣은 원소 보존, 같은 병합 키가 중복된 입력, finalizer 삭제, 볼륨 종류 교체. 그리고 kubectl을 기준으로 한 e2e(컨테이너 추가·삭제, 환경변수 삭제, 순서 변경, 전략 종류 변경을 `kubectl apply`로 하고 결과 객체를 확인).

- **Protobuf 요청 디코더** (결정 5): 요청 본문만 읽는다. 쓰지는 않는다. 대상은 Deployment, Service, Pod, Namespace다. 그 밖의 `kubectl create <종류>`는 지원한다고 쓰지 않는다.
  - 봉투 [R1]: 바이트 `6b 38 73 00` 뒤에 `runtime.Unknown` 메시지. 1=TypeMeta(1=apiVersion, 2=kind), 2=원본 바이트, 3=contentEncoding, 4=contentType. 원본 바이트가 실제 객체다. contentType 필드는 보통 비어 있으므로 그것으로 형식을 판단하지 않는다. HTTP Content-Type, 표식, 봉투의 종류, URL의 리소스·버전이 서로 맞는지 검증한다.
  - 번호 표 [R1]: 쿠버네티스 v1.36.6 태그의 `generated.proto`에서 옮겨 적는다. core/v1, apps/v1, meta/v1에 더해 runtime, intstr, resource의 것도 필요하다. 표마다 원본 파일과 태그를 주석으로 남긴다. 번호는 메시지 안에서만 의미가 있다. 모듈을 가져오는 것이 아니라 번호를 옮겨 적는 것이다.
  - 범위 [R1]: 루트 4종으로 끝나지 않는다. Deployment → PodTemplate → PodSpec → Container·Volume, ObjectMeta, 셀렉터, 수량이 이어진다. "저장만" 등급 필드(affinity, 프로브, securityContext 등)도 보존하려면 그 메시지의 표가 필요하다. 공통 wire 리더와 메시지별 표를 나눈다.
  - **"있었는지"를 보존한다** [R1]: kubectl은 값이 0이거나 빈 필드도 보낸다(`paused=false`, `minReadySeconds=0`). 반대로 `replicas=0`, `enableServiceLinks=false`, 길이 0인 `emptyDir`처럼 비어 보이지만 뜻이 있는 값도 있다. 그래서 "값이 비면 버린다"는 규칙은 쓰지 않는다. 아는 필드는 값과 "요청에 있었는지"를 따로 기억한다. kubectl이 자동으로 넣는 기본 필드는 "생략과 같다고 확인한 것"만 명시적인 표로 허용한다. 표에 없는 번호는 값만 보고 빈 값인지 알 수 없으므로 "메시지 이름 + 필드 번호"와 함께 거절한다.
  - 값 모양 [R1]: Time은 seconds=1, nanos=2이고 빈 메시지는 "시각 없음"이다(1970년으로 만들지 않는다). 정수 또는 문자열은 type=1(0 정수, 1 문자열), intVal=2, strVal=3. 수량은 안쪽 메시지의 문자열(`500m`). labels(11)·annotations(12)는 key=1/value=2 항목의 반복이고 빈 값도 유효하다.
  - 리더의 방어 [R1]: 길이·재귀 깊이·varint 넘침 제한, 음수 정수, 순서가 바뀐 필드, 중복 필드(스칼라는 마지막 값, 메시지는 병합, 반복 필드는 추가).
  - 디코드한 결과는 JSON 경로와 같은 자체 구조체로 들어가므로 기본값·검증·저장은 한 벌이다.
  - 검증: 가짜 서버로 받아 둔 실제 kubectl 본문(`create deployment`, `create service clusterip`, `create namespace`. 뒤의 둘은 M0에서 캡처해 Protobuf인지부터 확인)을 테스트 자료로 넣고, 같은 내용을 `create -f`로 만든 객체와 결과가 같은지 비교한다. kubectl 1.36.0(윈도우)과 1.36.6 둘 다로 캡처한다.
  - M1 구현: 본문을 "같은 객체의 JSON"으로 바꿔 JSON 경로에 그대로 넘긴다(`internal/protobuf`). 표에는 대상 메시지의 모든 필드 번호를 적었고, Go 타입에서 포인터인 필드만 "있었는지"를 보존한다. 프로브·affinity·securityContext 같은 저장만 등급의 **메시지** 필드는 표를 만들지 않았고 본문에 있으면 거절한다(`kubectl create <종류>`와 `debug --copy-to`는 보내지 않는다). Pod 본문은 `kubectl debug POD --copy-to`가 보낸다(실측).
  - 한계: kubectl이 다른 버전에서 새 필드를 보내기 시작하면 거절된다. 조용히 틀리는 것보다 낫다고 보고, 오류에 "매니페스트로 `apply -f`를 쓰라"는 안내를 넣는다. 서버가 kubectl의 요청 형식을 JSON으로 바꾸게 할 방법은 없다 [R1].

위험: kubectl이 보내는 패치 중 구현하지 않은 모양이 있으면 apply가 틀린 결과를 낼 수 있다. 모르는 지시어를 만나면 조용히 넘기지 않고 `422`로 거절한다.

## 4. 프로세스 구조

```
sw server (root, 프로세스 1개)
 ├─ API 서버        HTTPS 127.0.0.1:6443, kubectl·sw·MCP가 붙는 곳
 ├─ 저장소          SQLite + watch 허브
 ├─ 컨트롤러        deployment, replicaset, endpointslice, gc
 ├─ 스케줄러        nodeName이 빈 Pod를 노드에 묶는다
 ├─ 노드 에이전트   kubelet 역할. containerd로 컨테이너를 맞춘다
 └─ 서비스 프록시   ClusterIP:port → Pod IP:port
        │ containerd API (gRPC, /run/containerd/containerd.sock, 네임스페이스 shearwater)
containerd (기존 systemd 서비스, Docker와 공유) → shim → runc → 컨테이너
```

- 구성 요소는 고루틴이고, 서로를 직접 부르지 않는다. 전부 저장소의 `get/list/watch/update` 인터페이스로만 통신한다. HTTP를 거치지 않지만 의미(낙관적 잠금, watch)는 같다. 4단계에서 프로세스를 나눌 때 이 인터페이스만 원격으로 바꾼다.
- 컨테이너는 containerd의 shim 프로세스가 붙들고 있다. `sw server`가 죽거나 재시작해도 컨테이너는 계속 돈다. 다시 뜬 `sw server`는 네임스페이스 `shearwater`의 컨테이너를 읽어 다시 붙는다. 다만 **죽은 컨테이너를 다시 띄우는 것은 전부 노드 에이전트의 일**이다(containerd와 runc에는 재시작 정책이 없다). `sw server`가 죽어 있는 동안에는 컨테이너가 죽어도 다시 뜨지 않고, 서버가 돌아온 뒤에 따라잡는다 [R1].
- 주소는 127.0.0.1만 연다. 윈도우 쪽 kubectl은 WSL의 localhost 전달로 붙는다.
- 1단계의 `sw up`은 `sw server`를 독립 세션의 백그라운드 프로세스로 시작한다. `sw down`은 시작 기록과 실제 프로세스 핸들을 대조해 이 서버만 멈춘다. 3단계에서 `sw server`를 systemd 유닛으로 옮긴다.

패키지 구조:

```
cmd/sw/                 진입점 (server, kubeconfig, status, tree ...)
internal/apis/          자체 타입, 기본값, 검증, 필드 등급, 셀렉터·수량 파서
internal/patch/         merge patch, JSON patch, strategic merge patch
internal/store/         SQLite, 리비전, 변경 로그, watch 허브
internal/api/           HTTP 라우팅, discovery, CRUD, watch, Table, Status 오류
internal/controller/    deployment, replicaset, endpointslice, gc
internal/scheduler/
internal/node/          Pod 동기화, 재시작 지연, 상태 보고, 로그
internal/runtime/       containerd 클라이언트, 네트워크(네임스페이스·netlink·전달 규칙), 가짜 구현
internal/proxy/         서비스 프록시
internal/agent/         트리, diff, 오류 코드, 정책 (2단계에서 MCP 추가)
hack/                   env.sh, e2e-stage1.sh
```

외부 의존성(예정): `modernc.org/sqlite`, `github.com/containerd/containerd/v2`(client 패키지와 그 의존성). netlink 소켓과 네임스페이스 호출에는 `golang.org/x/sys`를 쓴다(containerd 클라이언트가 이미 끌고 오는 모듈이다). 그 밖에는 표준 라이브러리. containerd 모듈의 go.mod에는 간접 의존성이 많지만 바이너리에 들어가는 것은 client 패키지가 쓰는 것뿐일 것으로 본다 (미검증: M2에서 `go version -m`으로 확인해 문서에 적는다).

## 5. 저장 구조 (SQLite)

설정: `journal_mode=WAL`, `synchronous=FULL`, `busy_timeout=5000`, `foreign_keys=ON`. 쓰기는 고루틴 하나가 순서대로 한다(SQLite는 어차피 쓰기가 하나다). 쓰기량이 적어서 `FULL`의 비용은 무시할 만하고, 전원이 나가도 커밋된 쓰기를 잃지 않는다.

```sql
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) WITHOUT ROWID;
-- schema_version, cluster_uid, rev(현재 리비전), compacted_rev

CREATE TABLE objects (
  grp        TEXT NOT NULL,            -- '' | 'apps' | 'discovery.k8s.io'
  resource   TEXT NOT NULL,            -- pods, deployments, ...
  namespace  TEXT NOT NULL,            -- 클러스터 범위는 ''
  name       TEXT NOT NULL,
  uid        TEXT NOT NULL UNIQUE,
  rev        INTEGER NOT NULL,         -- 마지막으로 쓴 리비전 = resourceVersion
  generation INTEGER NOT NULL,
  deleting   INTEGER NOT NULL DEFAULT 0,
  data       BLOB NOT NULL,            -- 객체 전체 JSON
  PRIMARY KEY (grp, resource, namespace, name)
) WITHOUT ROWID;

CREATE TABLE owners (                  -- GC가 "이 소유자의 자식"을 찾는 색인
  owner_uid TEXT NOT NULL,
  child_uid TEXT NOT NULL,
  PRIMARY KEY (owner_uid, child_uid)
) WITHOUT ROWID;
CREATE INDEX owners_by_child ON owners (child_uid);  -- M0에서 추가: 갱신·삭제 때 자식 uid로 지운다

CREATE TABLE changes (                 -- watch용 변경 로그
  rev       INTEGER PRIMARY KEY,
  grp       TEXT NOT NULL,
  resource  TEXT NOT NULL,
  namespace TEXT NOT NULL,
  name      TEXT NOT NULL,
  type      TEXT NOT NULL CHECK (type IN ('ADDED','MODIFIED','DELETED')),
  data      BLOB NOT NULL,             -- 그 리비전의 객체
  prev_data BLOB,                      -- 바뀌기 전 객체(ADDED는 NULL). 셀렉터 watch의 전이 판단용
  at        TEXT NOT NULL
);
CREATE INDEX changes_by_resource ON changes (grp, resource, namespace, rev);

CREATE TABLE ip_alloc (                -- IP 할당: Service ClusterIP와 Pod IP
  ip        TEXT PRIMARY KEY,
  kind      TEXT NOT NULL CHECK (kind IN ('service','pod')),
  owner_uid TEXT NOT NULL UNIQUE
) WITHOUT ROWID;

CREATE TABLE audit (                   -- 2단계에서 채운다. 스키마만 미리 둔다
  id        INTEGER PRIMARY KEY,
  at        TEXT NOT NULL,
  actor     TEXT NOT NULL,             -- admin | agent
  via       TEXT NOT NULL,             -- kubectl | sw | mcp
  verb      TEXT NOT NULL,
  target    TEXT NOT NULL,
  dry_run   INTEGER NOT NULL,
  confirmed INTEGER NOT NULL,
  result    TEXT NOT NULL              -- ok | 오류 코드
);
```

쓰기 한 번은 트랜잭션 하나다:

1. `meta.rev`를 1 올려 새 리비전을 얻는다. 한 트랜잭션에서 여러 객체를 바꾸면 객체마다 다른 리비전을 쓴다 [R1].
2. 객체의 `metadata.resourceVersion`에 그 값을 넣어 `objects`에 쓴다. update면 `WHERE rev = ?`로 기존 값을 확인하고, 0행이면 리비전 증가까지 전부 되돌리고 409를 돌려준다 [R1].
3. `changes`에 같은 리비전으로 한 줄 넣는다(바뀌기 전 객체 포함). `owners`와 `ip_alloc`도 같은 트랜잭션에서 맞춘다. `owners`에는 외래 키가 없으므로 정합성은 코드가 책임진다 [R1].
4. 커밋한 뒤에 watch 허브를 깨운다.

- **dryRun**은 객체, 리비전, IP 할당, 소유 관계, watch 알림 어느 것도 남기지 않는다. 감사 기록에 `dry_run=1`로 남기는 것만 한다(2단계) [R1].
- **내용이 같으면 쓰지 않는다.** 새 객체가 저장된 것과 바이트 단위로 같으면 리비전을 올리지 않고 성공을 돌려준다. 컨트롤러가 같은 status를 반복해서 써도 유휴 쓰기가 생기지 않는다 [R1].

list:

- 항목과 목록의 `resourceVersion`을 같은 읽기 스냅숏에서 얻는다. 목록의 버전은 항목들의 최댓값이 아니라 스냅숏 시점의 `meta.rev`이고, 빈 목록도 이 값을 가진다 [R1].

watch:

- 허브는 "새 리비전이 생겼다"는 신호만 보낸다. 이벤트 내용은 싣지 않는다. 구독자는 자기 커서(마지막으로 보낸 리비전) 뒤의 `changes`를 리비전 순서로 읽어 보낸다. 순서는 DB가 보장하고, 느린 구독자가 있어도 쓰기를 막지 않고 메모리에 큐가 쌓이지 않는다 [R1].
- 읽기는 짧은 배치로 나눈다. 읽기 트랜잭션을 전송하는 동안 잡고 있으면 WAL 정리가 막힌다. 배치마다 커서가 정리 경계(`meta.compacted_rev`)보다 뒤인지 다시 확인한다. 뒤처졌으면 건너뛰지 않고 `ERROR` 이벤트(Status 410)로 끝낸다 [R1].
- **셀렉터가 걸린 watch의 전이** [R1]: 바뀌기 전과 후가 조건에 맞는지로 이벤트 종류를 정한다. 밖 → 안은 ADDED, 안 → 안은 MODIFIED, 안 → 밖은 "바뀌기 전 객체 내용 + 이번 리비전"의 DELETED, 밖 → 밖은 보내지 않는다. `prev_data`가 이 판단에 쓰인다. `labelSelector`, `spec.nodeName`, `status.phase`, `metadata.name` 모두 같다.
- 시작할 때 요청한 버전이 정리 경계보다 오래됐으면 HTTP 410을 돌려준다.
- `changes`는 최근 1시간 또는 1만 줄만 남긴다(둘 중 더 많이 남는 쪽). 정리는 따로 도는 주기 작업이 아니라 쓰기 트랜잭션에 묶어서 하고, 삭제와 `meta.compacted_rev` 갱신은 한 트랜잭션이다 [R1].
- 북마크는 요청했을 때 1분마다 보낸다. 이미 처리한 리비전을 다시 쓸 뿐이고 새 리비전을 만들지 않는다. watch-list의 끝 북마크는 반드시 보낸다 [R1].

경로: DB `/var/lib/shearwater/state.db`, 인증서·토큰·정책 `/etc/shearwater/`, pid·네트워크 네임스페이스 `/run/shearwater/`. 백업(온라인 `VACUUM INTO`)은 3단계에서 한다.

## 6. 조정 루프

공통 규칙: 이벤트는 "이 키를 다시 보라"는 신호일 뿐이고, 루프는 매번 현재 상태 전체를 읽어 판단한다(레벨 기반). 키(`namespace/name`)별 작업 큐, 실패하면 지수 지연으로 재시도, 5분마다 전체 재확인(watch가 프로세스 안에서 돌아 이벤트를 잃지 않으므로 안전망 용도다). 같은 입력으로 여러 번 돌아도 결과가 같아야 한다. status는 계산한 값이 저장된 값과 다를 때만 쓰고, condition의 시각은 상태가 실제로 바뀔 때만 갱신한다 [R1].

**Deployment 컨트롤러** (Deployment, ReplicaSet을 본다)

1. 내가 소유한 ReplicaSet을 찾는다(ownerReferences + selector).
2. `spec.template`의 해시를 구해 `pod-template-hash` 라벨로 쓴다. 이 라벨은 Deployment 컨트롤러가 ReplicaSet마다 붙이는 것이다 [S4]. 해시 알고리즘은 자체 것(정규화한 JSON의 FNV-1a)이고 쿠버네티스 값과 같을 필요는 없다.
3. 템플릿이 같은 ReplicaSet이 없으면 새로 만든다. `deployment.kubernetes.io/revision` 주석을 1 올려 붙인다 [S4].
4. 전략에 따라 크기를 맞춘다.
   - RollingUpdate: 전체 Pod 수는 `replicas + maxSurge`를 넘지 않고, 사용 가능한 Pod 수는 `replicas - maxUnavailable` 아래로 내려가지 않는다. 기본값은 둘 다 25%이고, 백분율은 maxSurge는 올림, maxUnavailable은 내림이다 [S4].
   - Recreate: 옛 Pod를 모두 없앤 뒤 새로 만든다 [S4].
5. status와 conditions를 쓴다.
6. 롤아웃이 끝난 뒤 `revisionHistoryLimit`(기본 10)을 넘는 옛 ReplicaSet을 지운다 [S4].

M4에서는 삭제 중인 Pod도 객체가 사라지거나 phase가 Succeeded 또는 Failed가 될 때까지 서지 한도에 포함한다. ReplicaSet이 먼저 사라져도 Pod의 Deployment UID로 슬롯을 추적한다. 가용 수에는 삭제가 시작되지 않은 Ready Pod만 세고, 새 Pod가 준비되지 않으면 가용성 하한까지 복구할 수 있도록 기존 Pod를 보존한다. 두 한도가 백분율 반올림 뒤 모두 0이 되면 `maxUnavailable`을 1로 계산한다. [Kubernetes v1.36.6의 반올림 규칙](https://github.com/kubernetes/kubernetes/blob/v1.36.6/pkg/controller/deployment/util/deployment_util.go#L814-L840)

Deployment UID별 마지막 할당 번호는 SQLite에 보관하고 Deployment와 현재 ReplicaSet의 `deployment.kubernetes.io/revision`에 표시한다. 이전 템플릿으로 돌아가면 보존 중인 ReplicaSet을 재사용하고 새 번호를 준다. `revisionHistoryLimit`은 현재 ReplicaSet을 제외한 옛 세트에 적용하며, 롤아웃이 끝난 뒤 실행 중이거나 종료 처리 중인 Pod가 없는 세트를 오래된 순서로 정리한다. 일시 정지 중에는 템플릿 교체가 멈추고 기존 세트의 복제본 수는 조정할 수 있다.

**ReplicaSet 컨트롤러** (ReplicaSet, Pod를 본다)

1. selector에 맞는 Pod 중 내 것을 센다. 컨트롤러 소유자가 없는데 selector에 맞는 Pod는 입양한다 [S5].
2. 모자라면 템플릿으로 만든다(`generateName`, ownerReference 설정). 넘치면 지운다. 지우는 순서는 스케줄 안 됨 → 준비 안 됨 → 최근에 만든 것.
3. status를 쓴다.

**스케줄러** (nodeName이 빈 Pod를 본다)

- 노드가 하나라서 그 노드가 Ready면 묶는다(`spec.nodeName` 기록, PodScheduled=True). 노드가 준비되지 않았으면 Pending으로 두고 PodScheduled=False, 이유 `Unschedulable`을 적는다.
- 자원 계산은 1단계에서 안 한다. 4단계 직전에 넣는다.

**노드 에이전트** (이 노드에 묶인 Pod를 본다. 실행 순서는 7절)

- 원하는 상태는 저장소의 Pod, 실제 상태는 containerd에서 읽은 컨테이너·태스크다. 둘의 차이를 줄인다. 종료는 태스크의 종료 알림으로 바로 알고, 놓친 것은 30초마다 전체를 다시 읽어 잡는다.
- 컨테이너가 끝나면 `restartPolicy`(Always·OnFailure·Never, 기본 Always [S3])대로 한다. 첫 번째는 바로 다시 띄우고, 반복되면 10초부터 두 배씩 늘려 최대 300초까지 기다린다. 10분 동안 문제없이 돌면 지연을 초기화한다 [S3]. 기다리는 동안 컨테이너 상태는 `waiting`, 이유 `CrashLoopBackOff`다.
- 삭제: `deletionTimestamp`가 찍히면 EndpointSlice 컨트롤러가 그 Pod를 먼저 뺀다. 노드 에이전트는 TERM을 보내고 유예 시간(기본 30초)을 기다린 뒤 남은 것을 KILL한다 [S3]. 컨테이너, 네트워크, 볼륨 디렉터리까지 지운 다음 저장소에서 객체를 없앤다. 이때 DELETED 이벤트가 나간다. 유예가 끝나도 안 죽는 프로세스는 KILL을 다시 시도하고, 그동안 Pod는 종료 중으로 남는다 [R1].
- **현실을 먼저 읽는다** [R1]: 조정은 저장소의 status를 믿지 않고 containerd, 네트워크 네임스페이스, 주소의 실제 상태를 먼저 읽은 뒤 원하는 상태로 간다. 종료가 중간에 실패해 "IP는 반납됐는데 네임스페이스는 남은" 상태도 여기서 잡힌다.
- **서버가 죽어 있던 동안의 따라잡기** [R1]: 시작할 때 모든 태스크를 읽는다. 죽은 태스크는 종료 시각과 코드를 status에 적고 정책대로 다시 띄운다. 태스크가 이미 정리돼 코드를 알 수 없으면 이유를 "알 수 없음"으로 두되 `restartCount`는 센다. 끊긴 종료 대기를 다시 걸고, 놓친 종료는 containerd의 종료 이벤트 구독으로 잡는다.
- **재시작 지연은 status에서 복원한다** [R1]: 지연 단계를 메모리에만 두면 서버가 재시작할 때마다 초기화돼서, 계속 죽는 앱이 즉시 재시작을 반복한다. `restartCount`와 `lastState.terminated.finishedAt`에서 다시 계산한다. 그래서 `finishedAt`은 반드시 채운다.
- **고아 정리** [R1]: 네임스페이스 `shearwater` 안에서 우리 라벨이 붙은 것뿐 아니라 이름 규칙(`<pod-uid>-<컨테이너>-<시도>`)에 맞는 것도 훑는다(생성 도중 실패하면 라벨이 없을 수 있다). 부모 없는 스냅숏, Pod 객체 없이 남은 veth와 네트워크 네임스페이스도 정리한다. IP 할당은 주인 Pod 객체가 없고 그 uid의 컨테이너와 네임스페이스도 없을 때만 반납한다.
- 노드 객체는 상태가 바뀔 때만 쓴다. 단일 노드라 주기적인 하트비트 기록이 필요 없고, 유휴 때 디스크 쓰기를 0으로 두기 위해서다. containerd 연결은 10초마다 확인하되 결과가 같으면 저장소에 쓰지 않는다. 못 붙으면 NotReady다.

**EndpointSlice 컨트롤러** (Service, Pod를 본다)

- selector와 ClusterIP가 있는 Service마다 같은 네임스페이스의 Ready이고 종료 중이 아닌 Pod를 EndpointSlice에 담는다 [R1]. 이름 `targetPort`는 Pod의 포트 선언에서 찾아 숫자로 바꾼다 [S6]. Slice의 포트는 그 안의 모든 Pod가 공유하므로 숫자 포트 조합이 다른 Pod는 별도 Slice에 넣는다. Selector가 없거나 `clusterIP: None`이면 만들지 않는다.

**서비스 프록시** (Service, EndpointSlice를 본다)

- ClusterIP를 더미 인터페이스(`sw-svc0`)에 `/32`로 붙이고, `ClusterIP:port`에서 듣는 TCP 프록시를 Go로 직접 돌린다. 기존 호스트 주소나 경로와 겹치는 IP는 붙이지 않는다. 연결마다 현재 Service와 Pod의 식별자, Pod IP 예약과 Ready를 확인한다. 준비된 엔드포인트를 돌아가며 고르고 연결에 실패하면 다음 엔드포인트로 넘어간다. Service가 지워지면 소켓을 닫고 주소를 뗀다. 서버가 정상 종료되면 주소와 인터페이스는 남긴다. M6의 `sw down --all`은 그것까지 정리한다 [R1].
- iptables·nftables 규칙을 만들지 않으므로 정리할 것도 없고, 연결 수 같은 값을 에이전트 인터페이스로 바로 보여 줄 수 있다. 대가는 처리량이다(작은 서비스에는 충분, 문서에 적는다).
- Pod의 첫 컨테이너 실행 시도 직전에 같은 네임스페이스의 서비스 환경변수(`{이름}_SERVICE_HOST`, `{이름}_SERVICE_PORT`, 이름 포트별 변수)를 저장한다. 이름은 대문자로, `-`는 `_`로 바꾼다 [S6]. 명시한 `env`가 우선하며, 나중에 만든 Service는 새 Pod부터 반영한다. DNS는 3단계에서 한다. 동작 예제와 문제 확인 순서는 [Service 네트워킹 안내](service-networking.md)에 있다.

**GC** (모든 리소스의 삭제를 본다)

- Background(기본): 소유자를 바로 지우고, 소유자가 모두 사라진 자식을 뒤에서 지운다 [S7].
- Foreground: 소유자에 `foregroundDeletion` 파이널라이저를 달고 자식을 먼저 지운다 [S7].
- Orphan: 자식의 ownerReference만 뗀다 [S7].

## 7. containerd 연동 (결정 2)

containerd의 Go 클라이언트로 기존 서비스의 소켓에 붙는다. containerd 네임스페이스는 `shearwater`다. Docker가 쓰는 `moby` 네임스페이스는 읽지도 쓰지도 않는다. 이미지는 네임스페이스별로 따로 받는다.

CRI를 쓰지 않으므로 kubelet이 CRI에 맡기는 일 [S8]을 직접 한다. 아래 표가 그 목록이다.

| 일 | 방법 |
|---|---|
| Pod 네트워크 공유 | pause 컨테이너를 쓰지 않는다. Pod마다 네트워크 네임스페이스를 직접 만들어 `/run/shearwater/netns/<pod-uid>`에 묶어 두고, Pod의 모든 컨테이너가 OCI 설정으로 그 네임스페이스에 들어간다 |
| IP 할당과 연결 | 직접 한다(결정 6). 아래 "Pod 네트워크" 참고 |
| 이미지 | 클라이언트의 Pull + 압축 풀기. 짧은 이름(`nginx:1.27`)은 `docker.io/library/nginx:1.27`로 풀어 쓴다. `imagePullPolicy`에 따라 건너뛴다 |
| 실행 명령 | 쿠버네티스 규칙대로 `command`가 이미지의 ENTRYPOINT를, `args`가 CMD를 덮는다 |
| 컨테이너 생성 | 시도마다 새 컨테이너와 새 스냅숏을 만든다(재시작하면 루트 파일시스템이 새것이 되는 쿠버네티스 동작과 맞춘다). 이름은 `<pod-uid>-<컨테이너>-<시도>`, 라벨에 Pod의 네임스페이스·이름·uid |
| 파일 | Pod마다 `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf`를 만들어 마운트한다. `resolv.conf`는 1단계에서 호스트 것을 복사한다 |
| 볼륨 | `emptyDir`은 `/var/lib/shearwater/pods/<uid>/volumes/<이름>`, `hostPath`는 그대로 마운트 |
| 자원 제한 | `resources.limits`의 cpu·memory를 OCI 리눅스 자원 설정으로 넣는다. cgroup 경로는 `/shearwater/<pod-uid>/<컨테이너>_<시도 번호>`. 컨테이너 이름에 없는 `_`로 구형 경로와도 구분하고, 재시작 중 이전 태스크를 정리해도 새 태스크의 cgroup에 닿지 않게 시도별로 분리한다. |
| 로그 | 컨테이너의 표준 출력·오류를 shim이 로그 디렉터리 아래 `pods/<ns>_<name>_<uid>/<컨테이너>/<시도>.log`에 직접 쓰게 한다. `sw server`가 죽어도 로그가 끊기지 않는다. 최근 2회분만 남긴다 |
| 종료 감지 | 태스크의 종료 대기 + 30초마다 전체 재확인 |
| 종료 | TERM → 유예 시간 → KILL → 태스크·컨테이너·스냅숏 삭제 → veth 삭제, IP 반납 → 네트워크 네임스페이스 삭제 |

Pod 하나가 뜨는 순서:

```
네트워크 네임스페이스 생성 → IP 할당 → veth 연결, 주소·경로 설정 → hosts·resolv.conf 생성
→ 컨테이너마다: 이미지 받기 → 컨테이너+스냅숏 생성 → 태스크 생성(로그 파일 연결) → 시작 → 종료 대기 등록
→ status 기록 (podIP, containerStatuses, phase)
```

Pod 네트워크(결정 6, 직접 구현):

- **브리지**: `sw up` 때 `sw-br0`가 없으면 만들고 `10.244.0.1/24`를 붙인다. 있으면 그대로 쓴다.
- **Pod마다**: veth 쌍을 만든다. 호스트 쪽(`sw` + uid 앞 8글자)은 브리지에 붙이고, 반대쪽은 Pod의 네트워크 네임스페이스로 옮겨 `eth0`으로 이름을 바꾼다. 주소를 붙이고, 기본 경로를 `10.244.0.1`로 잡고, `lo`를 켠다.
- **IP 할당**: SQLite의 `ip_alloc` 표에서 한다(Service IP와 같은 표, `kind`로 구분). 게이트웨이 주소(`.1`)와 네트워크·브로드캐스트 주소는 주지 않는다 [R1]. 반납 조건은 6절 고아 정리와 같다.
- **MTU** [R1]: 브리지와 veth의 MTU를 바깥 인터페이스에 맞춘다(이 WSL의 eth0은 1500, 실측). 안 맞으면 작은 응답은 되고 큰 응답에서 멈춘다.
- **대역 충돌** [R1]: `sw up`과 `sw doctor`가 Pod·Service 대역이 호스트의 다른 경로와 겹치는지 검사한다(이 WSL에서는 겹치지 않음, 실측).
- **네임스페이스 정리** [R1]: 묶어 둔 마운트를 푸는 것은 우리 책임이다. Pod 삭제와 생성 실패 되돌리기마다 풀고, 시작할 때 `/run/shearwater/netns`를 훑는다. 컨테이너는 네임스페이스가 준비된 것을 확인한 뒤에 만든다.
- **재부팅 복구** [R1]: `/run`은 재부팅하면 비워져서 네임스페이스가 전부 사라지고 컨테이너도 없다. 저장소에는 Pod가 남아 있다. 시작할 때 이 상태를 보면 모든 컨테이너를 "종료됨(이유 알 수 없음)"으로 기록하고, 네임스페이스와 veth를 다시 만들어 **같은 IP**를 붙인 뒤(할당이 SQLite에 남아 있다) 정책대로 다시 띄운다.
- **구현 수단**: rtnetlink 메시지를 직접 만들어 netlink 소켓으로 보낸다. 필요한 동작은 링크 생성(bridge, veth), 링크 설정(브리지 연결, 켜기, 네임스페이스 이동, 이름 변경), 주소 추가, 경로 추가, 링크 삭제다. `ip` 명령을 부르지 않는다.
- **전달과 주소 변환 규칙**: Docker가 FORWARD 기본 정책을 DROP으로 두고, `bridge-nf-call-iptables=1`이라 같은 브리지 안의 Pod끼리 통신도 FORWARD를 거친다(둘 다 이 WSL에서 실측). 그래서 허용이 세 갈래 필요하다 [R1]: Pod ↔ Pod(`sw-br0`에서 `sw-br0`으로), Pod → 밖, 밖 → Pod의 응답(연결 추적). 주소 변환은 Pod 대역에서 나가는 것 중 목적지가 Pod 대역과 Service 대역이 아닌 것에만 건다.
  - 수단: `iptables` 실행 파일을 부른다(Docker가 도는 호스트에는 반드시 있다). 규칙은 전용 체인 `SHEARWATER-FORWARD`에 모으고, **`DOCKER-USER` 체인**에서 점프 한 줄을 넣는다. `DOCKER-USER`는 FORWARD의 맨 앞에 있고 Docker가 비우지 않는 자리다(이 WSL에서 맨 앞에 있고 비어 있음을 실측). `DOCKER-USER`가 없는 호스트에서는 FORWARD에 직접 넣는다. 주소 변환은 nat 표의 전용 체인에 둔다 [R1].
  - `sw up` 때마다 규칙이 있는지 확인하고 없으면 넣는다(Docker 재시작으로 사라졌을 경우). `sw down --all`에서 지운다.
  - M2에서 이 WSL의 Pod 간·외부 통신을 확인했다. 실험과 구현 검증 기록은 `docs/m2-first-checks.md`와 `docs/HANDOFF.md`에 있다.
- **커널 설정**: `net.ipv4.ip_forward`가 꺼져 있으면 `sw up`이 필요한 변경을 안내하고 멈춘다. Pod 네트워크를 위해 호스트 공통 설정을 자동으로 바꾸지 않는다.
- **자기 Service로 돌아오는 경우**: 서비스 프록시가 호스트에서 Pod로 별도 TCP 연결을 열어 Pod가 받는 연결의 출발지는 게이트웨이 주소다. `hack/m5-check.sh`에서 Pod 안의 `wget`이 자기 Service의 ClusterIP를 거쳐 자기 HTTP 서버에서 응답을 받는 것을 확인했다.
- 대가: 사용자가 따로 설치할 것이 containerd뿐이 되는 대신, 위 코드를 전부 직접 짜고 직접 검증해야 한다. IPv6, 네트워크 정책, 대역 변경은 1단계에서 하지 않는다.

1단계의 제약과 위험:

- `kubectl logs --timestamps`와 시간 필터는 지원하지 않는다(로그에 시각을 적는 주체가 없다). 로그 회전도 없다. 3단계에서 자체 로그 기록기로 바꾼다.
- **로그 파일 직접 쓰기** [R1]: 이 WSL의 containerd v2.3.3에서 shim이 파일을 열고 서버와 독립적으로 stdout·stderr를 기록하는 것을 확인했다(`docs/m2-first-checks.md`).
- 서버가 죽은 동안 로그는 파일에 쌓이지만 조회는 안 된다(API가 없다). README에서 "유지"와 "조회 가능"을 구분해 적는다 [R1].
- 3단계에서 systemd 유닛으로 옮길 때 마운트 네임스페이스를 분리하는 옵션(`PrivateMounts` 등)을 켜면 네트워크 네임스페이스가 밖에서 안 보인다. 켜지 않는다 [R1].
- Docker와 containerd를 같이 쓴다. Docker나 containerd를 apt로 올리거나 서비스를 재시작하면 `sw server`의 연결이 끊긴다. 컨테이너는 shim이 붙들고 있어 계속 돌지만, 그동안 노드는 NotReady가 되고 다시 붙을 때까지 재시작·배포가 멈춘다. README에 적는다.
- 사설 레지스트리 인증은 3단계.
- Service 대역은 `10.96.0.0/16`.

**M2 첫 확인 목록** (구현 전에 작은 실험으로 먼저 본다) [R1]: ① shim의 로그 파일 직접 쓰기 ② `DOCKER-USER` 규칙으로 Pod ↔ Pod, Pod → 밖 통신 ③ MTU ④ 재부팅 뒤 같은 IP로 복구.

**결정 6을 다시 볼 조건** (딥시크 제안. 판단은 사용자가 한다): M2 첫 확인에서 ②가 이 WSL에서 풀리지 않거나, netlink 직접 구현이 M2를 M3~M6 전체보다 길게 만들 것으로 보이면 그때 사용자에게 보고하고 CNI 플러그인으로 돌아갈지 묻는다. 플러그인은 `/opt/shearwater/cni/bin`에 남아 있다.

테스트: `internal/runtime`은 인터페이스이고 가짜 구현이 있다. root와 containerd 없이도 컨트롤러·노드 로직의 단위 테스트가 돈다.

## 8. 에이전트 인터페이스 초안

구현은 2단계다. 1단계에서는 토대(오류 코드, `sw status`, `sw tree`, `sw doctor`, 변경 로그)만 만든다. desk-agent에서 가져오는 것과 바꾸는 것을 먼저 적는다.

| desk-agent | Shearwater | 바꾼 이유 |
|---|---|---|
| 줄인 트리 + `@e12` ref | 줄인 트리 + `deploy/web` 형식 이름 | 화면 요소와 달리 쿠버네티스 객체는 안정된 이름이 있다. 가짜 ref가 필요 없다 |
| `STALE_REF` | `CONFLICT` + `--if-rev N` | "내가 본 뒤로 바뀌었나"를 resourceVersion으로 확인한다 |
| `--diff`: 직전 스냅샷과 비교 | `--diff`: 변경 로그에서 리비전 사이를 읽는다 | 스냅샷 파일이 필요 없고 클라이언트마다 커서가 독립이다 |
| `desk wait --text/--gone/--stable` | `sw wait --for available/deleted/stable` | 잠자지 않고 조건으로 기다린다는 원칙은 그대로 |
| 정책을 관리자 에이전트 안에서 검사 | 정책을 API 서버 안에서 검사 | 어떤 클라이언트로 오든 같은 규칙을 거친다는 원칙은 그대로 |
| `confirm_labels` (글자 일치) | 동사·리소스·조건 규칙 | 위험이 글자가 아니라 동작(삭제, 0으로 축소)에 있다 |
| 깨진 정책 파일은 오류 | 같음 | 깨졌다고 보호가 꺼지면 안 된다 |
| 의존성 없는 MCP(stdio, JSON-RPC) | 같음(Go 표준 라이브러리만) | |
| SKILL.md | 같음 | |

### 8.1 명령

```
읽기   sw tree [-n NS] [--root deploy/web] [--problems]   줄인 상태 트리
       sw diff [--since REV]                               그 뒤로 바뀐 줄만
       sw get deploy/web [--full]                          한 객체 (기본은 줄인 형태)
       sw why pod/web-7c9f-x2k4q                           상태 + 최근 이벤트 + 마지막 종료 코드 + 로그 끝
       sw logs pod/NAME [--tail 50] [--previous]
       sw events [--since REV]
       sw wait deploy/web --for available [--timeout 60]
       sw status                                           서버·노드·Pod 요약과 리비전
       sw doctor                                           서버, containerd, 네트워크, 정책 지원 상태, 최근 오류
       sw policy                                           지금 적용 중인 정책
쓰기   sw apply -f FILE [--dry-run] [--diff] [--wait]
       sw scale deploy/web 5 [--dry-run] [--diff] [--wait] [--confirm]
       sw restart deploy/web [--diff] [--wait]
       sw delete deploy/web [--dry-run] [--confirm]
       sw run --image IMG [--timeout 60] [--net none] -- CMD...   샌드박스 Pod (2단계)
기타   sw up                                               준비 확인 + 서버를 뒤에서 시작 + kubeconfig 생성 + 쓰는 법 한 줄 출력
       sw down                                             서버만 멈춘다. --all이면 Pod·전용 네트워크를 내리고 SQLite·설정·로그는 보존
       sw server · sw kubeconfig · sw mcp · sw skill
```

### 8.2 출력 예시

트리:

```
$ sw tree
cluster sw-dev rev=1042 node=sw-node Ready pods=3/3
ns default
  deploy/web 3/3 ready rev=2 image=nginx:1.27
    rs/web-7c9f 3/3
      pod/web-7c9f-x2k4q Running 1/1 restarts=0 10.244.0.5
      pod/web-7c9f-m8d2p Running 1/1 restarts=0 10.244.0.6
      pod/web-7c9f-q1w9z Running 1/1 restarts=0 10.244.0.7
    rs/web-5d8a 0/0 old
  svc/web 10.96.0.12:80 -> 3 endpoints
```

`--problems`는 정상이 아닌 줄과 그 조상만 남긴다:

```
$ sw tree --problems
cluster sw-dev rev=1107 node=sw-node Ready pods=2/3
ns default
  deploy/web 2/3 ready rev=3 image=nginx:1.99
    rs/web-84bd 0/1
      pod/web-84bd-k2j5n Pending 0/1 ImagePullBackOff restarts=0
```

동작과 변경분을 한 번에:

```
$ sw scale deploy/web 5 --diff --wait
ok rev=1042..1061 settled=4.2s
~ deploy/web 3/3 -> 5/5 ready
+ pod/web-7c9f-abcde Running 1/1 10.244.0.8
+ pod/web-7c9f-fghij Running 1/1 10.244.0.9
~ svc/web 3 -> 5 endpoints
```

dry-run(서버의 `dryRun=All`을 쓴다. 저장하지 않는다):

```
$ sw delete deploy/web --dry-run
dry-run: nothing was changed
- deploy/web
- rs/web-7c9f, rs/web-5d8a
- pod x3 (Running)
~ svc/web 3 -> 0 endpoints
needs: --confirm (policy: delete deployments)
```

오류. 첫 줄은 `코드 retry=safe|unsafe|never: 내용`, 둘째 줄은 `hint:`다:

```
$ sw delete deploy/web
POLICY_DENIED retry=never: delete deploy/web would remove 3 running pods. Nothing was changed.
hint: ask the user about this exact action; only if they agree, repeat with --confirm
```

진단:

```
$ sw why pod/web-84bd-k2j5n
pod/web-84bd-k2j5n Pending 0/1 age=2m node=sw-node
cause: ImagePullBackOff - nginx:1.99 not found (manifest unknown)
events:
  2m  Scheduled   bound to sw-node
  2m  Pulling     nginx:1.99
  1m  Failed      pull: manifest unknown   x3
next: fix the image tag in deploy/web (current revision 3, previous revision 2 used nginx:1.27)
```

### 8.3 오류 코드

| 코드 | retry | 뜻 | 쿠버네티스 Status |
|---|---|---|---|
| `NOT_FOUND` | never | 그런 객체가 없다 | 404 NotFound |
| `ALREADY_EXISTS` | never | 같은 이름이 있다 | 409 AlreadyExists |
| `CONFLICT` | safe | 본 뒤로 바뀌었다. 다시 읽고 시도 | 409 Conflict |
| `EXPIRED` | safe | 리비전이 보관 범위 밖. 다시 `sw tree` | 410 Expired |
| `INVALID` | never | 필드·값이 잘못됐다 | 422 Invalid, 400 |
| `UNSUPPORTED` | never | 지원하지 않는 기능·필드 | 415, 422 |
| `POLICY_DENIED` | never | 확인이 필요하다. 아무것도 바뀌지 않았다 | 403 Forbidden |
| `FORBIDDEN` | never | 정책과 무관하게 허용되지 않는 동작(`default` 네임스페이스 삭제 등). M0에서 추가 | 403 Forbidden |
| `UNAUTHORIZED` | never | 토큰이 없거나 틀렸다. M0에서 추가 | 401 Unauthorized |
| `TIMEOUT` | safe | 기다린 조건이 안 됐다. 상태를 보고 판단 | - |
| `RUNTIME_ERROR` | unsafe | 런타임이 도중에 실패. 적용됐는지 먼저 확인 | 500 |
| `INTERNAL` | unsafe | 서버 내부 오류 | 500 |
| `NOT_RUNNING` | never | `sw server`가 안 떠 있다 | - |
| `PRECHECK_FAILED` | never | 시작 전에 필요한 런타임·파일시스템·네트워크 검사를 통과하지 못했다 | - |

kubectl로 들어온 요청도 같은 Status 본문의 `message` 첫 줄에 이 형식을 넣는다. `retry`는 코드의 기본값이고 오류마다 바꿀 수 있다(비어 있지 않은 네임스페이스 삭제는 `CONFLICT retry=never`).

### 8.4 정책 초안 (`/etc/shearwater/policy.json`)

```json
{
  "confirm": [
    {"verb": "delete", "resources": ["deployments", "services", "namespaces"]},
    {"verb": "delete", "resources": ["pods"], "when": "no_controller"},
    {"verb": "scale", "when": "to_zero"},
    {"verb": "scale", "when": "decrease_ratio>=0.5"},
    {"verb": "apply", "when": "removes_objects"}
  ],
  "protected_namespaces": ["kube-system"],
  "sandbox": {"network": "none", "memory": "256Mi", "cpu": "500m", "timeout_seconds": 300}
}
```

- 자격은 둘이다. `admin`(사람의 kubectl, 정책 없음)과 `agent`(`sw`·MCP, 정책 적용). 에이전트가 agent kubeconfig로 kubectl을 직접 써도 서버가 같은 규칙으로 막는다. 확인은 `sw --confirm`(MCP `confirm=true`)으로만 넘길 수 있다.
- `--confirm`은 "사용자가 이 동작에 동의했다"는 표시다. 미리 붙이지 않는다는 규칙을 SKILL.md에 적고, 감사 기록(`audit`)에 남긴다.

### 8.5 MCP 도구 초안

읽기(`readOnlyHint`): `sw_tree`, `sw_diff`, `sw_get`, `sw_why`, `sw_logs`, `sw_events`, `sw_wait`, `sw_doctor`, `sw_policy`.
쓰기(`destructiveHint`): `sw_apply`, `sw_scale`, `sw_restart`, `sw_delete`, `sw_run`. 모두 `dry_run`, `diff`, `confirm` 인자를 받는다.

### 8.6 샌드박스가 1단계에 요구하는 것

`sw run`은 2단계지만 1단계 Pod 구현이 다음을 갖춰야 한다: `restartPolicy: Never`, 종료 코드 보고(Succeeded·Failed), 로그 읽기, 자원 제한. 네이티브 클라이언트라서 컨테이너 안 명령 실행(exec)과 런타임 교체(gVisor 등)는 2단계에서 같은 클라이언트로 붙일 수 있다. runc 컨테이너는 호스트와 커널을 공유한다. 신뢰할 수 없는 코드를 막는 강한 경계가 아니라는 점을 문서에 적는다.

## 9. 하지 않을 것 (1단계)

- 다중 노드, Raft, 고가용성
- server-side apply, `managedFields`, 집계 discovery, Protobuf, CRD, 승인(admission) 웹훅, RBAC
- ConfigMap, Secret, ServiceAccount, Job, CronJob, StatefulSet, DaemonSet, Ingress, PersistentVolume, HPA
- 프로브 실행, init 컨테이너, 사이드카
- NodePort·LoadBalancer, UDP, DNS, NetworkPolicy
- `exec`, `attach`, `port-forward`, `cp`
- 자원 요청 기반 스케줄링, 우선순위, 축출(eviction)
- 사설 레지스트리 인증, 이미지 가비지 컬렉션, 로그 회전·시각 표시
- MCP 서버, 정책 적용, 샌드박스 실행(2단계)
- SQLite 백업, systemd 유닛, 무중단 교체 보장(3단계)
- 대시보드. 사람용 화면은 만들지 않는다. 사람은 kubectl을 쓴다

## 10. 구현 순서

| | 내용 | 끝났다는 증거 |
|---|---|---|
| M0 | 뼈대, 자체 타입·기본값·검증, 저장소, discovery, 최소 OpenAPI v3, CRUD, TLS·kubeconfig | 컨테이너 없이 `kubectl create -f/get/delete`가 객체에 동작. `docs/kubectl-trace.md`의 남은 항목 채움. **완료(2026-11-10): `hack/m0-check.sh` 47개 통과, 결과는 `docs/HANDOFF.md`** |
| M1 | **먼저 테스트 자료 고정**(패치, Protobuf 본문, 셀렉터 watch 전이). 그다음 watch, Table, 패치 3종(strategic merge patch 포함), Protobuf 요청 디코더, dry-run, 필드 등급 | `kubectl get -w`, `kubectl apply` 반복(추가·삭제·순서 변경), `kubectl edit`, `kubectl create deployment`. **완료(2026-11-11): `hack/m1-check.sh` 103개 통과(kubectl v1.36.6 91개, 윈도우 v1.36.0 12개), `hack/m0-check.sh` 47개 그대로 통과. `/scale`은 M4** |
| M2 | **먼저 첫 확인 목록**(7절). 그다음 런타임: containerd 클라이언트, 네트워크 직접 구현(네임스페이스, netlink, IP 할당, 전달 규칙), 로그. 노드 에이전트 | **완료(2026-11-11).** Pod 실행·IP·로그·재시작 및 Pod 간·외부 통신 실측. `hack/m2-check.sh`와 M0 47개·M1 103개 통과. 상세 기록은 `docs/HANDOFF.md` |
| M3 | 스케줄러, ReplicaSet, Deployment, GC | **완료(2026-11-11).** `hack/m3-check.sh`에서 Deployment 생성, 자동 바인딩, Pod 대체, Background·Foreground·Orphan을 확인했다. |
| M4 | 롤링 교체, `/scale`, 리비전 | **완료(2026-11-11).** `hack/m4-check.sh`에서 롤아웃 예산, `kubectl scale`, `rollout status/history/undo`, 리비전 정리를 실측했다. |
| M5 | Service: IP 할당, EndpointSlice, 프록시, 환경변수 | **완료(2026-11-11).** `hack/m5-check.sh`에서 WSL과 Pod에서 ClusterIP로 접속하고, 연결 분산·종료 대상 제외·재시작과 IP 재사용을 실측했다. [사용 예제와 구현 설명](service-networking.md) |
| M6 | 재시작 내성, `sw up/down/status/tree/doctor`, e2e 스크립트, 발자국 측정 | **완료(2026-11-11).** `hack/e2e-stage1.sh`, M0–M5 회귀와 `hack/measure.sh` 통과. [운영 안내](operations.md) · [검증 결과와 범위](stage1-verification.md) |

## 11. 검증: 1단계 완료 기준

`hack/e2e-stage1.sh`가 아래를 순서대로 실행하고 전부 통과해야 1단계 완료다. 실제로 돌린 결과만 보고한다.

1. `kubectl version`, `kubectl cluster-info`, `kubectl api-resources`가 오류 없이 나온다.
2. `kubectl get nodes`에 노드 하나가 `Ready`.
3. `kubectl create deployment web --image=nginx:1.27 --replicas=2` 뒤 `kubectl rollout status deploy/web --timeout=120s`가 0으로 끝난다.
4. `kubectl get deploy,rs,pods -o wide`에서 `2/2`, Pod는 `Running`이고 IP가 있다.
5. WSL에서 Pod IP로 `curl`하면 200.
6. **죽으면 재시작**: 컨테이너의 주 프로세스를 죽이면 같은 Pod의 `RESTARTS`가 1 늘고 `Running`으로 돌아온다.
7. **없어지면 대체**: `kubectl delete pod`로 하나를 지우면 새 Pod가 생겨 다시 `2/2`.
8. `kubectl scale deploy/web --replicas=4` 뒤 Pod 4개.
9. 매니페스트를 `kubectl apply -f`로 올리고, 이미지를 바꿔 다시 apply하면 새 ReplicaSet으로 교체되고 `rollout status`가 0으로 끝난다. `kubectl rollout history`에 리비전 2개.
10. apply로 환경변수를 추가·삭제하고 컨테이너 순서를 바꾼 결과가 매니페스트와 같다(strategic merge patch 확인).
11. `kubectl logs`, `kubectl describe pod`(이벤트 포함), `kubectl get pods -w`가 동작한다.
12. `kubectl expose deploy web --port=80` 뒤 ClusterIP로 `curl`하면 200. `kubectl get endpointslices`에 Pod IP가 보인다.
13. **서버 재시작 내성**: `sw server`를 죽였다 다시 띄워도 컨테이너 ID와 `RESTARTS`가 그대로이고, 상태가 SQLite에서 복구된다.
14. `kubectl apply --dry-run=server`는 아무것도 저장하지 않는다.
15. 낡은 `resourceVersion`으로 PUT하면 409. 거부 등급 필드는 422, 저장만 등급 필드는 경고, 모르는 필드는 400이 나온다.
16. `kubectl delete deploy web` 뒤 ReplicaSet·Pod가 모두 사라지고, containerd 네임스페이스 `shearwater`에 컨테이너·태스크가, 런타임 디렉터리에 네트워크 네임스페이스가, veth 인터페이스와 IP 할당이 남지 않는다. Docker 격리는 코드와 검증 스크립트가 `shearwater`만 지정하는지 확인한다. 사용자 지시대로 `moby`·`moby_history`의 직접 조회와 전후 비교는 하지 않는다.

17. **발자국 측정**: `hack/measure.sh`로 0.1절 표의 항목(유휴 디스크 쓰기, CPU, 메모리, 바이너리 크기, 시작 시간)을 재서 README에 적는다. 목표를 넘은 항목은 이유와 함께 적는다.
18. **처음 쓰는 흐름**: 아무 설정 없이 `sw up` 한 번으로 서버가 뜨고, 출력된 한 줄대로 kubectl이 붙는다. 준비물이 빠진 상태에서는 `sw up`이 무엇이 없는지와 고치는 법을 알려 주고 멈춘다.

19. `kubectl get pods -A`, `kubectl get pods -l app=web -w`(라벨을 떼면 DELETED가 나온다), `kubectl scale --current-replicas`가 동작한다.
20. **서버가 죽은 동안 죽은 컨테이너**: `sw server`를 멈춘 상태에서 컨테이너를 죽이고 서버를 다시 띄우면, `RESTARTS`가 1 늘고 `lastState`에 종료 시각이 남고 다시 `Running`이 된다.
21. 같은 이름의 Pod를 지우자마자 다시 만들어도 `kubectl delete`의 대기가 정상 종료한다.

별도로 `go test ./...`: 저장소(리비전 단조 증가, 내용이 같으면 리비전 불변, 조건부 갱신 실패 시 전부 되돌림, watch 재생, 셀렉터 전이 네 경우, 느린 구독자, 정리 경계의 410, dryRun 무부작용), 패치 3종, Protobuf 리더와 메시지별 표, 셀렉터·수량 파서, 컨트롤러, 가짜 런타임을 꽂은 노드 에이전트(지연 복원, 따라잡기, 고아 정리).

## 12. 참고 소스에서 가져온 것

- **pi** [S13]: LLM API, 에이전트 루프, TUI, 코딩 에이전트 CLI를 묶은 도구 모음. 가져올 점은 에이전트가 쓰는 도구를 작고 조합하기 쉬운 CLI로 둔다는 방향이다. `sw`를 MCP 없이도 셸에서 그대로 쓸 수 있게 하고, MCP는 같은 명령을 얇게 감싼다.
- **cua** [S14]: 에이전트에게 컴퓨터(VM·컨테이너 샌드박스)를 내주는 인프라. `cua sb create`처럼 샌드박스를 명령 하나로 만든다. `sw run`의 모양과, 사람과 에이전트가 같은 환경을 번갈아 쓴다는 생각을 참고한다.
- **minikube 대시보드** [S15]: 배포, 문제 해결, 자원 관리, 전체 현황 보기, 리소스 생성·수정을 사람이 화면으로 한다. Shearwater는 같은 다섯 가지를 `sw apply`, `sw why`, `sw scale/restart/delete`, `sw tree`, `sw get`으로 에이전트에게 준다.

## 출처

- [S1] Kubernetes API Concepts (watch, resourceVersion, 410 Gone, bookmark, streaming list, Table, dry-run, field validation, patch 종류): https://kubernetes.io/docs/reference/using-api/api-concepts/
- [S2] The Kubernetes API (discovery 집계·비집계, OpenAPI v2·v3, Protobuf): https://kubernetes.io/docs/concepts/overview/kubernetes-api/
- [S3] Pod Lifecycle (phase, restartPolicy, 재시작 지연, conditions, 종료 절차): https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/
- [S4] Deployments (pod-template-hash, 롤링 교체, maxSurge·maxUnavailable, status, revisionHistoryLimit, selector 불변): https://kubernetes.io/docs/concepts/workloads/controllers/deployment/
- [S5] ReplicaSet (ownerReferences, Pod 입양): https://kubernetes.io/docs/concepts/workloads/controllers/replicaset/
- [S6] Service (ClusterIP, targetPort, selector 없는 Service, 환경변수, 422): https://kubernetes.io/docs/concepts/services-networking/service/
- [S7] Garbage Collection (연쇄 삭제 Foreground·Background, orphan): https://kubernetes.io/docs/concepts/architecture/garbage-collection/
- [S8] Container Runtime Interface (kubelet이 런타임에 맡기는 일의 기준): https://kubernetes.io/docs/concepts/architecture/cri/
- [S9] EndpointSlices: https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/
- [S10] Server-Side Apply (managedFields): https://kubernetes.io/docs/reference/using-api/server-side-apply/
- [S11] Update API Objects in Place Using kubectl patch (strategic merge patch, patchStrategy, patchMergeKey, $retainKeys): https://kubernetes.io/docs/tasks/manage-kubernetes-objects/update-api-object-kubectl-patch/
- [S12] Declarative Management Using Configuration Files (last-applied-configuration): https://kubernetes.io/docs/tasks/manage-kubernetes-objects/declarative-config/
- [S13] pi: https://github.com/earendil-works/pi
- [S14] cua: https://github.com/trycua/cua
- [S15] minikube Dashboard: https://minikube.sigs.k8s.io/docs/handbook/dashboard/
- [S16] 이름 충돌 검색(2026-11-10, agentkube를 버린 근거): https://github.com/agentkube/agentkube
- [R1] 검토 회의 1차(2026-11-10): `docs/meetings/2026-11-10-design-review.md`. 코덱스(쿠버네티스 v1.36.6 소스 대조)와 딥시크(헤드리스, 설계 문서 검토)의 의견과, 클로드가 이 WSL에서 실측한 커널·방화벽 상태. 코덱스가 제시한 소스 경로는 그 파일에 적었다.
- desk-agent(로컬, 읽은 파일): `README.md`, `skills/desk-agent/SKILL.md`, `deskagent/errors.py`, `deskagent/policy.py`, `deskagent/mcp_server.py`
