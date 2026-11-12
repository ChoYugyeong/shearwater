# 설계 검토 회의 1차 (2026-11-10)

- 방식: Reason Network 메시지. 사용자 지시("챗지피티랑 딥시크도 같이 회의하면서")에 따라 진행.
- 대상 문서: `docs/design-stage1.md` 2차 초안, `docs/kubectl-trace.md`
- 참석: 클로드(진행·설계), 코덱스 `dev-codex-21`(kubectl 호환·패치·Protobuf·SQLite/watch), 딥시크 `desktop-dsh`(런타임·네트워크·조정 루프)
- 상태: **코덱스와 딥시크 답 모두 도착. 설계에 반영함.**

## 읽는 법

코덱스의 답은 쿠버네티스 v1.36.6 소스를 읽고 대조한 것이다. 코덱스는 kubectl이나 서버를 실행하지 않았고, 클로드도 그 소스 줄을 직접 열어 보지 않았다. 그래서 아래 "근거"는 **코덱스가 제시한 것**이고, 구현할 때 해당 파일을 읽어 다시 확인한다. 실측으로 확인된 것은 `docs/kubectl-trace.md`에만 있다.

## 코덱스 검토 요약과 처리

### Protobuf 요청 디코더 (설계 3.6)

| # | 코덱스 의견 | 처리 |
|---|---|---|
| P1 | 봉투는 바이트 `6b 38 73 00` 뒤에 `runtime.Unknown` 메시지. 1=TypeMeta(1=apiVersion, 2=kind), 2=raw, 3=contentEncoding, 4=contentType. raw가 실제 객체다. 일반 인코더는 contentType을 비워 두므로 그것으로 형식을 판단하면 안 된다. HTTP Content-Type, 표식, 봉투의 종류, URL의 리소스·버전을 같이 검증한다 | 채택. 3.6의 (미검증) 표시를 "코덱스 소스 대조"로 바꿈 |
| P2 | 번호 표 원본은 core/v1, apps/v1, meta/v1의 `generated.proto` 외에 runtime, intstr, resource의 `generated.proto`도 필요하다. 번호는 메시지 안에서만 의미가 있다. Protobuf 규칙이 번호 변경·재사용을 금지하지만 모든 향후 버전 호환을 보증하지는 않는다. 태그를 고정하고 kubectl 1.36.0과 1.36.6 캡처를 따로 검증한다 | 채택 |
| P3 | **"표에 없는 번호는 값이 비면 버린다"는 규칙은 안전하지 않다.** `enableServiceLinks=false`, `automountServiceAccountToken=false`, `replicas=0`, `runAsUser=0`, 길이 0인 `emptyDir` 메시지는 생략과 뜻이 다르다. 모르는 번호는 값만 보고 빈 값인지 알 수 없다 | 채택. 규칙을 바꿈: 아는 필드는 값과 "있었는지"를 따로 보존, kubectl이 자동으로 넣는 기본 필드는 명시적인 처리표, 진짜 모르는 번호는 거절 |
| P4 | kubectl이 빈 필드를 보내는 것은 정상이다(`paused=false`, `minReadySeconds=0`도 실제로 쓴다. 포인터 필드는 nil이면 안 쓴다) | 채택. P3의 처리표가 필요한 이유 |
| P5 | 값 모양: Time은 seconds=1, nanos=2인데 실제 디코더는 빈 메시지를 "시각 없음"으로 보고 nanos를 버린다. IntOrString은 type=1(0 정수, 1 문자열), intVal=2, strVal=3. Quantity는 안쪽 메시지의 string=1. labels=11, annotations=12는 key=1/value=2인 항목의 반복이고 빈 값도 유효하다 | 채택 |
| P6 | 크기가 "루트 4종"으로 끝나지 않는다. Deployment → PodTemplate → PodSpec → Container/Volume, ObjectMeta, Selector, Quantity가 이어지고, "저장만" 등급(affinity, 프로브, securityContext)도 보존하려면 그 메시지의 번호 표가 필요하다. 공통 wire 리더와 메시지별 표를 나누고, 길이·재귀·varint 넘침 제한, 음수, 순서가 바뀐 필드, 중복 필드(스칼라는 마지막 값, 메시지는 병합, 반복은 추가)를 검증한다 | 채택. M1이 커진다 |
| P7 | `create service clusterip`, `create namespace`도 같은 경로라 Protobuf를 보낼 것이다(소스 확인, 미실측). 서버가 kubectl의 요청 형식을 JSON으로 바꾸게 할 방법은 없다(415 뒤 재시도는 CBOR용이고 Protobuf는 제외) | 채택. M0에서 캡처로 확인. 지원하지 않는 종류의 `create`를 지원한다고 쓰지 않는다 |

### strategic merge patch (설계 3.6)

| # | 코덱스 의견 | 처리 |
|---|---|---|
| S1 | 병합 키는 지원 범위에서 맞다. 빠진 것: `metadata.finalizers`(키 없는 값 목록의 병합), `volumes`는 `merge,retainKeys`(키 `name`). `$retainKeys`는 Deployment `strategy`만의 것이 아니다. emptyDir → hostPath로 바꿀 때 이전 종류가 남으면 잘못된 객체가 된다 | 채택 |
| S2 | 지시어 이름을 늘리기보다 뜻을 정확히: `$setElementOrder`는 "패치에 없는 서버 쪽 원소를 지워라"가 아니다. finalizers에는 `$deleteFromPrimitiveList/finalizers`가 온다. `$retainKeys`는 목록 밖의 기존 키를 지우고, 패치의 키가 목록 안에 있는지 검증한다. `$patch: merge`는 임의로 넣지 않는다 | 채택 |
| S3 | 테스트 자료: null·누락·빈 목록 구분, 추가+삭제+순서 변경을 한 요청에, 서버가 넣은 원소 보존, 같은 병합 키 중복 입력, finalizer 삭제, 볼륨 종류 교체. ports의 키는 protocol과의 복합키가 아니다 | 채택 |
| S4 | 서버는 3-way 비교를 다시 하지 않는다. kubectl이 보낸 패치를 현재 객체에 적용하면 된다. 읽기 → 패치 → 기본값·전체 검증 → 조건부 커밋을 한 경계로 묶는다. 지시어를 처리하기 전에 "모르는 필드" 검사를 하지 않는다 | 채택 |

### 엔드포인트·동사·쿼리 (설계 3.1~3.2)

| # | 코덱스 의견 | 처리 |
|---|---|---|
| E1 | `/scale`은 PATCH 외에 GET·PUT도 둔다(`--current-replicas`, `--resource-version` 경로). discovery의 `*/scale` 항목에는 kind=Scale과 함께 group=autoscaling, version=v1을 적는다 | 채택 |
| E2 | 전체 네임스페이스 경로(`/api/v1/pods`, `/apis/apps/v1/deployments`의 LIST·WATCH, `-A`)를 명시. `watch=true`와 `watch=1` 둘 다. 셀렉터의 `!=`, 존재·부재, `fieldManager`(받기만), `resourceVersionMatch`, `includeObject=Object`. `fieldValidation`의 서버 기본값을 정한다(`run`, `expose`는 쿼리를 안 보낸다) | 채택. 서버 기본값은 `Warn` |
| E3 | DELETE는 본문의 DeleteOptions에서 옵션을 읽고 uid·resourceVersion 전제조건을 원자적으로 검사. 삭제 시작은 MODIFIED(deletionTimestamp), 완료는 DELETED. 응답에 uid가 있어야 삭제 대기가 같은 이름으로 다시 만든 객체를 기다리지 않는다 | 채택 |
| E4 | logs는 JSON이 아니라 텍스트 스트림. 지원 파라미터를 명시하고, 시각을 저장하지 않으면 `sinceSeconds`·`sinceTime`을 조용히 무시하지 말고 안내한다. `describe service`는 EndpointSlice를 service-name 라벨로 찾으므로 옛 Endpoints는 필요 없다 | 채택 |
| E5 | `rollout status`는 status 값이 일관돼야 한다. `/status` 쓰기가 generation을 올리거나 Pending인 Pod를 available로 세면 대기가 틀어진다. 오류 응답은 Status의 reason·code를 유지한다 | 채택 |

### SQLite·watch (설계 5)

| # | 코덱스 의견 | 처리 |
|---|---|---|
| W1 | **변경 로그에 "바뀐 뒤 객체"만 있으면 셀렉터에서 빠져나간 것을 알릴 수 없다.** 조건 밖 → 안은 ADDED, 안 → 안은 MODIFIED, 안 → 밖은 "이전 객체 내용 + 이번 리비전"의 DELETED. 이전 객체가 필요하다 | 채택. `changes`에 `prev_data` 추가 |
| W2 | LIST의 항목과 목록 resourceVersion을 같은 읽기 스냅숏에서 얻는다. 목록의 버전은 항목들의 최댓값이 아니라 스냅숏 시점의 전체 리비전이고, 빈 목록도 가진다. 재생 순서를 고정한다. 정리(compaction)와 경계 갱신은 한 트랜잭션. 이미 시작한 스트림은 HTTP 코드를 못 바꾸므로 ERROR 이벤트(Status 410)로 끝낸다 | 채택 |
| W3b | (코덱스 추가) 재생을 배치로 나눠 읽으면 배치 사이에 정리 경계를 다시 확인해야 한다. 잘린 범위를 조용히 건너뛰지 말고 410으로 다시 맞추게 한다 | 채택 |
| W3 | 커밋 뒤 알림을 고루틴으로 흩뿌리면 순서가 뒤집힌다. 허브를 "깨우기 신호"로만 쓰고 DB를 순서대로 읽는 방법이 있다. 느린 구독자 때문에 쓰기를 막거나 큐를 무한히 쌓지 않는다. 재생하는 동안 읽기 트랜잭션을 오래 잡으면 WAL 정리가 막힌다 | 채택. 허브는 신호만, 구독자는 리비전 커서로 짧게 나눠 읽는다 |
| W4 | 조건부 UPDATE가 0행이면 리비전 증가까지 전부 되돌린다. IP 할당도 같은 트랜잭션. dryRun은 객체·리비전·IP·소유 관계·알림을 남기지 않는다(코덱스가 뒤에 정정: dry-run 요청을 감사 기록에 `dry_run=true`로 남기는 것은 괜찮다. 실제 요청이므로 유휴 쓰기와도 다르다). 한 트랜잭션에서 여러 객체를 바꾸면 객체마다 다른 리비전. `owners`에 외래 키 선언이 없다는 점 유의 | 채택 |

### 경량화·편의성 (설계 0.1)

| # | 코덱스 의견 | 처리 |
|---|---|---|
| L1 | 하트비트 수정은 맞다. 추가로: 내용이 같은 status 쓰기, 북마크 때문에 리비전을 발급하는 것, 변화 없는 정리·감사 갱신을 막아야 유휴 쓰기 0이 된다. 정리는 실제 쓰기에 묶고, 북마크는 이미 처리한 리비전을 다시 쓴다. 주기적으로 도는 DB 작업을 따로 두지 않는다 | 채택 |
| L2 | 유휴 측정은 초기 정리가 끝난 안정 상태에서, API 호출 수가 아니라 DB·WAL·로그 파일의 실제 쓰기를 잰다. 오래 열린 watch의 읽기 트랜잭션과 구독자별 무한 큐가 메모리 목표를 깬다 | 채택 |
| L3 | 1단계에서 빼도 되는 것: `/openapi/v2`(404 유지), 집계 discovery, 응답 Protobuf, server-side apply, 실제 페이지 나눔, 옛 Endpoints. 주기 북마크는 단순화해도 되지만 watch-list의 끝 북마크는 빼면 안 된다. Protobuf 요청, 패치 정확성, uid 삭제 대기, 셀렉터 watch 전이는 경량화를 이유로 빼면 사용자가 "틀린 성공"을 본다 | 채택. 이미 뺀 것과 일치 |
| L4 | 위험 순위: ① Protobuf의 "있었는지"와 패치 변환에서 설정을 조용히 잃는 것 ② LIST·재생·실시간·셀렉터 경계의 누락과 watch 버퍼(rollout 멈춤, 메모리 증가) ③ 변화 없는 status·리비전·정리의 반복 쓰기. 기능을 늘리기 전에 각각의 최소 회귀 테스트 자료를 먼저 고정한다 | 채택. 10절 M1의 순서에 반영 |

## 코덱스가 제시한 근거 (구현 때 다시 확인)

- 봉투: `staging/src/k8s.io/apimachinery/pkg/runtime/generated.proto`, `.../runtime/serializer/protobuf/protobuf.go` (v1.36.6)
- 값 모양: `.../apis/meta/v1/time_proto.go`, `.../util/intstr/generated.proto`, `.../api/resource/generated.proto`
- 패치: `.../apimachinery/pkg/util/strategicpatch/patch.go`, `staging/src/k8s.io/api/core/v1/types.go`(volumes의 patchStrategy), `.../apis/meta/v1/types.go`(finalizers)
- scale: `staging/src/k8s.io/kubectl/pkg/scale/scale.go`, `client-go/scale/util.go`
- 삭제 대기: `kubectl/pkg/cmd/delete/delete.go`
- 셀렉터 watch 전이: `staging/src/k8s.io/apiserver/pkg/storage/cacher/cache_watcher.go`
- Protobuf 규칙: https://protobuf.dev/programming-guides/proto2/#assigning-field-numbers , https://protobuf.dev/programming-guides/field_presence/ , https://protobuf.dev/programming-guides/encoding/
- SQLite: https://www.sqlite.org/isolation.html , https://www.sqlite.org/wal.html#ckpt

## 딥시크 검토 요약과 처리

딥시크의 답은 **헤드리스 자동 응답**이다(데스크톱 채팅 맥락 없음, 읽기 전용, 설계 문서만 읽음). 첫 메시지 기준으로 답해서 일부는 결정 6 이전의 CNI 질문에 대한 것이다. "(확실)", "(추측)", "(미검증)"은 딥시크가 붙인 표시다.

### 채택한 것

| # | 딥시크 의견 | 처리 |
|---|---|---|
| D1 | 고아 정리를 "우리 라벨" 기준으로만 하면 생성 도중 실패해 라벨이 없는 컨테이너가 남는다. 이름 규칙으로도 훑는다. 부모 없는 스냅숏, Pod 객체 없이 남은 veth·네임스페이스도 정리 대상 | 채택 |
| D2 | headless Service(`clusterIP: None`)는 DNS가 없으면 쓸 곳이 없다. "저장만 + 경고"로 내린다 | 채택 |
| D3 | "서버가 죽어도 컨테이너는 계속 돈다"는 맞지만 "계속 재시작된다"는 아니다. containerd와 runc에는 재시작 정책이 없어서 재시작은 전부 노드 에이전트 몫이다 | 채택. 4절에 명시 |
| D4 | 네임스페이스를 묶어 둔 마운트를 푸는 책임이 우리에게 있다. Pod 삭제와 실패 되돌리기마다 풀고, 시작할 때 디렉터리를 훑어 고아를 정리한다 | 채택 |
| D5 | `/run`은 재부팅하면 비워진다. 그러면 저장소에는 Running인데 컨테이너도 네임스페이스도 없는 상태가 된다. 재부팅 복구 경로가 설계에 없다 | 채택. 복구 경로를 7절에 추가. IP 할당이 SQLite에 있어 같은 IP로 다시 만들 수 있다(딥시크는 IP가 바뀐다고 봤다) |
| D6 | 3단계에서 systemd 유닛으로 옮길 때 마운트 네임스페이스를 분리하는 옵션을 켜면 네임스페이스가 밖에서 안 보인다 | 채택. 3단계 주의로 기록 |
| D7 | "shim이 로그 파일에 직접 쓴다"는 문서화된 사용법이 아니고 버전에 따라 다를 수 있다. M2 첫 확인 대상. 안 되면 서버가 로그를 옮겨 적는 방식으로 물러나야 하고, 그러면 서버가 죽은 동안 로그가 끊긴다 | 채택. M2 첫 확인 목록에 넣고, 물러날 경우를 7절에 적음 |
| D8 | 서버가 죽은 동안 로그는 쌓여도 조회는 안 된다. "유지"와 "조회 가능"을 구분해 적는다 | 채택 |
| D9 | IP 할당기에서 게이트웨이 주소(.1)를 Pod에 주지 않게 예약한다 | 채택 |
| D10 | 허용 규칙은 `DOCKER-USER` 체인에 넣는다. `FORWARD`에 직접 넣으면 Docker가 규칙을 다시 만들 때 밀리거나 지워진다 | 채택(권고). 실측으로 체인이 맨 앞에 있고 비어 있음을 확인. M2에서 실제 통신으로 확정 |
| D11 | Docker가 `bridge-nf-call-iptables=1`을 켜면 같은 브리지의 Pod끼리도 FORWARD를 거쳐 막힌다. 규칙이 세 갈래 필요: Pod↔Pod, Pod→밖, 밖→Pod의 응답. 주소 변환은 Pod 대역끼리와 Service 대역을 제외 | 채택. 아래 실측으로 전제 확인 |
| D12 | MTU가 문서에 없다. 브리지와 veth의 MTU를 바깥 인터페이스에 맞춘다. Pod·Service 대역이 다른 대역과 겹치는지 `sw doctor`가 검사한다 | 채택 |
| D13 | 서비스 프록시: 주소를 붙인 뒤에 듣는다(순서가 바뀌면 실패). 엔드포인트 연결에 실패하면 다음 엔드포인트로. Service 삭제 때 소켓을 닫고 주소를 뗀다. `sw down`은 주소를 유지하고 `--all`에서만 뗀다 | 채택 |
| D14 | 자기 Service로 돌아오는 경우는 막히지 않는다(설계의 추측이 맞다) | 확인. M5에서 실측은 그대로 한다 |
| D15 | 종료 중인 Pod(`deletionTimestamp` 있음)를 EndpointSlice에서 빼는 규칙이 없다. 유예 30초 동안 죽어 가는 Pod로 트래픽이 간다 | 채택 |
| D16 | **서버가 죽은 동안 컨테이너가 죽으면 아무도 안 살린다.** 다시 뜬 뒤 죽은 태스크를 찾아 종료 시각과 코드를 읽고 재시작한다. 이미 정리돼 코드가 없으면 "알 수 없음"으로 두되 횟수는 센다 | 채택 |
| D17 | 재시작 지연 상태가 메모리에만 있으면 서버 재시작마다 초기화된다. Pod status의 `restartCount`와 `lastState.terminated.finishedAt`에서 복원한다. `finishedAt`을 반드시 채운다 | 채택 |
| D18 | 종료 순서에 빠진 것: EndpointSlice에서 먼저 빼기, 유예 안에 안 죽는 프로세스 재시도, 중간 실패 복구. 조정은 저장소가 아니라 현실(containerd, 네임스페이스, 주소)을 먼저 읽는다 | 채택 |
| D19 | 서버 재시작 직후에는 종료 대기가 끊겨 있다. 다시 걸고, 놓친 종료는 containerd의 종료 이벤트 구독으로 잡는다 | 채택 |
| D20 | 미검증 항목을 M2 첫 확인 목록으로 옮긴다: 로그, br_netfilter, MTU, 자기 Service 접근, 대역 충돌 | 채택. 일부는 아래에서 이미 실측 |

### 채택하지 않은 것 (사용자 결정과 다름. 사용자에게 보고함)

| # | 딥시크 의견 | 처리 |
|---|---|---|
| X1 | Protobuf 요청 디코더(결정 5)를 뺀다. 이득이 `kubectl create deployment` 하나인데 번호 표 유지비가 계속 든다 | **미채택.** 사용자가 결정 5로 정했다. 반대 의견으로 기록 |
| X2 | Pod 네트워크 직접 구현(결정 6)이 가장 큰 위험이다. M2 실측을 보고 결정 6을 다시 볼 조건을 지금 적어 둔다 | **부분 채택.** 결정은 그대로. 다만 "다시 볼 조건"은 7절에 적는다(판단은 사용자) |
| X3 | Table 출력을 뺀다 | 미채택. 없으면 `kubectl get`에 상태 열이 안 나온다. 사용자 편의성에 직접 걸린다 |
| X4 | strategic merge patch의 지시어를 3개로 줄인다 | 미채택. 코덱스의 S1·S2와 반대 방향이다(빠뜨리면 잘못된 객체가 조용히 만들어진다). 모르는 지시어는 422로 거절하는 규칙은 유지 |
| X5 | `sw tree/diff/wait`, EndpointSlice 노출, `hostPath`, fieldValidation Warn/Ignore를 뺀다 | 미채택. `tree`는 1단계 토대이고 `diff/wait`는 원래 2단계다. 나머지는 구현이 작다 |

## 클로드 실측 (2026-11-10, WSL에서 root로 읽기만 함)

딥시크가 미검증이라고 한 전제를 확인했다.

| 항목 | 값 |
|---|---|
| `br_netfilter` | 올라와 있음 |
| `net.bridge.bridge-nf-call-iptables` | 1 → 같은 브리지 안 통신도 FORWARD를 거친다(D11의 전제가 맞다) |
| FORWARD 기본 정책 | DROP |
| FORWARD의 첫 규칙 | `-j DOCKER-USER`, 그다음 `-j DOCKER-FORWARD` |
| `DOCKER-USER` 체인 | 있고 비어 있음 |
| iptables | v1.8.10 (nf_tables 백엔드) |
| `net.ipv4.ip_forward` | 1 |
| MTU | eth0 1500, docker0 1500 |
| 경로 대역 | 172.23.16.0/20(eth0), 172.17.0.0/16(docker0). 10.244.0.0/24, 10.96.0.0/16과 겹치지 않음 |

아직 실측하지 않은 것: 로그 파일 직접 쓰기, 실제 Pod의 외부 통신, 자기 Service 접근.

## 두 검토가 엇갈린 곳

- Protobuf 디코더: 코덱스는 "하되 이렇게 해야 안전하다", 딥시크는 "빼라". 사용자 결정(직접 작성)을 따른다.
- 패치 지시어: 코덱스는 "뜻을 정확히, 빠진 키 추가", 딥시크는 "3개로 축소". 코덱스 쪽을 따른다.
- 위험 1순위: 코덱스는 "설정을 조용히 잃는 것(Protobuf·패치)", 딥시크는 "Pod 네트워크 직접 구현". 둘 다 사용자가 직접 짜기로 한 부분이다. M1과 M2 각각에서 테스트 자료와 첫 확인을 먼저 한다.

## 아직 열려 있는 것

1. 로그 파일 직접 쓰기가 containerd 2.3.3에서 되는지: M2 첫 확인.
2. `DOCKER-USER`에 넣은 규칙으로 Pod가 실제로 통신하는지: M2 실측.
3. `create service`, `create namespace`의 본문 형식: M0 캡처.
4. 사용자의 설계 동의.
