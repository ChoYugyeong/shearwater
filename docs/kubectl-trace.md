# kubectl이 실제로 보내는 요청 (실측)

- 날짜: 2026-11-10 · kubectl v1.36.6 (linux/amd64, WSL)
- 방법: 요청을 기록하고 최소 응답만 돌려주는 임시 가짜 서버(파이썬, 평문 HTTP 127.0.0.1:18080)에 `kubectl --server=... --token=...`으로 붙였다. 가짜 서버는 프로젝트 코드가 아니고 저장소에 넣지 않았다.
- 한계: 가짜 서버가 최소 응답만 하므로 "kubectl이 무엇을 보내는가"만 확인했다. "어떤 응답이면 kubectl이 만족하는가"는 일부만 확인했고, 그런 항목은 따로 표시했다.

## 결과 요약

| # | 확인한 것 | 결과 | 설계에 주는 영향 |
|---|---|---|---|
| 1 | discovery | `/api`, `/apis`에 집계 형식을 먼저 요청하되 `application/json`도 받는다. 비집계 문서를 주면 `/api/v1`, `/apis/apps/v1`을 따로 읽고 정상 진행한다(`kubectl get pods` 종료 코드 0) | 비집계 discovery만으로 된다. 확정 |
| 2 | 표 출력 | `get`은 `Accept: application/json;as=Table;v=v1;g=meta.k8s.io,application/json;as=Table;v=v1beta1;g=meta.k8s.io,application/json`, 쿼리 `limit=500` | Table 구현 필요. 확정 |
| 3 | 평문 HTTP와 토큰 | `--token`을 줬는데도 모든 요청에 `Authorization` 헤더가 **없었다** | 처음부터 TLS로 간다. 확정 |
| 4 | `create -f`, `apply -f`의 검증 | 먼저 `GET /openapi/v3`, 이어서 `GET /openapi/v3/<그룹버전>?hash=...`를 읽는다. 둘 다 404면 `failed to download openapi`로 **실패**한다. 해당 종류의 patch 연산에 `x-kubernetes-group-version-kind`와 쿼리 파라미터 `fieldValidation`이 있는 최소 문서를 주면 `fieldValidation=Strict`를 붙여 진행한다 | `/openapi/v3` 최소 문서가 필수. 확정 |
| 5 | `/openapi/v2` | 두 번째 `apply`(패치 경로)에서 `GET /openapi/v2`(Protobuf 형식 요청)도 읽는다. 404를 줘도 패치를 보내고 종료 코드 0 | v2는 없어도 된다 |
| 6 | `apply` 첫 번째 | `GET` 대상 → 404면 `POST`(JSON, `last-applied-configuration` 주석 포함, `fieldManager=kubectl-client-side-apply`) | |
| 7 | `apply` 두 번째 | `PATCH`, `Content-Type: application/strategic-merge-patch+json`. 본문에 `$setElementOrder/containers`, `$setElementOrder/env`가 들어 있다(이미지 변경 + 환경변수 하나 삭제의 경우) | strategic merge patch와 지시어 구현 필수. 확정 |
| 8 | `create deployment NAME --image=...` | **Protobuf 본문**을 보낸다(`Content-Type: application/vnd.kubernetes.protobuf`, `Accept: application/vnd.kubernetes.protobuf,application/json`). 서버가 415를 주면 JSON으로 다시 시도하지 **않고 실패**한다 | 새 쟁점. 아래 참고 |
| 9 | `run`, `expose` | JSON 본문. `fieldValidation` 쿼리 없음. 본문에 `dnsPolicy`, `resources: {}`, `status: {}`가 들어 있다 | `dnsPolicy`를 저장만 등급에 넣는다. 생성 요청의 `status`는 버린다 |
| 10 | `scale` | `GET` 대상 → `PATCH .../deployments/web/scale`, `Content-Type: application/merge-patch+json`, 본문 `{"spec":{"replicas":3}}` | `/scale`에 merge patch 필요. 확정 |
| 11 | `rollout status` | watch-list를 쓴다: `watch=true&sendInitialEvents=true&resourceVersionMatch=NotOlderThan&allowWatchBookmarks=true&fieldSelector=metadata.name=web&timeoutSeconds=...`. 스트림이 바로 닫히면 `limit=500&resourceVersion=0` list와 `resourceVersion=N` watch로 내려간다 | `sendInitialEvents` 구현 필요. 확정 |
| 12 | `describe pod` | 대상 `GET`(Accept에 Protobuf와 JSON), 이어서 `GET /api/v1/namespaces/default/events?fieldSelector=involvedObject.name=...,involvedObject.namespace=...,involvedObject.uid=...&limit=500`. JSON으로 응답해도 정상 출력 | 응답은 JSON만으로 된다. events의 fieldSelector 세 가지 필요 |
| 13 | `logs` | 대상 `GET` → `GET .../pods/p1/log?container=c&tailLines=5` | |
| 14 | `delete` | `DELETE`, JSON 본문 `{"propagationPolicy":"Background"}` | |

## 새 쟁점: `kubectl create deployment`의 Protobuf 본문

`kubectl create deployment`(과 같은 계열로 보이는 `create service`, `create namespace` 등. 이 둘은 실측하지 않았다)은 요청 본문을 Protobuf로 보내고, 415를 받아도 JSON으로 바꾸지 않는다. minikube 첫 예제가 `kubectl create deployment`라서 "kubectl이 붙는다"는 기준에 걸린다.

- A: 요청 본문용 Protobuf 디코더를 직접 짠다(읽기 전용, 지원하는 종류와 필드만). 응답은 JSON으로 준다(Accept에 JSON이 있다).
- B: 415와 함께 "매니페스트로 `kubectl apply -f` 또는 `create -f`를 쓰라"는 안내를 돌려주고, 1단계 검증은 매니페스트로 한다.

**결정(2026-11-10, 사용자): A.** 요청 본문용 Protobuf 디코더를 직접 짠다. 설계 문서 2절 결정 5, 3.6절.

## M0에서 실제 서버로 확인한 것 (2026-11-10)

- 방법: `hack/m0-check.sh`. `sw server --trace FILE`이 받은 요청을 한 줄씩 기록한다(개발용, 기본은 꺼짐). kubectl v1.36.6(WSL)과 v1.36.0(윈도우, `WIN_KUBECTL=...`로 WSL의 localhost 전달을 거쳐 접속) 둘 다로 돌렸다. 47개 확인 통과.

| # | 확인한 것 | 결과 | 설계에 주는 영향 |
|---|---|---|---|
| 15 | `create service clusterip`, `create namespace` | 둘 다 **Protobuf 본문**이다. `create deployment` 240바이트, `create service clusterip` 141바이트, `create namespace` 57바이트. v1.36.0과 v1.36.6에서 바이트 수가 같다 | 결정 5의 대상 4종(Deployment, Service, Pod, Namespace)이 맞다. M1에서 이 본문을 테스트 자료로 저장한다 |
| 16 | `run`, `expose` | 실제 서버에서도 JSON이고 그대로 생성된다(201) | |
| 17 | `create -f` | `POST ...?fieldManager=kubectl-create&fieldValidation=Strict`. 그 전에 `/openapi/v3`, `/openapi/v3/api/v1`, `/openapi/v3/apis/apps/v1`을 읽는다. 종류마다 patch 연산과 `fieldValidation` 파라미터만 둔 문서로 통과한다(Namespace 포함) | 최소 OpenAPI v3 확정 |
| 18 | `--validate=warn` | 쿼리가 `fieldValidation=Warn`으로 오고, 서버의 `Warning: 299` 헤더를 kubectl이 `Warning: unknown field ...`로 보여 준다 | |
| 19 | `replace -f` | `PUT ...?fieldManager=kubectl-replace&fieldValidation=Strict`, JSON. 본문에 `resourceVersion`이 들어 있어 낡았으면 409가 그대로 보인다 | |
| 20 | `delete` (바로 지워지는 객체) | `DELETE` 본문 `{"propagationPolicy":"Background"}` → 200(Status, `details.uid`) → 이어서 같은 객체 `GET` → 404를 받고 대기를 끝낸다 | 파이널라이저가 있어 삭제가 미뤄지는 경우의 대기(watch)는 M1에서 확인 |
| 21 | `delete --dry-run=server` | dryRun을 쿼리가 아니라 **본문** DeleteOptions로 보낸다: `{"propagationPolicy":"Background","dryRun":["All"]}` | 삭제는 본문의 `dryRun`도 읽어야 한다(구현함) |
| 22 | `create --dry-run=server` | 쿼리 `dryRun=All` | |
| 23 | `get` (Table 없음) | Table 대신 JSON을 주면 이름과 나이만 보인다. 오류는 없다 | Table은 M1 |
| 24 | `describe pod` | events가 빈 목록이어도 정상 출력 | |
| 25 | 윈도우 kubectl v1.36.0 | version, `create -f`, get, delete가 같은 결과. 인증서의 `127.0.0.1`·`localhost` SAN으로 붙는다 | |

## 아직 확인하지 못한 것

- watch가 있어야 보이는 흐름: `get -w`, `edit`, `rollout history/undo`, 미뤄진 삭제의 대기, `logs -f`, `wait`. M1부터 채운다.

## M1에서 실제 서버로 확인한 것 (2026-11-11 KST)

- 기준: `326187edd52be9bde6b4426870939004313834cb`. `hack/m1-check.sh`를 WSL Ubuntu-24.04에서 root 없이 실행했다. Linux kubectl v1.36.6의 91개 확인과 Windows kubectl v1.36.0의 12개 확인, **103개 통과·0개 실패**.
- M0 회귀: 같은 구현에서 `hack/m0-check.sh` **47개 통과·0개 실패**. 스크립트의 이전 “실패 예상” 안내와 달리 typed create 3종도 두 버전에서 모두 201로 성공했다.
- M1 실행: `~/.cache/shearwater-dev/m1-20261111-001244-539`. M0 실행: `~/.cache/shearwater-dev/m0-20261111-000956`. 각 실행의 원문 요청은 `trace.jsonl`에 남겼다.
- 저장소에 남긴 근거: `hack/testdata/m1/trace.samples.jsonl`(원문 요청 35줄), `checks.observed.jsonl`(최종 실행의 판정 103개), `RESULTS.md`. `edit`만 `script`로 가상 터미널을 제공하고 `KUBE_EDITOR="sed -i ..."`로 편집했다.
- 아래는 위 “아직 확인하지 못한 것” 중 M1 항목을 갱신한다. 런타임·컨트롤러 없이 검증했으므로 `logs -f`와 실제 rollout 완료는 확인하지 않았다.

| # | 확인한 것 | 결과 | 설계에 주는 영향 |
|---|---|---|---|
| 26 | Table / wide | pods의 READY·STATUS·RESTARTS·AGE, deploy의 READY·UP-TO-DATE·AVAILABLE·AGE, svc의 TYPE·CLUSTER-IP·PORT(S)·AGE, ns의 STATUS·AGE를 출력한다. wide의 NODE·CONTAINERS·SELECTOR도 확인 | Table 응답을 실제 kubectl이 표시함 |
| 27 | `get pods -w` | 최초 list 뒤 `GET .../pods?resourceVersion=N&watch=true`, Accept는 Table 우선. 생성·라벨 제거·복구·삭제 때 각각 행이 출력됨 | watch의 Table 출력도 동작 |
| 28 | `get pods -w -l track=yes --output-watch-events -o json` | `labelSelector=track%3Dyes&resourceVersion=N&watch=true`, Accept JSON. ADDED → DELETED(셀렉터 이탈) → ADDED(재진입) → DELETED(실제 삭제). 이탈 이벤트는 이전 라벨과 새 사건 RV를 가지며 RV는 증가 | selector 전이와 이전 객체 보존 확인 |
| 29 | 반복 apply: 컨테이너 추가·삭제·순서 변경 | `PATCH` strategic merge, `fieldManager=kubectl-client-side-apply&fieldValidation=Strict`. 추가는 원소+`$setElementOrder/containers`, 삭제는 `$patch:delete`, 순서만 바꿀 때는 `$setElementOrder/containers`만 전송 | 최종 배열을 jsonpath로 확인. 각 단계 재적용은 unchanged, global RV 불변 |
| 30 | apply: env 삭제 | container 안에 `$setElementOrder/env`와 `{"$patch":"delete","name":"REMOVE"}`를 보냄. KEEP만 남음 | 중첩 리스트 패치 확인 |
| 31 | apply: RollingUpdate → Recreate | `strategy:{"$retainKeys":["type"],"rollingUpdate":null,"type":"Recreate"}` | Recreate로 변경되고 rollingUpdate가 없어짐 |
| 32 | apply: emptyDir → hostPath | volumes에 `$retainKeys:["hostPath","name"]`, `emptyDir:null`, hostPath.path, `$setElementOrder/volumes`를 보냄 | path만 사용한 지원 범위에서 교체·재적용 확인. hostPath.type 제한은 아래 참고 |
| 33 | `edit` | GET 뒤 strategic merge PATCH, `fieldManager=kubectl-edit&fieldValidation=Strict` → 200. sed가 바꾼 이미지가 저장됨 | PUT로 가정하면 안 됨. 비대화식 WSL 검증에는 PTY가 필요했음 |
| 34 | `patch` 3종 / label / annotate / set image | strategic·merge·JSON patch 각각 200 및 최종 값 확인. label/annotate는 merge patch(`fieldManager=kubectl-label/kubectl-annotate`), set image는 strategic patch(`fieldManager=kubectl-set`) | 명령별 미디어 타입과 manager 수신 확인 |
| 35 | typed create / debug copy | create deployment·create service clusterip·create namespace 및 `debug --copy-to --set-image --profile=baseline --share-processes=false`가 Protobuf POST → 201. 복사 Pod의 이미지 확인. Windows에서도 명령 성공 | 요청 디코더 4종을 실제 명령으로 검증 |
| 36 | 저장만 필드 경고 | livenessProbe 생성에서 `Warning: spec.containers[0].livenessProbe: stored, but has no effect: ...`를 표시 | 이미 같은 값인 필드를 변경하지 않은 update에는 다시 경고하지 않는 정책 |
| 37 | server-side apply 거절 | `PATCH`, `application/apply-patch+yaml`, `fieldManager=kubectl&fieldValidation=Strict&force=false` → 415와 client-side apply 안내 | 미지원 SSA를 성공처럼 처리하지 않음 |
| 38 | finalizer 삭제 대기 / wait delete | DELETE 뒤 객체는 남음. delete와 wait 각각 이름 fieldSelector의 watch-list를 열고 대기. merge patch로 finalizers를 비우면 둘 다 성공 종료하고 GET은 404 | 삭제 완료 이벤트와 대기 해제 확인 |
| 39 | `wait --for=jsonpath='{.status.phase}'=Pending` | 이름 fieldSelector와 `sendInitialEvents=true&resourceVersionMatch=NotOlderThan&allowWatchBookmarks=true&watch=true`. 기존 Pending 객체로 성공 | wait도 초기 이벤트 흐름을 사용함 |
| 40 | rollout용 watch-list 형태 | `get --raw`로 Deployment에 `sendInitialEvents=true&resourceVersionMatch=NotOlderThan&allowWatchBookmarks=true&timeoutSeconds=2&watch=true`. ADDED 다음 `k8s.io/initial-events-end:"true"` BOOKMARK를 받고 정상 종료 | 초기 목록 종료 신호만 검증. 컨트롤러가 만드는 rollout 성공 상태는 아직 미검증 |
| 41 | Windows M1 표본 | typed create 3종·debug copy·Table/wide·apply/재적용/컨테이너 추가·jsonpath wait·delete, 12개 통과 | Windows v1.36.0은 표본 검증이며 Linux 91개 전체를 반복한 것은 아님 |

### 검증 중 발견한 범위 제한

`hostPath.type: DirectoryOrCreate`를 포함한 최초 볼륨 교체는 400이었다. 출력은 `strict decoding error: unknown field "spec.template.spec.volumes[0].hostPath.type"`. 현재 자체 타입은 `hostPath.path`만 가지므로, 최종 M1 볼륨 교체 검사는 지원되는 path만 사용했다. type을 지원한다고 판정하지 않았다. 재현용 `hostpath-type.json`과 실제 실패 요청 한 줄 `hostpath-type.failure.jsonl`을 보존했다. 실행 폴더 `m1-20261111-000707-569`의 `report.md`에 전체 kubectl 오류 출력이 있다.

자동 실행 환경에서 PTY 없이 시작한 `edit`는 PATCH 전 멈췄다. `script --quiet --return --command ...`로 PTY를 제공하면 동일한 kubectl·서버·sed 편집이 정상 동작한다. 서버 코드는 변경하지 않았다.
