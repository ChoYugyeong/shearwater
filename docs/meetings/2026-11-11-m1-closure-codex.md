> 공개 준비 과정에서 개인 경로·내부 세션 식별자·로컬 앱 이름을 일반화했어. 측정값과 당시 검증 범위는 유지했고, `<private-evidence>`는 공개 저장소에 없는 원본 기록을 뜻해.

# M1 후속 결함 4건 독립 검수 및 답변

- 검수일: 2026-11-11 KST. 검수자: Codex `review-agent`.
- 사용자 요청: 후속 결함 4건의 수정 여부 검수, M1 검토 답변을 memdex로 전달.
- 원 요청: memdex #113 (`dsh-desktop@agentkube`가 클로드의 세 질문 전달).
- 검수 대상: `18555de6c8ed23a10f7ab88ac8ecfd34ee1e51ff`. 시작·종료 HEAD가 같고 `cmd`, `internal`, `go.mod`, `go.sum`에 미커밋 차이가 없었다.
- 서버·테스트 구현은 수정하지 않았다. 기존의 미커밋 문서와 `boundaries.py`는 유지했다.

## 판정

**후속 4건은 모두 수정 확인. 원래 실패하던 HTTP 재현 4/4, 관련 테스트 함수 5/5, M1 103/103, M0 47/47 통과.**

`3598a0b`에서 기록된 0/4는 과거 결과다. 현재의 네 이슈 수정 판정은 아래 결과로 대체한다. 이 판정은 Kubernetes API 전체의 동일성을 인증하지 않는다.

| 이슈 | 수정 커밋·근거 | 이번 실행에서 관측한 결과 |
|---|---|---|
| Deployment annotation만 바꿔도 generation 증가 | `18555de`; `internal/api/handlers.go:411`, `internal/apis/registry.go:94` | PUT 200, generation 1 → 2. PUT·PATCH 3종, label만 변경, 같은 값 재적용, spec과 annotation 동시 변경, `/status` 보존도 관련 테스트 통과 |
| 삭제 중 새 finalizer 추가 거절 | `4f83c7c`; `internal/api/handlers.go:394` | PUT 422 Invalid. PUT·PATCH 3종의 추가 거절 및 거절 시 revision 불변, 기존 finalizer 제거·재정렬·마지막 제거 후 삭제까지 테스트 통과 |
| Strict 중복 JSON 키 검출 | `18555de`; `internal/apis/decode.go:57`, `internal/api/handlers.go:193`, `:353`, `:504` | 중복 `spec.replicas` POST 400 BadRequest. Warn은 경고+마지막 값, Ignore는 무경고, PUT·PATCH 3종·`/status`의 status 중복 검사 및 중첩/배열 경로 검사 통과 |
| 정상 본문에도 fieldValidation enum 검증 | `4f83c7c`; `internal/api/handlers.go:99` | `fieldValidation=Maybe` POST 400 BadRequest. POST·PUT·PATCH와 `/status`의 잘못된 값 거절, 거절 시 revision 불변, 정상 값 허용 확인 |

Deployment의 조건은 기억에만 의존하지 않았다. Kubernetes v1.36.6의 `PrepareForUpdate`가 spec 또는 annotations 변경 시 generation을 한 번 증가시키는 것을 원문과 대조했다. 이번 구현의 Deployment 분기와 일치한다. [Kubernetes v1.36.6 strategy.go](https://github.com/kubernetes/kubernetes/blob/v1.36.6/pkg/registry/apps/deployment/strategy.go#L99)

## 실제 실행한 검사와 증거

WSL Ubuntu-24.04, `go1.27.3 linux/amd64`, `CGO_ENABLED=0`. 검사 서버는 격리된 ext4 상태 디렉터리와 `127.0.0.1:17643`을 사용했다. 각 검사 종료 후 해당 포트가 비었음을 확인했다.

| 검사 | 결과 | 증거 |
|---|---|---|
| gofmt 사전 확인 + `bash hack/t.sh ./...` | 통과. go vet 실행, 전체 go test 출력은 캐시 결과 | `unit-vet.log` |
| 관련 API 테스트 4개와 DuplicateFields 테스트를 `go test -count=1 -v -run 'Test(NoNewFinalizerWhileDeleting\|FieldValidationValueIsAlwaysChecked\|DeploymentGenerationFollowsAnnotations\|DuplicateKeysFollowFieldValidation\|DuplicateFields)$' ./internal/api ./internal/apis`로 실행 | 5개 함수 모두 새 실행으로 통과 | `focused.log` |
| `SW_PORT=17643 python3 hack/testdata/m1/boundaries.py` | 4 passed, 0 failed | `boundaries.log`, `boundary-report.json` |
| `SW_PORT=17643 WIN_KUBECTL=/mnt/c/ProgramData/chocolatey/bin/kubectl.exe bash hack/m1-check.sh` | 103 passed, 0 failed; Linux v1.36.6 91개 + Windows v1.36.0 12개 | `m1.log`, `m1-report.md` |
| `SW_PORT=17643 bash hack/m0-check.sh` | 47 passed, 0 failed; Linux v1.36.6 | `m0.log` |

증거 사본: `<private-evidence>\agentkube-m1-20261111\`. kubeconfig·토큰은 복사하지 않았다.

원본 실행 디렉터리:

- 공통 로그: `$HOME/.cache/shearwater-dev/m1-closure-codex-20261111-538`
- 네 경계 HTTP 요청: `$HOME/.cache/shearwater-dev/m1-boundaries-20261111-033655-834`
- M1: `$HOME/.cache/shearwater-dev/m1-20261111-033720-619`
- M0: `$HOME/.cache/shearwater-dev/m0-20261111-033754`

최초 PowerShell→WSL 인라인 명령은 따옴표 전달 문제로 테스트 실행 전에 실패했다. 외부 검수 폴더에 스크립트를 저장하고 실행한 위 결과만 판정에 사용했다.

## M1 세 질문에 대한 답

### 1. kubectl 호환 관점에서 M1에 빠진 흐름

합의된 M1 명령 흐름에서는 이번 검사로 새 차단 결함을 찾지 못했다. 기존 103개 검사와 별도로 후속 4건까지 통과했다. apply 반복·SMP 목록 변경·edit·PATCH 3종·Protobuf create·Table/watch·delete/wait 경로의 회귀도 없었다.

남은 범위를 구분해야 한다. `/scale`은 설계상 M4이며 현재 501, `logs -f`와 실제 Pod 동작은 M2, 실제 컨트롤러를 기다리는 `rollout status`는 이후 단계다. SSA는 의도적으로 415이다. 이를 M1 통과로 지원한다고 광고하면 안 된다. Windows는 12개 표본만 실행했고 91개 전체 검사는 반복하지 않았다. race 검사는 `CGO_ENABLED=0` 환경에서 실행하지 않았다. 원래 M0 리뷰의 Service 등 다른 kind generation 정책과 나머지 API 전체 정합성은 이번 네 건의 수정 판정에 포함하지 않는다.

### 2. hostPath.type 처리 방향

**M2에서 런타임 동작까지 구현하는 안을 권한다.** `type`은 단순 메타데이터가 아니라 실제 호스트 경로의 종류 검사와 필요시 생성 여부를 결정한다. 예를 들어 `DirectoryOrCreate`는 없는 디렉터리를 생성하고, `Directory`는 기존 디렉터리를 요구한다. 따라서 장기적인 "저장만+경고"는 매니페스트 의도와 실제 마운트 동작을 어긋나게 한다. [Kubernetes hostPath volume types](https://kubernetes.io/docs/concepts/storage/volumes/#hostpath-volume-types)

M1에서는 현재 Strict의 명시 거절을 유지해도 된다. M2에서 타입 enum 검증, 생성/종류 확인, 실패 시 사용자에게 보이는 이유, 매니페스트 적용부터 실제 마운트까지를 함께 구현하고 시험한다. 구현 전 저장만 지원으로 바꾸자는 의견은 아니다. 이 문서는 제안이며 사용자 결정이나 구현 완료로 기록하지 않는다.

### 3. M0 리뷰 답을 보냈는지와 요점

복구된 원문 `docs/meetings/2026-11-10-m0-review.md`에 따르면 원 요청 `01M72XBV86RHV863620H3JC1VJ`에 대한 최초 Reason 답장은 `1WY5KWXB3TMGC890CWGJ2CCXVC`로 queued까지 확인됐고 수신 확인은 없었다. 이번 세션이 당시 전달 성공을 새로 확인한 것은 아니다.

요점은 generation의 종류별 정책, 삭제 중 finalizer 추가 차단, Strict 중복 JSON 키와 fieldValidation enum 검사, 그리고 SMP·Protobuf·watch 경계를 실제 fixture로 고정하라는 것이었다. 그중 후속으로 분리된 네 결함은 이번 검사에서 모두 해결됐다. 전체 M0 원문은 위 파일에 보존돼 있다.

## 전달 기록

memdex 전송 결과는 아래에 기록한다. 큐 적재와 상대의 실제 읽음은 구분한다.

- **memdex #122 발송 완료**, 대상 `dsh-desktop@agentkube`의 명시적 공용 편지함. 원 요청 전달자에게 클로드 질문의 답을 보냈다.
- 전송 도구 응답: `posted #122 to dsh-desktop@agentkube`. 상대의 다음 inbox 확인을 기다리는 상태이며 읽음 확인은 아직 없다.
- 원문 #113은 `codex` 수신함에 있었고 이 MCP 연결은 `gpt`여서 `reply_to=113`은 주소 불일치로 거절됐다. 발신자 정체성을 바꾸지 않고 문서화된 `group=true`로 원 전달자 편지함에 새 메시지를 보냈다. 공용 편지함은 `request_key`를 지원하지 않아 그 인자를 뺀 정상 전송이 #122다. 앞선 두 호출에서는 메시지가 생성되지 않았다.
- 별도의 Reason 전송, 그룹 전체 방송, 커밋, push는 수행하지 않았다.

## 다음 세션 첫 프롬프트

```text
<workspace>/shearwater에서 docs/meetings/2026-11-11-m1-closure-codex.md만 먼저 읽어.
레포 전체를 재탐색하지 마. 18555de에서 후속 4건과 M1 103/M0 47 검수가 끝났고,
답변은 dsh-desktop@agentkube memdex #122에 있다. 코드는 수정하지 않았다.
추가 작업을 시작하기 전에 해당 inbox 회신과 HEAD 변화만 확인해.
```
