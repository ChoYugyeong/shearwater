# 릴리스와 버전 확인

## v0.1.0-alpha.16

- **여러 서버.** 컨트롤 플레인 한 대에 워커를 붙여 써. [여러 서버](multi-node.md)
  - `sw up --advertise-address IP`로 시작하면 다른 서버와 원격 kubectl이 그 주소로 붙어(설정 폴더에 기억). `sw node token`이 붙일 명령을 출력하고, 워커에서 `sw join URL --token … --ca-hash …`을 실행해. 워커는 CA 해시를 확인한 뒤에만 붙어.
  - 워커에는 DB가 없어. 컨트롤 플레인의 노드 API로 상태를 읽고 쓰고, 노드는 자기 Node·자기 Pod 상태·그 이벤트만 쓸 수 있어. Secret은 자기 Pod가 쓰는 것만 읽어.
  - 스케줄러가 여러 노드에서 고른다. cordon, taint/toleration, nodeSelector, 필수·선호 nodeAffinity, 요청 자원·Pod 110개, 볼륨 노드 제약을 보고, 같은 소유자의 Pod를 나눠. 맞는 노드가 없으면 `0/3 nodes are available: …`.
  - 워커가 40초 응답하지 않으면 `NotReady`와 `node.kubernetes.io/unreachable` taint, 그 노드 Pod는 엔드포인트에서 빠지고 toleration 시간(기본 300초) 뒤 다른 노드에 다시 만들어져. 하트비트는 메모리에만 기록해(유휴 디스크 쓰기 없음).
  - 노드 사이 Pod 네트워크는 VXLAN(`sw-vxlan0`, UDP 8472, 평문). 노드마다 `10.244.0.0/16`의 `/24`. 다른 노드 Pod의 `kubectl logs`는 컨트롤 플레인이 중계해(TCP 10250, 상호 TLS).
- **접근 제어.** Role·ClusterRole·RoleBinding·ClusterRoleBinding과 권한 상승 방지, `kubectl auth can-i`·`can-i --list`·`whoami`, `sw user add|list|remove|token`과 테넌트 네임스페이스(Pod Security baseline), ResourceQuota(Deployment가 만드는 Pod 포함), Pod Security baseline·restricted. [접근 제어](access-control.md)
- **클러스터 DNS.** 노드마다 게이트웨이에서 `sw`가 답해: Service(ClusterIP, headless), 이름 포트 SRV, Pod 이름, PTR, 그 밖의 이름은 호스트 resolver로. Pod `resolv.conf`는 `dnsPolicy`/`dnsConfig`대로. [클러스터 DNS](dns.md)
- **Ingress.** `networking.k8s.io/v1` Ingress·IngressClass(`shearwater`), 호스트·경로 규칙, TLS Secret 또는 ACME. 다른 네임스페이스가 이미 쓰는 호스트는 409로 거절. [Ingress](ingress.md)
- **영속 볼륨.** PV·PVC·StorageClass, 기본 `local-path`(WaitForFirstConsumer)는 첫 Pod가 배치된 노드에 디렉터리를 만들고 이후 Pod도 그 노드로 보내. Delete 정책은 PVC를 지운 뒤에만 그 노드가 데이터를 지워. PV가 없는 디렉터리는 지우지 않아(오래된 백업을 복원해도 데이터가 남아).
- **NetworkPolicy.** Ingress·Egress, pod·namespace 셀렉터, ipBlock·except, TCP/UDP 포트를 노드마다 iptables(FORWARD)로 적용.

**업데이트:** DB 형식은 바뀌지 않아. 서버 실행 파일을 교체한 뒤 다음 세 곳의 버전을 확인해.
- `sw version`: `0.1.0-alpha.16`
- 서버 `/version`의 `gitVersion`: `v1.36.0+shearwater.0.1.0-alpha.16`
- Node의 `kubeletVersion`: `v1.36.0+shearwater.0.1.0-alpha.16`

바뀌는 동작: 관리자 토큰이 아닌 요청은 RBAC를 거치고, 새로 시작하는 Pod는 클러스터 DNS를 써. [설치와 기존 서버 업데이트](install.md)

**확인한 범위:**
- 단위·API 시험 전체(`hack/t.sh`)와 CI.
- QA 배포판에서 서버 세 대를 흉내 낸 9단계(서버마다 별도 network namespace·containerd·cgroup): join, 분산 배치, 워커 Pod에서 DNS·세 노드로 가는 Service·다른 노드 Pod IP·로그 중계, Ingress, 테넌트 격리·Pod Security·쿼터(Deployment 포함), NetworkPolicy, 워커의 PVC 데이터 유지와 해제 뒤 삭제, 워커 장애 때 44초 NotReady·66초 대체 Pod Ready(`tolerationSeconds: 20`).
- 실제로 서로 다른 서버, 클라우드 네트워크·방화벽에서는 아직 확인하지 않았어. 컨트롤 플레인 고가용성은 없어.

## v0.1.0-alpha.15

- **실패한 검사의 위치와 이유.** `http_failed` 결과의 사실(`facts`)에 어느 검사가 몇 개 사본에서 왜 실패했는지 적어. 예: `check GET / failed on 1 of 1 replicas: HTTP 200, expected text missing`. 앱의 앞 주소(Service 또는 공개 포트)도 따로 적어.
  - 기대한 문구 자체는 적지 않아. 비공개 검사 파일에서 올 수 있어서야.
  - `diagnose`도 같은 목표의 마지막 실패 검사를 시각과 함께 보여 줘.
- **저장된 증거의 결과.** `sw task evidence`의 `http`가 저장된 증거의 결과(`passed`/`failed`, `retained: true`, `checkedAt`, `goalGeneration`)야. 맨 위 `state`는 지금 상태를 다시 확인하기 전까지 `needs_verification`으로 남아. 저장된 기록은 실시간 검사가 아니기 때문이야.
- **공개 진입점 인증서 갱신.**
  - 인증서가 갱신 시기에 들어가면 갱신 반복이 바로 깨어나.
  - 갱신한 인증서는 이전 인증서의 남은 기간 절반(최대 1시간)이 지나 안전하게 유효해질 때까지 이전 인증서를 계속 써.
  - `sw edge status`가 만료된 인증서를 표시해.
  - 백업에 `edge.json`, ACME 계정 키, 인증서가 들어가.
- **시작 실패 표시.** `sw up`이 시작 중 실패하면 서버 로그의 마지막 줄을 보여 줘.
- **verify 사실 문구.** 백엔드마다 실제로 검사한 범위(엔진: 새 Pod와 내부 Service, Compose: 컨테이너와 공개 호스트 포트)를 적어. Compose에도 "Pod/internal Service"라고 쓰던 문구를 고쳤어.

**업데이트:** DB 형식은 바뀌지 않아. 서버 실행 파일을 교체한 뒤 다음 세 곳의 버전을 확인해.
- `sw version`: `0.1.0-alpha.15`
- 서버 `/version`의 `gitVersion`: `v1.36.0+shearwater.0.1.0-alpha.15`
- Node의 `kubeletVersion`: `v1.36.0+shearwater.0.1.0-alpha.15`

[설치와 기존 서버 업데이트](install.md)

**확인한 범위:**
- 실패 설명과 증거 표시: 단위 시험과, 새 GitHub 호스팅 VM의 실제 Docker·Compose에서 README 빠른 시작 경로(source 27/27).
- 인증서 갱신: 격리된 QA 환경에서 ACME 시험 서버 Pebble로 19단계, 240초 인증서로 실제 갱신까지.
- 실제 Let's Encrypt와 공인 IP 서버는 아직 확인하지 않았어.
- README의 업데이트·밀도 측정값은 alpha.13에서 잰 것이야. 4개 도구 비교는 [CI 비교](benchmarks/runtime-bench-ci.md)에 있어.

## v0.1.0-alpha.14

- **기존 Docker Compose 앱도 같은 위임 흐름으로.** `sw compose serve`를 Compose 프로젝트 앞에 띄우면, AI는 엔진과 같은 `sw task` 위임·정확한 계획 승인·실행 기록·컨테이너별 HTTP 증거로 서비스 하나를 업데이트하고 복구해.
  - 사용자의 compose 파일은 고치지 않아. 이미지와 라벨만 담은 override 파일로 `docker compose up --no-deps`를 실행해.
  - 이미지를 받지 못해 앱이 그대로인 실패는 계획을 닫아서 바로 복구 계획을 세울 수 있어.
  - 손으로 다시 `up`한 상태는 `stale`로 보고하고, 이 작업의 목표로 받아들이지 않아.
  - Compose는 업데이트 때 컨테이너를 새로 만들어서 짧은 끊김이 있을 수 있어. [Compose 안내](compose-backend.md)
- **실행 기록을 두 단계로.** DB 밖에서 변경하는 백엔드는 시작 기록 → 실행 → 결과 기록 순서로 남겨. 응답이 사라지거나 서버가 죽어도 다음 `continue`가 같은 계획을 이어 가고, 두 번 적용하지 않아. 엔진은 예전처럼 한 트랜잭션에 적용과 기록을 함께 저장해.
- **공개 진입점.** Service에 `shearwater.io/public-host` 주석을 달면, `sw server`가 호스트의 80·443으로 받아 그 Service에 넘겨. 인증서는 ACME HTTP-01로 받고 갱신해. 기본으로 꺼져 있고 `sw edge enable`로 켜. [공개 진입점 안내](public-entry.md)
- **Secret.** v1 Secret(`data`, 쓰기 전용 `stringData`, `type`, `immutable`)을 받고, 컨테이너의 `env[].valueFrom.secretKeyRef`와 `envFrom[].secretRef`로 값을 넣어. `kubectl create secret generic`이 보내는 Protobuf 본문도 읽어.
  - 값은 Kubernetes 기본값처럼 base64로 저장할 뿐 암호화하지 않아.
  - 표 출력과 개발용 trace에는 값이 나오지 않아. [호환표](compatibility.md)
- **새 서버 설치를 한 번에.** `sudo python3 install.py --version v0.1.0-alpha.14 --host`는 미리보기를 보여 주고, `--apply`로 실제 설치를 해.
  - containerd가 없을 때만 배포판의 containerd·runc를 설치해. Docker 같은 기존 containerd는 재사용해.
  - IPv4 전달을 영구 설정하고, `/opt/shearwater/releases`에 설치한 뒤 systemd 서비스를 시작해. [설치 안내 0절](install.md)
- **kubectl `kind: List`.** 검증을 켠 채 `kind: List` 파일을 `kubectl apply`할 수 있어. 각 항목은 서버가 `fieldValidation=Strict`로 검증해.
- **일반 사용자 명령.** `sw setup` 뒤에는 `sw status`·`get`·`observe` 같은 읽기 명령이 옵션 없이 저장된 연결을 써.

**업데이트:** DB 형식은 바뀌지 않아. 서버 실행 파일을 교체한 뒤 다음 세 곳의 버전을 확인해.
- `sw version`: `0.1.0-alpha.14`
- 서버 `/version`의 `gitVersion`: `v1.36.0+shearwater.0.1.0-alpha.14`
- Node의 `kubeletVersion`: `v1.36.0+shearwater.0.1.0-alpha.14`

[설치와 기존 서버 업데이트](install.md)

**확인한 범위:** 아래는 모두 이 PC의 격리된 QA 환경과 로컬 Hyper-V VM에서 확인한 거야. 공인 IP 서버, 실제 Let's Encrypt 발급, 언어 모델이 Compose 흐름을 끝까지 따르는지는 아직 확인하지 않았어.
- 실제 kubectl로 Secret 7단계.
- 실제 dockerd·Compose로 17단계.
- Let's Encrypt 시험용 서버 Pebble로 공개 진입점 14단계.
- 새 Ubuntu 24.04 VM에서 설치 도우미로 첫 HTTP 200, 재실행 시 무변경, 재부팅 복구.
- README의 업데이트·밀도 측정값은 alpha.13에서 잰 것이고 alpha.14에서 다시 재지 않았어.

## v0.1.0-alpha.13

- **새 Pod의 준비 검사.** `startupProbe`가 없으면 새 컨테이너의 `Running` 상태를 기록한 뒤 첫 `readinessProbe`를 실행해. 시작 직후 검사에 실패하면 100ms, 200ms, 400ms 간격으로 최대 세 번 다시 시도해. 첫 성공 뒤에는 설정한 주기로 검사하며, Service는 지정한 연속 성공 횟수를 채운 Pod로 트래픽을 보내.
- **호스트에서 Pod로 직접 연 TCP 연결.** Linux의 기본 conntrack `TIME_WAIT`가 120초이면, Shearwater 브리지 게이트웨이에서 Pod IP로 직접 연 IPv4 TCP 연결에 한해 이 상태의 추적 시간을 30초로 설정해. 호스트에서 따로 지정한 시간과 Service를 경유하는 연결에는 기존 정책이 적용돼. [준비 검사와 연결 추적의 적용 조건](connection-continuity.md)

두 변경은 서버에 적용돼. 업데이트 뒤 `sw version`에서 `0.1.0-alpha.13`을, 서버 `/version`의
`gitVersion`과 Node의 `kubeletVersion`에서 `v1.36.0+shearwater.0.1.0-alpha.13`을 확인해. [설치와 기존 서버 업데이트](install.md)

10개 사본의 교체 시간과 120 Pod 부하 수치는 공개 alpha.12와 **개발 후보**를 비교한 결과야.
최종 배포 파일의 소스 커밋·SHA-256과 그 파일로 다시 측정한 수치는 발행 시 릴리스 본문에 기록해. [후보 측정 조건과 결과](connection-verification.md) · [alpha.13 릴리스 본문](https://github.com/ChoYugyeong/shearwater/releases/tag/v0.1.0-alpha.13)

## v0.1.0-alpha.12

`sw task`로 한 앱의 UID·접속 경로·허용 동작·만료를 서버에 고정한 제한 토큰을 발급해.
AI가 관리자 인증을 갖지 않아도 계획·검증·승인된 실행을 이어갈 수 있어. 다른 앱·원시 API·
자기 승인·권한 확대 요청은 서버가 거부해. 실제 적용에는 별도 운영자가 정확한 계획 해시를
검토하고 승인해야 해. [초보자 위임 안내](agent-operations.md)에 전체 순서와 한계를 적었어.

앱 변경과 적용 기록을 같은 SQLite 트랜잭션에 저장해 응답 유실·서버 재시작·모델 교체 뒤
중복 적용 없이 이어가. 새 Pod마다와 고정 Service를 직접 HTTP 검사하고, 실제 실행 신원과
검사 내용을 대조해 완료를 판단해. 준비 상태를 기다리는 조회에서도 승인한 적용 기록을 유지해.
Qwen의 빈 응답과 검증 완료 뒤 반복 호출을 제한하고, MCP 목록은 연결 중 유지해.

서버와 클라이언트 모두 alpha.12가 필요해. 기존 관리자 인증·작업 파일을 자동으로 제한 연결로
바꾸지 않아. 첫 위임 API 요청 때 SQLite에 내부 `agent_operations` 표 하나를 추가해.
기존 앱 선언·컨테이너·Service 전달·Windows 중계는 유지해. 이전 실행 파일로 복귀해도
현재 DB를 오래된 백업으로 덮어쓰지 마. 이미지 복구는 앱 데이터 이전을 되돌리는 기능이 아니야.

[개발 중 39사례와 실패 기록](agent-operations-verification.md)은 일반적인 속도·토큰 절약을
입증하지 않아. 최종 공개 실행 파일의 출처·검사·운영 보존은
[릴리스 본문](https://github.com/ChoYugyeong/shearwater/releases/tag/v0.1.0-alpha.12)에 따로 남겨.
CLI·API·Node 제품 버전은 `0.1.0-alpha.12`야.

## v0.1.0-alpha.11

기존 앱의 Service 이름·라벨·포트가 기본 예제와 달라도 실제 연결 경로를 발견하고,
Deployment와 그 소유 Pod·EndpointSlice가 맞는지 확인한 뒤 고정해. 선택이 모호하면
Service와 포트를 지정해야 하고, 승인한 뒤 연결 경로가 바뀌면 적용 전에 멈춰.
기존 앱의 라벨을 바꾸어야 했던 alpha.10 workflow의 제약을 줄였어.

`sw setup`은 서버 준비와 일반 사용자 인증을 묶어. 기본은 미리보기이며 `--apply`로
실행해. 공개 준비 상태 응답뿐 아니라 인증이 필요한 API까지 검사하고, 기존 사용자
인증을 다른 서버의 인증으로 덮어쓰지 않아. `workflow mcp-config`가 Linux와 Windows용
AI 연결 명령을 만들며 `install.py`는 릴리스 압축과 내부 실행 파일 체크섬을 확인해.

[처음 설치·AI 연결](onboarding.md)과 [실제 세 모델 검증](onboarding-verification.md)에
명령, 구조, 실패와 한계를 설명했어. 세 모델 검증은 개발 단계의 모델별 한 사례이며
일반적인 정확도·속도 우위를 보장하지 않아. 릴리스 재빌드와 운영 교체의 실측은
[릴리스 본문](https://github.com/ChoYugyeong/shearwater/releases/tag/v0.1.0-alpha.11)에 별도로 남겨.

CLI·API·Node 버전은 `0.1.0-alpha.11`이야. SQLite 스키마·Pod 실행·Service 전달·
Windows 중계는 alpha.10과 같아. 새 작업 파일로 다시 연결해야 기존 앱의 확장된 경로
검사를 사용할 수 있어. 기존 작업 파일을 자동으로 다른 대상으로 옮기지 않아.
관리자 토큰은 여전히 전체 로컬 권한이며, 앱 고정은 사용자별 권한 분리(RBAC)가 아니야.

## v0.1.0-alpha.10

**기존 앱 연결 제한:** 새 workflow의 HTTP 검사는 표준 Service 선택 규칙
`app.kubernetes.io/name: 앱이름`을 요구해. `app: 앱이름` 등 다른 규칙의 기존 앱은
연결·진단이 가능해도 이 HTTP 검사를 통과하지 않아. 연결 성공을 전체 지원으로 해석하지 말고
[기존 앱의 제한과 대안](agent-workflow.md#기존-앱을-연결할-때의-alpha10-제한)을 먼저 봐.

`sw connect`로 서버·앱·확인할 HTTP 주소를 한 번 고정한 뒤, `sw workflow`와
AI용 `sw_workflow` 도구 하나로 배포·업데이트·장애 진단·복구를 이어 갈 수 있어.
Qwen 내장 대화와 외부 MCP가 같은 작업 기록을 사용해. AI는 다음 행동을 고르지만,
변경은 사람이 로컬에서 정확한 계획을 확인하고 승인해야 실행돼.

새 이미지는 작은 JSON 파일로 넘겨. 모델이 주소를 다시 써서 틀리거나 다른 앱으로
대상을 바꾸는 것을 막고, 이미 실행한 계획은 응답이 끊겨도 자동 재실행하지 않아.
복구에는 실제 HTTP 응답으로 확인한 이전 이미지만 사용해. 이미지 복구는 데이터베이스나
볼륨을 과거 시점으로 되돌리는 기능이 아니야. [작업 흐름](agent-workflow.md)에
준비 조건·승인 방법·중간에 멈춘 경우의 처리와 한계를 설명했어.

[선택 기준](choosing-shearwater.md)은 Compose·K3s와 다른 운영 도구의 차이를,
[첫 체험](agent-workflow-trial.md)은 별도 시험 앱의 배포부터 복구까지 안내해.
AI 비교 실험은 특정 모델·앱·도구 구성에서의 관측이야. 모든 작업에서의 우위나
외부 사용자의 만족도를 증명하지 않으며, 다른 제품의 MCP 지원도 함께 비교해.

CLI·API·Node 제품 버전은 `0.1.0-alpha.10`이야. SQLite 스키마·Pod 실행·Service 전달·
Windows 중계 코드는 alpha.9와 같아. [설치 안내](install.md)의 백업·교체 순서를 따라.
공개 소스 재빌드·모델 연결·운영 보존 검사의 실제 결과는 GitHub 릴리스 본문에 기록해.

## v0.1.0-alpha.9

AI가 만든 계획을 사람이 검토·승인한 뒤, 새 대화에서 그 계획 하나만 실행하도록
`chat --allow-write --approved-plan=HASH`를 추가했어. 시작할 때 계획 해시·유효 기간·
접속 주소·인증 정보·로컬 승인 기록을 확인해. 일반 한 질문 대화에는 사용할 수 없는
적용 도구를 보여 주지 않고, 승인 실행 대화에는 적용·영수증·원래 목표 검증만 보여 줘.
응답이 불확실하더라도 같은 계획을 자동으로 다시 적용하지 않아.

`--target=deployment/이름 --task=diagnose`는 앱의 namespace·UID·세대를 고정하고
실제 소유 관계로 이어진 Pod의 오류를 짧게 모아 줘. 다른 앱의 비슷한 이름이나 라벨을
따라가지 않고, 대상이 바뀌면 `stale`로 멈춰. 실제 HTTP 응답을 확인하지 않았다면
준비 상태와 웹 접속 성공을 구분해. 로그·환경변수·annotation은 이 진단에 포함하지 않아.

Linux 묶음에는 배포·업데이트·진단용 `skills/shearwater-ops/`와 초보자용
[작업별 운영 안내](agent-skills.md)가 들어 있어. Qwen은 내장 대화, DeepSeek 등 기존
AI 앱은 stdio MCP로 연결해. 모델별 답변 품질을 보장하거나 사람의 승인을 대신하지 않아.

CLI·API·Node 제품 버전은 `0.1.0-alpha.9`야. SQLite 스키마·앱 실행·Service 전달·
Windows 중계 코드는 alpha.8과 같아. [설치 안내](install.md)의 백업·교체 순서를 따라.
공개 소스 재빌드·실제 모델 연결·기존 앱 보존 검사의 실행 결과는 GitHub 릴리스 본문에 기록해.

## v0.1.0-alpha.8

AI가 필요한 사실을 읽고, 작은 변경 계획을 만들고, 원래 대상의 결과를 확인하는 흐름을
추가했어. `observe`, `capabilities`, `verify`, `app-plan`, `chat`과 외부 MCP가 같은
운영 함수를 사용해. [전체 흐름](agent-interface.md)과 [집중 모드](focused-tools.md)를 봐.

이미 앱을 골랐다면 `--target=deploy/NAME -n NAMESPACE --task=inspect`로 조회 도구1개만,
`--task=image|scale`로 선택한 계획까지2개만 보여 줄 수 있어. namespace·원래 UID·세대를
프로그램이 고정하고, 재생성·세대 변경·숨긴 도구·스키마 밖 인자를 거부해.
계획이 저장돼도 적용된 것은 아니며 사람의 정확한 로컬 승인이 계속 필요해.

집중 응답의 `answer`에는 현재 ID와 완전한 JSON만 담아. 내장 Qwen 대화는 이번 질문에서
성공한 도구의 JSON을 직접 표시해 과거 ID 전사·해시만 있는 답변을 피하고, 오류나 새 질문
뒤에는 앞의 결과를 재사용하지 않아. 외부 MCP 앱의 모델 최종 문장까지 보장하는 기능은 아니야.
일반 조회 도구11개·쓰기 포함14개와 집중 모드1~2개를 구분해 사용해.

Windows Ollama에 연결하는 `chat-windows.py`와 AI 안내2편도 체크섬과 함께 동봉해.
Qwen은 로컬 Ollama, DeepSeek 등 기존 AI 앱은 stdio MCP로 연결할 수 있어.
모델 설치·API 키 발급·자동 승인·소스 이미지 빌드를 대신하는 기능은 아니야.

제품 버전은 CLI·API·Node 모두 `0.1.0-alpha.8`이야. SQLite 스키마, Pod 실행,
Service 전달과 Windows 중계 코드는 alpha.7과 같아. 기존 설정·데이터 경로를 유지하고
서버만 교체할 수 있어. [설치 안내](install.md)의 백업·교체 절차를 따라.
공개 소스 빌드·실제 연결·운영 보존 검사의 범위와 결과는 GitHub 릴리스 본문에 기록해.
개발 중 반복 실험이나 이전 재부팅 검사를 이번 릴리스의 재실행으로 합산하지 않아.

## v0.1.0-alpha.7

QA 도구와 운영 설명을 보완한 릴리스야. `m3-check.sh`와 `measure.sh`가
`SW_CONTAINERD_SOCKET`으로 지정한 containerd를 사전 확인·서버 실행·관측·정리에
일관되게 사용해. M2/M3는 containerd 조회 실패를 빈 런타임으로 오인하지 않고,
containers와 tasks가 모두 비어 있는 것을 확인한 뒤에만 검사를 시작해.
소켓 선택과 별개로 기존 앱과 네트워크도 격리해야 해. [운영 안내](operations.md)를 봐.

[호환표](compatibility.md)의 서버 중단 설명도 alpha.5 이후 커널 Service 전달 유지
동작과 그 한계에 맞췄어. `sw version`·API·Node의 제품 버전은 `0.1.0-alpha.7`이야.
SQLite 스키마·Pod 실행·네트워크·Windows 중계 코드는 alpha.6과 같아.
기존 경로와 인증·노드 옵션을 유지해 서버만 교체하며, 같은 코드의 Windows 중계는
다시 설치할 필요가 없어. 업데이트 전에 DB·인증·앱 자료와 서비스 설정을 백업해.

이번 공개 소스 빌드·첨부 파일 검증과 운영 인계 결과는 GitHub alpha.7 릴리스 본문에
기록해. 이전 릴리스의 WSL 재부팅·장시간 부하 실험을 이번 결과로 합산하지 않아.

## v0.1.0-alpha.6

Windows에서만 localhost 연결이 멈추는 경우를 위한 선택적 TCP 중계와 자동 감독
프로그램을 공개해. 앱 연결마다 WSL/Hyper-V 소켓을 만들지 않고 WSL의 NAT IPv4로
전달해. 로그인 시 감독 프로그램을 시작하는 예약 작업 설치 도구, 중복 실행 잠금,
종료 후 대기·재연결, 제한된 로그와 상태 조회를 함께 제공해.

Linux/amd64 실행 파일과 별도의 `windows-forward.zip`을 SHA-256 목록과 함께
배포해. Windows용 네이티브 sw 실행 파일은 아니며 양쪽에 Python 3.11 이상이
필요해. [접속 원리·한계](windows-localhost.md), [설치·중지·복구](automatic-start.md)를 봐.
Linux systemd가 sw를 시작하고 Windows 감독 프로그램이 WSL과 중계를 유지해.
Windows 로그인이 되기 전부터 실행하는 시스템 서비스는 아니야.

`sw version`·API·Node의 제품 버전은 `0.1.0-alpha.6`으로 맞췄어. SQLite 스키마와
Pod 실행·Service 전달 방식은 alpha.5와 같아. 기존 경로와 설정으로 서버만
교체해 앱을 인계하고, 중계 패키지는 별도의 새 버전 폴더로 교체해.
개발 단계 재부팅 기록을 이번 릴리스 실측으로 합산하지 않아. 공개 소스 재빌드,
첨부 파일 다운로드 검증과 실제 운영 재부팅 결과는 GitHub alpha.6 릴리스 본문에 기록해.

## v0.1.0-alpha.5

Service TCP 전달을 Linux 커널 DNAT·conntrack으로 옮겨 같은 호스트에서 sw만 멈춰도 살아 있는 Pod로 연결을 전달해. 롤링 교체·ReplicaSet 축소는 새 연결을 제외한 다음 기존 Service 연결을 최대 30초 기다리고 원래 종료 유예를 시작해. Pod 자기 Service 연결, Service 삭제와 IP 재사용, 규칙/종료 대기의 일관된 상태 확인도 보완했어.

[연결 유지 안내](connection-continuity.md)에는 원리·제한·진단 방법을, [개발 검증 기록](connection-verification.md)에는 동일 후보의 정상 HTTP 112,000건/오류0 및 시험 방식 보완 내역을 남겼어. 이 수치를 릴리스 바이너리의 재검증과 섞지 마. 최종 공개 소스·실행 파일의 재검증 결과는 GitHub alpha.5 릴리스 본문과 빌드 정보에 기록해.

**첫 교체의 경계:** alpha.4 프로세스가 소유한 기존 소켓은 이전되지 않아. 최초 교체 중 연결은 닫힐 수 있어. 이후 서버 재시작에도 유지되는 전달 방식과 구분해야 해. 30초보다 긴 연결, Pod 직접 IP, 강제 삭제·앱/호스트 장애는 무중단 보장 대상이 아니야. schema1·기존 Pod·앱 데이터·인증은 같은 경로로 인계해. 이전 버전으로 복구할 때는 새 Service 커널 규칙도 원복해야 하므로 파일 교체만으로 충분하다고 가정하지 마.

## v0.1.0-alpha.4

자동 시작과 복구를 위한 `sw service`, 일관된 DB·인증 백업과 새 폴더 복원을 위한
`sw backup/restore`, HTTP/HTTPS·TCP·exec 방식의 startup/readiness/liveness 프로브를 추가했어.
`sw mcp`는 기본 조회 전용이고, `--allow-write`를 붙이면 계획·배포를 연결할 수 있어.
배포는 `sw plan`의 정확한 내용을 확인해 `sw approve`로 로컬에서 승인한 뒤
`sw apply`로 한 번 실행해. 여러 객체의 변경은 원자적 트랜잭션이 아니며 부분 결과를 기록해.

처음 쓰면 [자동 시작](automatic-start.md), [백업과 복원](backup-restore.md),
[상태 검사](health-probes.md), [AI 연결과 승인](agent-mcp.md)을 순서대로 봐.
개발 당시 검사 범위는 [운영 기능 실측](next-verification.md), 최종 공개 소스로 다시 빌드한
릴리스 파일의 검사 결과는 GitHub alpha.4 릴리스 본문에 기록해.

**업데이트 전 확인:** alpha.3에서 무시하던 기존 DB의 프로브도 이제 실행돼.
기존 Pod/Deployment의 프로브 경로·포트·명령·시간 제한을 먼저 확인해.
새 프로브가 잘못된 앱을 재시작하는 것을 실행 파일 교체 자체의 장애와 혼동하지 마.
SQLite 스키마는 1을 유지하고, 프로브 없는 앱의 기존 태스크를 인계할 수 있어.
alpha.3로 실행 파일을 되돌리면 새 프로브 정책도 실행되지 않는다는 점을 기억해.

systemd는 Linux/WSL 배포판 안의 sw를 관리해. Windows 로그인 때 WSL을 켜거나
배포판을 계속 살아 있게 만드는 기능은 아니야. MCP도 OS 권한 분리나 RBAC를 제공하지 않아.
백업의 볼륨 포함 조건·제외 항목과 각 기능의 제한은 위 안내에 명시했어.

## v0.1.0-alpha.3

`sw get`, `logs`, `events`, `why`를 추가했어.
기존 API를 읽기만 하며 대상 UID와 컨테이너 실행 시도를 확인해 진단 기록이
섞이지 않도록 해. 명령 사용법·제한은 [문제 해결 안내](troubleshooting.md),
개발 단계의 실제 장애와 회귀 검사 결과는 [검증 기록](diagnostics-verification.md)에 있어.
alpha.3 릴리스 파일을 다시 빌드한 뒤 수행한 검사는 GitHub 릴리스 본문에 별도로 기록해.

`get`은 요약/전체 JSON, `logs`는 한 번 읽는 현재/직전 로그, `events`는 UID별 최근 기록,
`why`는 관찰한 상태·이벤트·제한된 로그와 다음 확인을 제공해. `logs`/`why`의 대상은
현재 Pod이며 Ready가 앱 응답을 보장하는 것은 아니야. `why`의 종료 코드 0은 진단을
수집했다는 뜻이고 앱이 정상이라는 뜻은 아니야.

이번 버전은 SQLite 스키마·Pod 실행·네트워크 방식을 바꾸지 않았어. 기존 서버의
실행 파일을 바꿀 때는 [설치 안내의 업데이트 절차](install.md)를 따라 같은 데이터·
인증·노드 옵션을 쓰고 서버만 교체해. 기존 alpha.1/alpha.2 태그와 파일은 그대로 유지해.

## v0.1.0-alpha.2

첫 알파의 실행 파일은 GitHub 릴리스 이름이 `v0.1.0-alpha.1`이었지만
`sw version`에 개발 초기 값인 `0.0.1`을 표시했어. 이번 릴리스는 제품 버전을
`0.1.0-alpha.2`로 고쳐서 CLI, 도움말, 서버 시작 로그, API `/version`, Node의
버전 정보에 같은 제품 버전을 사용해. 컨테이너·네트워크 동작은 변경하지 않았어.

```text
sw 0.1.0-alpha.2 (Kubernetes API v1.36.0+shearwater.0.1.0-alpha.2)
```

앞의 `0.1.0-alpha.2`가 **Shearwater 제품 버전**이야. 뒤의 `v1.36.0`은
**호환 대상으로 삼은 Kubernetes API 수준**이야. kubectl이 서버 호환성을
판단하므로 제품 버전으로 바꾸지 않아. `+shearwater...`는 어느 Shearwater
버전이 그 API를 제공하는지 알려 주는 부가 정보야. Kubernetes 전체 기능을
구현했다는 뜻은 아니야. [호환표](compatibility.md)에 실제 지원 범위가 있어.

`sw version`은 지금 실행한 **파일**을 확인해. 실행 중인 **서버**는
`kubectl --kubeconfig /실제/kubeconfig get --raw /version`으로 확인해.
새 파일을 받기만 하면 이전 서버가 자동으로 교체되지는 않아.
[설치 안내](install.md)에 다운로드·체크섬·실행 순서와 업데이트 방법이 있어.

## 다음 릴리스를 만드는 개발자에게

제품 버전의 기준값은 `internal/api/server.go`의 `Version` 한 곳이야.
Git 태그에만 붙는 앞의 `v`는 그 값에 넣지 않아. Git이 없는 소스 압축에서도
같은 버전으로 빌드되도록 링크 옵션이나 Git 태그 자동 추측은 사용하지 않아.

1. 기준값과 현재 설치 안내·README의 릴리스 번호를 함께 올려.
2. WSL/Linux에서 아래 검사를 실행해. `--tag`에는 **실제로 발행할 태그**를 넣어.
3. 공개 커밋의 CI가 통과한 뒤, 그 공개 소스를 새로 받아 배포할 파일을 빌드해.
   같은 태그 검사를 다시 하고 `go version -m`의 revision과 modified=false를 확인해.
4. 실제 배포할 파일로 격리된 환경의 런타임 검사를 하고, 라이선스 원문·예제·
   설치 안내·빌드 정보와 함께 압축해. SHA-256은 최종 압축을 만든 뒤 계산해.
5. 새 prerelease에 올리고 다시 다운로드해서 체크섬과 `sw version`을 확인해.
   기존 릴리스의 태그나 파일을 덮어쓰지 않아.

```bash
. hack/env.sh
bash hack/t.sh ./...
python3 -m unittest discover -s hack -p 'test_*.py'
bash hack/build.sh
python3 hack/check_version.py dist/sw --tag v0.1.0-alpha.13
python3 hack/notices.py
```

`check_version.py`는 실행 파일의 `version`, `--version`, `--help`와 API 버전
메타데이터가 일치하는지 검사해. 이전 버전의 실행 파일을 새 태그로 발행하려면
종료 코드 1로 실패해. HTTP `/version` 응답도 Go 테스트에서 따로 검사해.
일반 브랜치 CI는 표시 일관성을 검사하고, 태그 CI는 `GITHUB_REF_NAME`까지
대조해. 태그 CI는 태그를 올린 뒤 실행되므로 발행 전 `--tag` 검사를 생략하면 안 돼.

알파의 지원 환경은 WSL 2 / Ubuntu 24.04 / Linux amd64야. 이번 변경에 대해
실행한 검사와 생략한 검사는 GitHub 릴리스 본문에 기록해. 이전 재부팅·장시간
부하 기록은 [별도 문서](reboot-soak.md)에 있으며 새 릴리스에서 반복했다는 뜻은 아니야.
