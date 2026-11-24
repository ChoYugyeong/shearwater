# 설계: 여러 서버·접근 제어·DNS·Ingress·영속 볼륨

2026-11-24 시작. 사용자 요청: "RBAC·다중 사용자 격리·DNS·Ingress·PVC 구현, 여러 서버 지원.
경량 오케스트레이션을 표방하는데 단일이면 누가 쓰겠냐."

이 문서는 세 작업자(Claude, Codex, Gemini)가 동시에 일하기 위한 **계약**이야. 설계가 바뀌면 이
문서를 먼저 고치고 다른 작업자에게 알린다.

**상태(2026-11-25):** 1·2단계 모두 `multi-node-tenancy` 브랜치에 합쳐짐(push 안 함). 사용 안내는
[여러 서버](multi-node.md), [접근 제어](access-control.md), [클러스터 DNS](dns.md), [Ingress](ingress.md).
QA 배포판의 서버 세 대 흉내에서 9단계 e2e 통과(`maintenance/20261124-multinode-tenancy/qa/`). 설계에서
바뀐 점: 볼륨 데이터는 "PV가 없는 디렉터리 정리" 대신 컨트롤 플레인이 표시한 PV만 그 노드가 지움(복원 안전),
ReplicaSet이 만드는 Pod도 admission(쿼터·Pod Security)을 거침, 컨트롤 플레인 호스트의 ClusterIP 연결은
다른 노드로 갈 때 마스커레이드. 남은 것: 실제 서로 다른 서버·클라우드에서의 확인, 컨트롤 플레인 고가용성,
eviction API, 워커 프록시의 연결 종료 대기 보고.

## 0. 원칙과 범위

- 핵심 원칙은 그대로: **경량화와 사용자 편의성**. 바이너리 하나, 노드마다 상주 프로세스 `sw` 하나.
  DNS·Ingress·볼륨 프로비저너도 `sw` 안에서 돈다. CoreDNS·nginx·별도 프로비저너를 띄우지 않는다.
- **새 외부 의존성 금지**: `go.mod`를 바꾸지 않는다. `k8s.io/*`는 테스트 도구로도 레포에 넣지 않는다.
  DNS 메시지 형식, RBAC 평가, VXLAN netlink 메시지 모두 직접 작성한다.
- 호환 기준은 계속 "kubectl이 붙는다". 쿠버네티스 객체 이름·필드·동작을 따르고, 다른 점은
  `docs/compatibility.md`와 경고에 적는다. 저장만 하고 실행하지 않는 필드는 `internal/apis/grades.go`에.
- 컨트롤 플레인은 **한 대**(SQLite). 워커는 여러 대. 컨트롤 플레인이 죽으면 API·재배치는 멈추지만
  워커의 앱은 계속 돈다. 컨트롤 플레인 고가용성(Raft)은 이번 범위가 아니다. 문서에 과장하지 않는다.
- 재지 않은 수치는 문서에 쓰지 않는다.

## 1. 작업 분담

| 작업자 | 범위 | 브랜치 / worktree |
|---|---|---|
| Claude | 공통 기반, 여러 서버(노드 API·join·스케줄러·노드 수명·VXLAN·로그), 통합, QA | `multi-node-tenancy` / `worktrees/tenancy` |
| Codex (GPT-6.2 Astra) | 접근 제어: RBAC, 사용자, 테넌트 네임스페이스, ResourceQuota, Pod Security | `mnt-access` / `worktrees/tenancy-access` |
| Gemini (4 Neon) | 네트워크 서비스: 클러스터 DNS, Pod resolv.conf, Ingress | `mnt-netsvc` / `worktrees/tenancy-netsvc` |

2단계(1단계가 합쳐진 뒤): PersistentVolume/PVC/StorageClass(5절), NetworkPolicy(6절).

### 1.1 공통 규칙

- 자기 worktree·브랜치에서만 일한다. 다른 체크아웃(특히 `source`, 다른 세션의 미커밋 작업이 있음)은
  건드리지 않는다. **push 금지**. 커밋은 자기 브랜치에 로컬로, 파일 묶음 단위로(`git add -A` 금지,
  경로를 지정). 커밋 메시지는 기존 기록처럼 영어 제목 + 왜 바꿨는지 쓰는 본문.
- 파일 소유권(아래 각 절)을 지킨다. 남의 파일을 고쳐야 하면 고치지 말고 보고서에 "필요한 변경"으로
  적는다. 예외로 허용한 공유 파일은 **추가만** 한다(기존 줄을 다시 쓰지 않는다).
- 확인: WSL에서 `bash hack/t.sh`(gofmt + vet + 전체 테스트)가 통과해야 한다.
  Windows에서는 `wsl.exe -d Ubuntu-24.04 -- bash -lc 'cd /mnt/g/Dev/shearwater/worktrees/<이름> && bash hack/t.sh'`.
  Git Bash에서 부를 때는 `MSYS_NO_PATHCONV=1`을 앞에 붙인다. root·containerd·실제 서버 실행은 하지
  않는다(실제 e2e는 Claude가 QA 배포판에서 한다). 운영 배포판의 `sw`·서비스를 건드리지 않는다.
- 끝나면 `C:\Dev\shearwater\maintenance\20261124-multinode-tenancy\<작업자>-report.md`에
  무엇을 했는지, 테스트 결과, 남은 것, 남의 파일에 필요한 변경을 쓴다.

### 1.2 공통 기반 (Claude가 먼저 커밋함, 모두 이 위에서 시작)

- `internal/auth`: `Identity{Name, Groups, Kind, CredentialID}`, 토큰 형식 `sw<k>_<id>_<secret>`
  (k: `u` 사용자, `n` 노드, `j` join), `Authenticator`(관리자 토큰 + 저장된 자격 증명),
  `Attributes`(RBAC가 판단할 요청 속성), `Authorizer` 인터페이스, `WithIdentity/From`(context).
  사용자 자격 증명은 `system:masters`를 가질 수 없다. 관리자 토큰만 인가를 건너뛴다.
- `internal/store/credentials.go`: `credentials` 표(해시만 저장). `Credential`, `PutCredential`,
  `DeleteCredential(s)`, `Credentials(kind)`. 감시(watch)·변경 기록에 나오지 않는다.
- `internal/store/reader.go`: `store.Reader{Get, List, Changed}`. **모든 노드에서 도는 코드(프록시,
  DNS, 네트워크 정책, 노드 에이전트)는 `*store.Store`가 아니라 이 인터페이스에 의존한다.** 워커에서는
  노드 API 클라이언트가 이걸 구현한다.
- `internal/admission`: `Request{Identity, Operation, Resource, Namespace, Name, Object, Old, Tx, DryRun}`,
  `Func`, `Denied`(HTTP 코드·이유·오류 코드·힌트), `Forbid`, `Conflict`, `Chain`.
- `internal/api/server.go`: 요청마다 `identify` → 노드·join 자격 증명은 쿠버네티스 API에서 403 →
  `/shearwater/admin/`는 관리자만(`Server.Admin`) → `/shearwater/node/`는 `Server.NodeAPI`가 직접 인증 →
  자원 요청은 `attributesOf`로 동사(get/list/watch/create/update/patch/delete/deletecollection)·그룹·
  자원·하위자원·네임스페이스·이름을 만들어 `Server.Authorizer`에 묻는다(관리자는 묻지 않음, Authorizer가
  nil이면 관리자만 허용). 거절 메시지는 쿠버네티스 형식(`pods is forbidden: User "alice" cannot list ...`).
  `/api`, `/apis`, `/version`, `/openapi/*`는 인증된 사용자 모두 허용(쿠버네티스 system:discovery).
- `internal/api/handlers.go`: create와 update/patch(전체 객체 교체 경로)에서 검증이 끝난 뒤, 같은 쓰기
  트랜잭션 안에서 `Server.Admit`을 부른다. `/status` 갱신과 delete는 부르지 않는다.
- `internal/apis/registry.go`: `apis.Register(*Resource)`. 새 자원은 **자기 파일의 `init()`에서
  등록**한다(`registry.go`의 목록을 고치지 않는다).
- `cmd/sw/wire.go`의 `wiring{Server, Store, Admin, ConfigDir, DataDir, Go}`와 기능별 파일:
  `wire_access.go`(Codex), `wire_netsvc.go`(Gemini). `runServer`가 둘을 부르고 반환된 admission을
  `admission.Chain`으로 묶는다. 배경 루프는 `w.Go(name, fn)`(서버 수명 동안 실행, 저장소를 닫기 전에
  끝남). 노드마다 도는 네트워크 서비스는 `runNodeNetworkServices(ctx, store.Reader, gateway, logf)`.

### 1.3 겹칠 수 있는 공유 파일

| 파일 | 규칙 |
|---|---|
| `internal/api/table.go` | 새 종류의 표 열 추가만 |
| `internal/apis/types.go` | Claude 소유. Gemini는 `PodSpec.DNSConfig` 추가만 허용 |
| `cmd/sw/main.go` | Claude 소유. Codex는 `case "user":` 한 줄과 도움말 한 줄만 추가 |
| `docs/compatibility.md` | 자기 기능의 행만 고친다 |
| `internal/protobuf/` | 종류별 새 파일로 추가(기존 파일 수정은 등록 한 줄까지) |

## 2. 접근 제어 (Codex)

목표: 여러 사람·여러 AI 에이전트가 한 클러스터를 나눠 쓴다. 네임스페이스가 테넌트 단위다.

### 2.1 RBAC

- 자원(`rbac.authorization.k8s.io/v1`): Role, ClusterRole, RoleBinding, ClusterRoleBinding.
  쿠버네티스 필드 그대로: `rules[]{apiGroups, resources, verbs, resourceNames, nonResourceURLs}`,
  `roleRef{apiGroup, kind, name}`, `subjects[]{kind: User|Group|ServiceAccount, name, namespace}`.
  ClusterRole의 `aggregationRule`은 저장만(grades). 검증은 쿠버네티스와 같은 수준(빈 verbs 거절,
  roleRef 변경 불가 등).
- 평가기 `internal/rbac`: 와일드카드 `*`, 하위자원 `pods/log`, `resourceNames`, 비자원 URL(`/healthz`,
  `/version`, 끝 `*`), RoleBinding은 그 네임스페이스에서만, ClusterRoleBinding은 전부. User/Group
  주체를 Identity의 Name/Groups와 맞춘다. 저장소에서 바인딩·역할을 읽는다(요청마다 전체를 읽지
  말고 변경 시점에 캐시를 다시 만든다 — `Store.Changed`).
- 기본 ClusterRole(시작할 때 없으면 만듦, 라벨 `kubernetes.io/bootstrapping: rbac-defaults`):
  `cluster-admin`(전부), `admin`(네임스페이스 안 전부 + Role/RoleBinding 관리, ResourceQuota는 읽기),
  `edit`(워크로드·Service·Secret·Ingress·PVC 쓰기, RBAC 없음), `view`(Secret 제외 읽기).
  인증된 모든 사용자에게 SelfSubjectAccessReview·SelfSubjectReview `create`와 discovery를 허용.
- **권한 상승 방지**(admission): Role/ClusterRole 생성·수정은 요청자가 그 규칙을 모두 이미 가졌거나
  `escalate` 동사가 있을 때만. RoleBinding/ClusterRoleBinding은 참조한 역할의 규칙을 모두 가졌거나
  `bind` 동사가 있을 때만. 관리자는 건너뛴다.
- `authorization.k8s.io/v1` SelfSubjectAccessReview(`kubectl auth can-i`, `can-i --list`는
  SelfSubjectRulesReview도 필요), `authentication.k8s.io/v1` SelfSubjectReview(`kubectl auth whoami`).
  저장하지 않는 create 전용 자원이다. kubectl이 실제로 보내는 요청을 trace로 확인할 것
  (`sw server --trace`, `docs/kubectl-trace.md`).
- `kubectl create role|rolebinding|clusterrole|clusterrolebinding|quota`는 Protobuf 본문을 보낸다.
  `internal/protobuf`에 해당 디코더를 추가한다(새 파일).

### 2.2 사용자와 테넌트 네임스페이스

- 관리자 API `/shearwater/admin/v1/users`(GET 목록, POST 추가, DELETE `/users/<name>` 삭제, POST
  `/users/<name>/tokens` 토큰 재발급). `wiring.Admin`에 등록. 토큰은 만들 때 한 번만 응답에 나온다.
- CLI `sw user add NAME [--namespace NS] [--role admin|edit|view] [--cluster-role NAME] [--out FILE]`:
  사용자 자격 증명을 만들고, `--namespace`가 있으면 네임스페이스(없으면 생성, 라벨
  `shearwater.io/tenant=NAME`, `pod-security.kubernetes.io/enforce=baseline`)와 RoleBinding(라벨
  `shearwater.io/user=NAME`)을 만들고, 그 사용자용 kubeconfig를 쓴다(기본 네임스페이스 = NS).
  `sw user list`, `sw user remove NAME`(토큰 폐기 + 라벨 붙은 바인딩 삭제, 네임스페이스는 남김),
  `sw user token NAME`(재발급). CLI는 저장된 관리자 연결(`sw kubeconfig`와 같은 출처)로 관리자 API를
  부른다. DB 파일을 직접 열지 않는다.
- AI 에이전트용: kubeconfig 하나로 자기 네임스페이스만 다루게 하는 것이 목적. `docs/access-control.md`에
  "에이전트에게 제한된 kubeconfig 주기" 예시를 넣는다.

### 2.3 ResourceQuota (`v1`)

- 지원 키: `pods`, `services`, `secrets`, `persistentvolumeclaims`, `requests.storage`,
  `requests.cpu`, `requests.memory`, `limits.cpu`, `limits.memory`, `count/<resource>.<group>`
  (`count/deployments.apps` 등). 그 밖의 키는 거절하지 말고 경고 + 저장만.
- admission에서 생성(과 Pod 자원 변경) 시 같은 트랜잭션으로 현재 사용량을 세어 넘으면 403
  `exceeded quota: <name>, requested: ..., used: ..., limited: ...`(쿠버네티스 문구). 쿼터가
  `limits.cpu` 등을 추적하는데 Pod가 그 값을 안 주면 거절(쿠버네티스와 같음). 끝난 Pod(Succeeded/Failed)와
  삭제 중인 객체는 세지 않는다.
- `status.hard`/`status.used`는 배경 루프(`w.Go`)가 갱신한다.

### 2.4 Pod Security (네임스페이스 라벨 `pod-security.kubernetes.io/enforce`)

- `privileged`(검사 없음), `baseline`, `restricted`의 쿠버네티스 정의 중 Shearwater가 표현할 수 있는
  필드를 검사: hostPath 볼륨, hostNetwork/hostPID/hostIPC, privileged, 추가 capabilities, hostPort,
  (restricted) runAsNonRoot·allowPrivilegeEscalation=false·seccomp·capabilities drop ALL.
  Pod 생성과 Pod 템플릿을 가진 Deployment/ReplicaSet 생성·수정에 적용(쿠버네티스는 워크로드에는
  경고만 하지만, 여기서는 Pod가 나중에 조용히 실패하지 않게 워크로드도 거절할지 판단해서 문서에 적기).
- 라벨이 없으면 검사 안 함(지금 동작 유지). **테넌트 네임스페이스는 baseline**: hostPath로 노드를
  탈취하는 길을 막는 것이 핵심이다.

### 2.5 소유 파일

새 파일: `internal/rbac/**`, `internal/quota/**`, `internal/podsecurity/**`, `internal/apis/rbac.go`,
`internal/apis/quota.go`, `internal/apis/review.go`(리뷰 자원 타입), `internal/api/review.go`,
`internal/protobuf/rbac.go` 등, `cmd/sw/wire_access.go`(이미 있음), `cmd/sw/user.go`,
`docs/access-control.md`. 허용된 공유 편집: 1.3절 표, `internal/api/server.go`의 `serveAPI`에서 리뷰
자원 분기 몇 줄.

### 2.6 완료 기준

단위 테스트: 평가기(바인딩·와일드카드·하위자원·resourceNames·비자원), 상승 방지, 쿼터 계산·거절 문구,
Pod Security 수준별, 관리자 API, `api` 패키지의 실제 HTTP 경로로 "alice는 team-a만, team-b list는
403". `hack/t.sh` 통과. `docs/compatibility.md`의 "인증" 행 갱신.

## 3. 여러 서버 (Claude)

### 3.1 구성

- `sw server`(지금과 같음)가 컨트롤 플레인 + 자기 노드. 워커는 `sw join`으로 붙는다.
- 워커의 `sw`는 노드 에이전트 + 서비스 프록시 + DNS + (2단계) 네트워크 정책만 돌린다. SQLite가 없다.
  상태는 컨트롤 플레인의 **노드 API**(`/shearwater/node/v1/...`, HTTPS, 노드 자격 증명)로 읽고 쓴다.
- 노드 API는 `store.Reader` + 노드 에이전트가 쓰는 쓰기(낙관적 트랜잭션: 읽은 객체의 rev를 함께 보내고
  컨트롤 플레인이 같은 SQLite 트랜잭션에서 확인 후 반영, 충돌이면 다시 시도) + Pod IP 할당 + 변경 대기
  (long-poll)를 제공한다. 노드 제한(NodeRestriction): 노드는 자기 Node 객체, 자기에게 배정된 Pod의
  상태·종료, Event 생성만 쓸 수 있고, Secret은 자기 Pod가 참조하는 것만 읽는다.

### 3.2 join

- 컨트롤 플레인: `sw node token [--ttl 24h]` → `sw join https://<주소>:6443 --token swj_... --ca-hash sha256:...`
  한 줄을 출력. join 토큰은 해시로 저장, 만료 있음.
- 워커: `sw join URL --token ... --ca-hash ...` → CA 인증서를 받아 해시를 확인(kubeadm 방식) → 노드
  자격 증명·Pod CIDR을 받음 → `/etc/shearwater/node.json` 저장 → 이후 `sw up`이 워커 모드로 시작.
- `sw node list|remove NAME`, `kubectl get nodes`, `kubectl cordon|uncordon|drain`(Node 갱신, Eviction).

### 3.3 Pod 네트워크

- 클러스터 Pod 대역 `10.244.0.0/16`, 노드마다 `/24`(`Node.spec.podCIDR`). 컨트롤 플레인 노드는 지금의
  `10.244.0.0/24`를 그대로 쓴다(기존 설치 호환). Service 대역 `10.96.0.0/16` 그대로.
- 노드 사이는 **VXLAN**(장치 `sw-vxlan0`, UDP 8472, flannel과 같은 방식: 노드마다 VTEP MAC을 Node
  주석에 공개, 경로 `10.244.N.0/24 via 10.244.N.0 onlink` + 이웃·FDB 고정 항목). L3로만 닿아도 된다.
  **평문**이므로 사설망이나 VPN 위에서 쓰라고 문서에 적는다. 노드 사이 UDP 8472, 컨트롤 플레인
  6443이 열려 있어야 한다.
- 마스커레이드는 클러스터 대역 밖으로 나갈 때만. Pod DNS 서버 주소는 각 노드의 게이트웨이
  (`podCIDR`의 첫 주소, 컨트롤 플레인은 `10.244.0.1`).

### 3.4 스케줄러·노드 수명

- 필터: Ready·cordon, taint/toleration(NoSchedule·NoExecute), nodeSelector, 필수 nodeAffinity
  (In/NotIn/Exists/DoesNotExist/Gt/Lt, matchFields metadata.name), requests 합(cpu·memory·pods 110),
  (2단계) PV 노드 제약. 점수: 덜 쓰인 노드, 같은 소유자 Pod 분산, 선호 nodeAffinity.
  배치 실패 메시지는 쿠버네티스 형식(`0/3 nodes are available: 1 Insufficient cpu, 2 node(s) didn't match ...`).
- 하트비트 10초. 40초 동안 없으면 Ready=Unknown + taint `node.kubernetes.io/unreachable:NoExecute`,
  그 노드 Pod의 Ready=False(엔드포인트에서 빠짐). 300초 뒤(toleration 없으면) Pod를 삭제 표시 →
  ReplicaSet이 다른 노드에 새로 만든다. 노드가 돌아오면 에이전트가 남은 컨테이너를 정리한다.
- `kubectl logs`: 다른 노드의 Pod는 컨트롤 플레인이 그 노드 에이전트의 HTTPS 로그 엔드포인트
  (`:10250`, 클러스터 CA가 서명한 노드 인증서)로 중계한다.

### 3.5 소유 파일

`internal/nodeapi/**`, `internal/controller/**`, `internal/node/**`(Gemini의 resolv.conf 부분 제외),
`internal/runtime/**`, `internal/proxy/**`, `internal/objects/**`, `internal/api/log.go`, `internal/apis/types.go`,
`internal/apis/grades.go`, `cmd/sw/main.go`, `cmd/sw/node.go`, `cmd/sw/join.go`, 수명 관리 파일, `docs/multi-node.md`.

## 4. 네트워크 서비스 (Gemini)

### 4.1 클러스터 DNS (`internal/dns`)

- `sw` 안의 DNS 서버. 노드마다 `runNodeNetworkServices`에서 시작해 **그 노드의 게이트웨이 주소:53**
  (UDP와 TCP)에서 듣는다. 게이트웨이 주소는 브리지가 생긴 뒤에 생기므로 바인드가 실패하면 잠시 뒤
  다시 시도한다. 상태는 `store.Reader`로만 읽는다(워커에서도 같은 코드가 돈다). 변경 시점에 기록 표를
  다시 만들고, 질의마다 저장소를 읽지 않는다.
- 레코드(도메인 `cluster.local`):
  - `<svc>.<ns>.svc.cluster.local` A → ClusterIP. headless(`clusterIP: None`)는 Ready Pod IP들
    (EndpointSlice 기준).
  - `_<port>._<proto>.<svc>.<ns>.svc.cluster.local` SRV(이름 있는 포트).
  - `<a-b-c-d>.<ns>.pod.cluster.local` A.
  - PTR(`in-addr.arpa`)은 Service IP·Pod IP에 대해.
  - 없는 클러스터 이름은 NXDOMAIN, AAAA는 빈 응답(NOERROR). 그 밖의 이름은 호스트 업스트림으로 전달:
    `/run/systemd/resolve/resolv.conf`가 있으면 그것, 없으면 `/etc/resolv.conf`의 nameserver 중
    `127.0.0.0/8`이 아닌 것. 업스트림이 없으면 SERVFAIL.
  - 메시지 형식(RFC 1035: 헤더, 질문, 이름 압축 읽기·쓰기, EDNS0 OPT 무시 또는 그대로 크기 존중,
    UDP 512바이트 초과 시 TC 비트, TCP 2바이트 길이)은 직접 작성. 잘못된 패킷에 죽지 않게 퍼징
    수준의 단위 테스트.
- 짧은 TTL(5초). 질의 수 제한 없이 가볍게.

### 4.2 Pod resolv.conf (`internal/node/files.go`의 해당 부분)

- `dnsPolicy`: `ClusterFirst`(기본) → `nameserver <노드 게이트웨이>`, `search <ns>.svc.cluster.local
  svc.cluster.local cluster.local`, `options ndots:5`. `Default` → 지금처럼 호스트 파일 복사.
  `None` → `dnsConfig`만 사용. `dnsConfig`(nameservers, searches, options)를 지원하려면
  `PodSpec.DNSConfig`를 추가(types.go에 그 필드만). `ClusterFirstWithHostNet`은 hostNetwork를
  지원하지 않으니 ClusterFirst처럼.
- 게이트웨이 주소는 `Agent.PodCIDR`의 첫 주소(`PodCIDR.Addr().Next()`). 노드마다 다르다.
- 이미 만든 Pod의 resolv.conf는 다시 쓰지 않는다(지금 동작과 같게, 문서에 적기).

### 4.3 Ingress (`networking.k8s.io/v1`)

- 자원 Ingress, IngressClass. IngressClass `shearwater`(controller `shearwater.io/ingress`)를 시작할
  때 없으면 만들고 기본 클래스 주석(`ingressclass.kubernetes.io/is-default-class: "true"`)을 단다.
  `ingressClassName`이 비었거나 `shearwater`인 Ingress만 처리.
- 기존 공개 진입점(`internal/edge`, Service 주석 방식)을 확장: 라우팅 표가 호스트 → [경로 규칙] →
  Service:포트. pathType `Exact`/`Prefix`(경로 요소 단위 접두어, 쿠버네티스 규칙)/
  `ImplementationSpecific`(Prefix로 취급), 가장 긴 일치 우선, `defaultBackend`, 호스트 없는 규칙은
  모든 호스트. 백엔드 포트는 number 또는 name. Service 주석 방식은 그대로 동작해야 한다.
- TLS: `spec.tls[].secretName`이 있으면 같은 네임스페이스의 `kubernetes.io/tls` Secret 인증서를
  쓴다(교체되면 반영). 없으면 지금처럼 ACME가 그 호스트의 인증서를 받는다(edge가 켜져 있을 때).
- **호스트 소유권**(admission, `wireNetworkServices`): 다른 네임스페이스의 Ingress나 Service 주석이
  이미 쓰는 호스트를 가진 Ingress 생성·수정은 409 Conflict로 거절. 같은 네임스페이스 안 중복은
  허용(경로로 나눠 쓸 수 있게). 라우팅 표의 "먼저 만든 것이 이김" 규칙도 유지.
- `status.loadBalancer.ingress[].ip`에 edge가 듣는 주소를 쓴다(쓸 수 있을 때).
- 표 열: `NAME CLASS HOSTS ADDRESS PORTS AGE`.
- `kubectl create ingress`는 Protobuf 본문을 보낸다 → `internal/protobuf`에 디코더 추가(새 파일).

### 4.4 소유 파일

새 파일: `internal/dns/**`, `internal/apis/ingress.go`, `internal/edge/ingress.go`(등 edge 안 새 파일),
`internal/protobuf/ingress.go`, `cmd/sw/wire_netsvc.go`(이미 있음), `docs/dns.md`, `docs/ingress.md`.
허용된 편집: `internal/edge/routes.go`·`server.go`·`certs.go`, `internal/node/files.go`(resolv.conf 부분),
1.3절 표.

### 4.5 완료 기준

단위 테스트: DNS 메시지 왕복·압축·잘린 패킷·TC·SRV/PTR·업스트림 전달(가짜 업스트림), 레코드 표
(headless 포함), resolv.conf 정책별, Ingress 검증·경로 일치 규칙(쿠버네티스 문서 예시 표), TLS Secret
선택, 호스트 소유권 거절. `hack/t.sh` 통과. `docs/compatibility.md`의 Service·공개 진입점 행 갱신.

## 5. 영속 볼륨 (2단계)

PersistentVolume, PersistentVolumeClaim(`v1`), StorageClass(`storage.k8s.io/v1`). 기본 StorageClass
`local-path`(프로비저너 `shearwater.io/local-path`, `volumeBindingMode: WaitForFirstConsumer`): 첫
Pod가 배치된 노드의 `/var/lib/shearwater/volumes/<pv>`에 디렉터리를 만들고 PV에 노드 제약을 단다
(스케줄러가 이후 그 노드에만 배치). reclaimPolicy Delete/Retain, PVC 보호 finalizer, 용량은 강제하지
않음(문서). 정적 PV: hostPath·local(노드 제약 필수), (선택) NFS.

## 6. 네트워크 정책 (2단계)

NetworkPolicy(`networking.k8s.io/v1`) Ingress·Egress: podSelector, namespaceSelector, ipBlock,
ports(TCP/UDP). 노드마다 자기 Pod에 대한 iptables 규칙을 FORWARD에서(DNAT 뒤, 마스커레이드 전이라
원래 출발지 Pod IP가 보인다). 같은 노드 브리지 안 트래픽을 위해 `br_netfilter`와
`bridge-nf-call-iptables=1` 필요. 호스트(edge·DNS) 트래픽은 영향 없음.

## 7. QA (Claude)

QA 배포판에서 노드마다 별도 network namespace + 전용 containerd + cgroup(기존 하네스 방식)으로 서버
2~3대를 흉내 낸다. 확인: join, 분산 배치, 노드 사이 Pod·Service 통신, DNS, Ingress로 워커의 Pod 접근,
테넌트 격리(alice/bob kubeconfig), 쿼터·Pod Security 거절, 노드 장애 후 재배치, (2단계) PVC 데이터 유지·
노드 고정, 네트워크 정책 차단.
