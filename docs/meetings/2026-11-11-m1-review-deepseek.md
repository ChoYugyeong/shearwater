> 공개 준비 과정에서 개인 경로·내부 세션 식별자·로컬 앱 이름을 일반화했어. 측정값과 당시 검증 범위는 유지했고, `<private-evidence>`는 공개 저장소에 없는 원본 기록을 뜻해.

# M1 리뷰 답 — watch·Table 테스트와 저장소 (딥시크)

작성: `review-agent`(딥시크). 대상: `<workspace>/shearwater`, main `3598a0b` 기준으로 읽었다.
내 테스트는 `66c6d37`(클로드가 커밋), `Changes` limit 수정은 `a465467`. 서버 코드는 내가 고치지 않았고 이 문서도 커밋하지 않는다.

## 1. 테스트

| 파일 | 함수 | 보는 것 |
|---|---|---|
| `internal/api/watch_test.go` | 14 | 전이표 8행 × 선택자 7개를 `filter.event`에 직접, 같은 표를 저장소 실제 쓰기로 한 번 더(시퀀스 14개), HTTP 스트림 11가지 |
| `internal/api/table_test.go` | 10 | 종류별 열 8종, Pod 셀(Waiting/Terminated/Running/RESTARTS), Deployment 0/2, Service PORT(S), includeObject 3종, 빈 목록, 406, humanDuration 경계 |

- 스트림 11가지: resourceVersion 없이 현재 객체 → 새 변경, `resourceVersion=N` 이후만, `sendInitialEvents=true`의 ADDED 뒤 `k8s.io/initial-events-end` BOOKMARK, 정리된 리비전에서 410(Expired), 스트림 중 뒤처져 ERROR 410 후 종료, `timeoutSeconds=1`, 현재보다 큰 리비전 504, 잘못된 조합 400(7가지), `watch=1`, Accept Table(첫 이벤트에만 columnDefinitions), 네임스페이스 범위.
- 결과: `bash hack/t.sh ./internal/api/` → api 39개 전부 통과(새 테스트만 세면 RUN 45개), 5회 연속 통과, `go vet ./...` 통과, `gofmt -l` 비어 있음. 지금 main에서 다시 돌려 `internal/api`와 `internal/store` 모두 통과.
- **watch·Table 경로에서 서버가 틀린 곳은 찾지 못했다.** 전이표는 코드와 정확히 일치했다: out→in은 ADDED, in→out은 이전 객체에 그 변경의 리비전을 실은 DELETED, in→in은 MODIFIED, out→out은 무음.
- 처음에 내가 쓴 Service 테스트가 422로 떴는데, 포트가 둘 이상이면 `name`이 필수라는 검증 규칙 때문이었다. 서버가 아니라 테스트를 고쳤다.

## 2. limit 수정에 대한 의견 (요청 2)

**동의한다.** 0 이하를 error로 거절하는 쪽이 내가 제안한 `if limit > 0 && len(changes) == limit` 가드보다 낫다.

- 가드는 panic만 막고 `limit=0`을 "0건"으로 조용히 해석한다. 지금 수정은 호출자 버그를 바로 드러낸다.
- `TestChanges`가 0과 -1을 둘 다 확인하므로 회귀도 막힌다.
- 한 가지만 적어 둔다: 이제 "무제한"을 limit으로 표현할 방법이 없다. 지금 서버는 `watchBatch=200` 고정이라 문제가 없고, 나중에 필요해지면 `limit<0=무제한` 같은 규칙을 만들지 말고 `AllChanges` 같은 별도 함수를 두는 편이 낫다. 지금 할 일은 아니다.

## 3. M1 저장소 리뷰 (compact, `Changes`의 next, 허브)

1. **compact: 틀린 곳 없음.** 동작은 `to = rev - keep`, 시간 규칙이 더 보수적이면 `firstRecent-1`로 낮춘다. 쓰기 트랜잭션 안에서 1분에 한 번, DELETE+UPDATE 두 문. 유휴 때는 아예 돌지 않는다.
2. **`compacted_rev`가 전역이라는 점은 알아 둘 것.** pod를 보는 watcher가 rev 4를 읽는 중에 deployment 쪽 변경만 압축돼도 그 watcher는 410을 받고 다시 목록을 뜬다. 안전한 쪽이지만 M3에서 컨트롤러 watch가 여러 개 붙으면 재목록이 잦아질 수 있다. 리소스별 하한을 두려면 스키마 한 칸이 필요하고, 지금은 두지 않는 쪽이 경량화에 맞다.
3. **시간 규칙은 초 단위 문자열 비교.** `at`도 cutoff도 RFC3339(UTC, 초)라 같은 초 안의 두 변경을 구분하지 못하고, `KeepFor`가 1초 미만이면 경계가 이상해진다. 실사용 값(1시간)에서는 문제가 없다. 주석 한 줄이면 충분하다.
4. **`Changes`의 next 규칙은 맞다.** 배치가 덜 찼을 때 스냅숏 리비전으로 올리는 것이 없으면, 다른 리소스가 커밋될 때마다 깨어나 같은 자리를 다시 읽는 바쁜 루프가 된다. read tx가 스냅숏이라 "그 사이 내 리소스 변경 없음"이 보장되고, 배치가 꽉 찼을 때 마지막 리비전으로 가는 것도 객체마다 리비전이 하나라 안전하다. 테스트(`TestChanges`)가 두 경우를 다 고정하고 있다.
5. **허브는 맞다.** `Changed()`를 먼저 잡고 읽으므로 커밋 누락이 없고, close→교체가 커밋 뒤라 데이터가 보이기 전에 깨우지 않는다. 유휴 비용은 0(고루틴·타이머 없음), 북마크를 켠 watcher만 분당 타이머 1개.
6. **모든 watcher가 모든 커밋에 깨어난다.** 커밋 1회당 N번의 `Changes` 쿼리다. 지금 규모에서는 무시할 수 있고, 필요해지면 wake에 리소스 키를 실어 거르면 된다.

## 4. M0 리뷰 답 (요청 `01M72XC4YK1KRNP5HBVVTK205Z`)

1. **`Write`는 `s.mu`를 잡은 채 `fn`을 돌린다.** fn 안에 네트워크나 컨테이너 대기가 들어가면 모든 쓰기가 멈춘다. M2 노드 에이전트는 "런타임에서 읽기 → 짧은 트랜잭션으로 저장"으로 나눠야 한다.
2. **충돌 재시도는 호출자 몫이다.** store는 `ErrConflict`만 준다. 컨트롤러용 재시도 헬퍼(다시 읽고 다시 적용, 3~5회)를 M2에 하나 두는 게 좋다.
3. **한 트랜잭션에 여러 객체는 그대로 쓸 수 있다.** 객체마다 리비전과 변경 로그가 하나씩, 커밋은 원자적, wake는 1회. GC와 롤아웃에 적합하다.
4. **`Update`가 `Encode`를 두 번 부른다**(같은 내용 판정용 옛 리비전, 저장용 새 리비전). 지금 객체 크기에서는 문제가 없고, 필요하면 첫 인코드에서 resourceVersion 숫자만 바꿔 재사용하면 된다(`withResourceVersion`에 같은 요령이 이미 있다).
5. **유휴 0을 깨는 것은 없다.** `database/sql`은 연결을 열어 둘 뿐 쿼리를 보내지 않고, WAL 자동 체크포인트는 쓰기 중에만 돌고, `/readyz`는 DB를 보지 않는다.
6. **M2 첫 확인 순서 제안:** ① 재부팅 뒤 같은 IP(할당표·veth 재연결) ② MTU(veth/bridge, Docker 브리지와 나란히) ③ shim 로그 직접 쓰기(재시작·회전·reopen) ④ DOCKER-USER — 우리 체인을 따로 만들고 맨 위에서 jump, Docker가 만든 행은 건드리지 않는다.

## 5. 확인한 것과 못 한 것

- 확인: 저장소·API 코드를 읽고, 테스트를 직접 돌리고, `Changes(limit=0)` panic을 임시 테스트로 재현한 뒤 그 파일을 지웠다. 위 수치는 전부 실행 결과다.
- 못 한 것: 실제 컨테이너·런타임이 필요한 확인은 하지 않았다. M2 항목은 코드를 읽고 낸 제안이지 측정값이 아니다.
