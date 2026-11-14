> 공개 준비 과정에서 개인 경로·내부 세션 식별자·로컬 앱 이름을 일반화했어. 측정값과 당시 검증 범위는 유지했고, `<private-evidence>`는 공개 저장소에 없는 원본 기록을 뜻해.

## alpha.5 릴리스 후속 (2026-11-14)

사용자가 alpha.5 발행과 기존 앱·데이터를 유지하는 운영 업데이트를 요청했어. `public-alpha-5`는 아래 `connection-continuity` 커밋 e46d0ed에서 이어진 공개 계열이며 제품 버전은 0.1.0-alpha.5야. 이 버전의 설치·첫 체험·연결 유지 안내를 맞췄어. 공개 소스 재빌드·릴리스 파일 검증·운영 교체와 실제 호스트 부하의 최종 결과는 GitHub 해당 릴리스 본문과 작업별 비공개 보고서에 남겨. 아래 '미발행/alpha.4 유지'는 이전 개발 작업의 완료 당시 기록이야.

다음 세션은 이 단락·`docs/releases.md` alpha.5 부분과 작업별 릴리스 검증 보고서부터 읽어. 레포 전체나 다른 프로젝트를 재탐색하지 마. 기존 앱·인증·데이터를 보존하고 원본 private main과 낯선 M1 파일을 공개 브랜치에 섞지 마. 추가 push·릴리스·교체는 그때의 사용자 지시 범위에 맞춰 진행해.

## 최신: 연결 유지 개선 검증 완료 (2026-11-14)

`connection-continuity`는 공개 소스 계열 `228876a`에서 이어진 로컬 작업 브랜치야.
프로세스 내부 Service TCP 프록시 대신 소유 표식이 있는 커널 DNAT·conntrack을 사용하고,
ReplicaSet 축소·롤링 교체 전에 기존 Service 연결을 최대 30초 기다리도록 구현했어.
Pod IP 반납 전 연결 기록 정리, 같은 SQLite 스냅샷의 규칙/종료 대기 확인,
Pod 자기 Service 연결의 veth hairpin, VIP 제거 순서도 보완했어.

[연결 유지 안내](connection-continuity.md)와 [검증 기록](connection-verification.md)을 먼저 봐.
같은 개발 바이너리의 정상 HTTP **112,000건/오류 0/재시도 0**을 확인했고, 1MiB 느린 다운로드,
30초 종료 경고 watch, Service IP 재사용, 3분간 TERM/KILL 5회도 실측했어.
Go 전체·M0 47/0·M1 103/0·M4/M5·지원 범위의 1단계 통합·root 격리 검사 2개를 통과했어.
full3의 Pod 삭제 후 이벤트 조회 assertion 실패는 원본 그대로 남겼고, 시험을 실시간 watch로
바꾼 full4에서 관련 경계와 나머지 부하를 통과했어. 실행별 상태·수치·바이너리 해시는
[측정 JSON](testdata/connection-continuity-20261114.json)에 있어.

운영 alpha.4의 프로세스·앱·인증·파일·저장 객체·IP를 보존했고 QA 실행 자원은 정리했어.
이 개선은 **미발행**이며 운영 바이너리를 교체하지 않았어. 개발 빌드의 기존 alpha.4 버전 문자열을
공개 릴리스와 혼동하지 마. C 컴파일러가 없어 race detector는 미실행이며 독립 코드 검수도 아직 없어.
30초보다 긴 연결, Pod 직접 IP, 강제 삭제, 컨테이너/호스트 장애는 무중단 보장 범위가 아니야.
처음 alpha.4에서 전환할 때 기존 프로세스 소유 소켓을 커널로 옮길 수 없다는 점도 남아 있어.

**다음 세션 첫 프롬프트:** HANDOFF의 이 최신 단락과 `docs/connection-continuity.md`,
`docs/connection-verification.md`를 먼저 읽고 변경할 대상 파일만 확인해. 레포 전체나 상위 폴더의
다른 프로젝트를 재탐색하지 마. `connection-continuity` 공개 계열 작업을 사용하고 원본 private main과
기존 미추적 M1 파일은 섞지 마. 실행 중인 앱·데이터를 보존하고 runtime 검사는 별도 QA에서만 해.
새 릴리스·push·운영 업그레이드는 후속 사용자 지시가 있을 때 진행하고, 그때는 버전을 올린 공개 소스를
다시 빌드·검증해. 아래 설명은 각 시점의 과거 기록이야.

## alpha.4 이후: 첫 사용 공동 검증

alpha.4는 공개했고 기존 운영 앱·데이터를 유지하며 서버 업데이트도 끝났어.
후속 작업은 공개 alpha.4 커밋을 기준으로 한 별도 `onboarding-alpha4` 브랜치에서
첫 체험 안내, 실제 설치 경로와 연결되는 MCP 설명, 승인 배포 예제와 피드백 양식을 보완하는 거야.
기능 추가·새 릴리스·운영 서버 교체를 뜻하지 않아.

[첫 체험 안내](first-try.md), [AI 연결 안내](agent-mcp.md), [피드백 양식](first-try-feedback.md)을
먼저 읽고 해당 부분만 점검해. 외부 사용자의 설치·재사용 결과는 아직 없으며 에이전트의
재현과 문서 검토를 외부 사용자 평가로 세지 마. Claude와 문서 검토를 나누고 실제 명령은
격리 QA에서 확인해. 운영 앱·인증·데이터와 기존 릴리스 파일은 유지해.

문서의 설치 블록과 JSON을 그대로 추출해 공개 alpha.4로 재현했어. 나머지 셸 명령은
QA 전용 경로·주소·소켓을 쓰는 동등한 인자로 재구성했어. nginx HTTP·로그, 의도적인
종료 코드 7의 네 진단 명령, 계획 검토·승인 전 거부·승인 후 HTTP·재사용 거부 등 런타임
검사 11개(정리 포함), Windows→WSL 조회 전용 stdio MCP 검사 8개를 통과했어.
Windows MCP는 직접 만든 프로토콜 클라이언트 검사이며 실제 Claude Code 대화 연결의
성공을 뜻하지 않아. 필요한 패키지가 이미 있는 격리 QA를 사용했으므로 새 OS 준비 시간과
외부 사용자 성공률은 미검증이야. 기존 운영 서버·앱 PID·IP·재시작 수·데이터도 동일함을 확인했어.

2026-11-14 Claude의 독립 문서 검토 14건을 받아 배포판 선택, 시작 안내,
PowerShell의 실행 인자 전달, 계획 해시 입력, 쓰기 MCP 등록·해제 등을 보완했어.
추가로 새 터미널의 kubectl 경로 3건, 문서에서 추출한 함수·해시 입력의 별도 API 검사 9건,
Windows→WSL 도구 목록 2건(조회6개/쓰기8개), PowerShell 5.1의 등록 인자 전달 2건을 확인했어.
API 검사는 실제 공개 실행 파일과 root 소유의 QA 인증을 사용하되 기본 경로를 QA 전용으로
바꿨고 컨테이너는 실행하지 않았어. PowerShell 검사는 인자 수집용 실행 파일을 사용했으며
Claude의 실제 연결 등록·대화 성공 검증은 아니야. 최종 동료 검토와 파일별 근거는 작업별 보고서를 봐.

다음 세션은 이 단락과 작업별 검증 보고서부터 읽고 레포 전체나 다른 프로젝트를 재탐색하지 마.

## alpha.4 릴리스 후속 (2026-11-13)

사용자가 alpha.4 발행과 기존 앱·데이터를 유지하는 실제 서버 업데이트를 요청했어.
`public-alpha-4`는 `next-operations`의 운영 복구·프로브·MCP 구현에서 이어진 공개 계열이고
제품 버전은 `0.1.0-alpha.4`야. 현재 설치 안내·기능 안내·릴리스 설명을 이 버전에 맞췄어.
공개 소스 재빌드·최종 패키지 검사·발행·운영 교체 결과는 GitHub 릴리스 본문과 작업별
비공개 완료 보고서에 남겨. 아래 '미발행', 'alpha.3 유지'는 개발 완료 당시 기록이야.
기존 프로브가 이번 버전부터 실제로 실행되므로 운영 교체 전에 선언을 확인해야 해.
SQLite와 앱 데이터·인증을 보존하고, 빈 런타임·재부팅 검사는 계속 격리 QA에서만 실행해.

## 최신: 운영 복구·프로브·MCP 개발 완료 (2026-11-13)

공개 alpha.3 커밋 `bc9364c`에서 이어진 `next-operations` 브랜치야.
제품 버전은 **미발행 `0.1.0-alpha.4-dev`**이고, 기존 운영 서버와 앱은 alpha.3 그대로 유지했어.
자동 시작, 온라인/볼륨 백업·새 경로 복원, startup/readiness/liveness 검사,
조회 전용 MCP와 계획 → 로컬 승인 → 1회 배포를 구현했어.
사용자는 AI 연결의 변경 계획·배포까지 이번 범위에 포함한다고 확정했어.

[검증 기록](next-verification.md)과 기능별 초보자 안내를 먼저 읽어.
전체 Go, Python 40개, M1 103개, M0 47개, 기존 수명주기, 실제 진단 장애 16개,
추가 프로브 장애 4개, 실제 MCP 계획·승인·배포, systemd 강제 종료/QA WSL 재부팅,
볼륨 복원·구버전 복귀를 확인했어. M1과 진단의 저장만 경고 기대값은 프로브 실행이라는
새 계약에 맞춰 변경했으며 검사를 제거하지 않았어. 독립 동료 검토는 아직 받지 못했어.

**다음 세션 첫 프롬프트:** 이 최신 단락과 `docs/next-verification.md`, 변경하려는 기능의
안내 문서만 먼저 읽어. 레포 전체·상위 폴더의 다른 프로젝트를 재탐색하지 마.
`next-operations` 공개 계열 작업을 사용하고 원본 private main이나 낯선 미추적 M1 파일
네 개를 섞지 마. 실행 중인 alpha.3 앱·인증·데이터를 보존하고, 빈 런타임 전용 검사는
격리 QA에서만 해. 공개 push·태그·릴리스·운영 서버 교체는 후속 사용자 지시가 있을 때만 해.
이 아래의 미발행/실행 버전/브랜치 안내는 해당 시점의 과거 기록이야.

## alpha.3 릴리스 후속 (2026-11-13)

사용자가 alpha.3 발행과 기존 앱·데이터를 유지하는 실제 서버 업데이트를 요청했어.
`public-alpha-3`는 아래 진단 구현에서 이어진 공개 계열 브랜치이며 제품 버전은
`0.1.0-alpha.3`이야. 설치·릴리스·문제 해결 안내를 해당 버전에 맞췄어.
발행·공개 소스 재빌드·실제 교체 결과는 GitHub 해당 릴리스 본문과 작업별 비공개
완료 보고서를 확인해. 아래의 “미발행”, “서버 교체 없음”은 진단 개발 완료 당시 기록이야.

## 최신 후속 작업: 진단 CLI (2026-11-13)

공개 alpha.2 소스에 기반한 로컬 `diagnostics-cli` 브랜치에서 `sw get/logs/events/why`를
구현했어. 제품 버전은 미발행 `0.1.0-alpha.3-dev`이며 새 릴리스나 서버 교체는 하지 않았어.
[문제 해결 안내](troubleshooting.md)와 [실측 기록](diagnostics-verification.md)을 먼저 봐.
Go 전체·Python 40·M1 103/0·M0 47/0·실제 장애 16개를 확인했어.

장애 테스트 전에 기존 WSL 서버가 내려가 있는 것을 발견해 설치된 alpha.2와 원래 옵션으로
복구했어. 앱 선언·파일·UID·IP는 유지됐고 재시작 수는 복구로 2→3이 됐어.
그 뒤 격리 장애 실험 동안에는 같은 서버/태스크와 재시작 수 3을 유지했어.
기본 서버에 빈 런타임 전용 스크립트를 실행하지 마.

**다음 세션 첫 프롬프트:** 이 핸드오프의 최신 단락과 `docs/troubleshooting.md`,
`docs/diagnostics-verification.md`부터 읽고 변경 대상만 확인한다. 레포 전체·상위 폴더의
다른 프로젝트를 재탐색하지 않는다. 공개 alpha.2에서 이어진 `diagnostics-cli` 작업인지
확인한다. 실제 앱은 alpha.2로 유지한다. 공개 push·태그·릴리스·서버 교체는 후속 사용자
지시가 있을 때만 한다. 아래 공개 준비 지침은 과거 기록이며 현재 배포 상태로 해석하지 않는다.

# 핸드오프 (2026-11-12, 공개 알파 준비)

## 가장 최근 작업

- 사용자 결정: 이름 `shearwater`, 라이선스 Apache-2.0. 공개 저장소 생성·push·릴리스 발행은 하지 않는다.
- README, quickstart, 호환표, 기여·보안 문서, 고지 원문, CI, 자체 nginx 예제, portable 부하/재부팅 도구를 준비했다. 자세한 실측과 공개 전 남은 단계는 [공개 알파 기록](alpha-release.md)에 있다.
- 기존 WSL과 별개로 공식 Ubuntu 24.04.5 이미지를 import하고 Go 1.27.3/containerd 2.2.1/runc 1.3.4/kubectl 1.36.6을 새로 준비했다. WSL의 호스트 네트워크 공유를 확인해 QA를 별도 netns 안에서 실행했다.
- 새 예제의 HTTP·로그·Service, 서버 재시작 인계, 컨테이너 강제 종료 후 자동 재시작을 확인했다. QA 배포판만 실제 terminate한 뒤 같은 UID/IP/ClusterIP·인증 해시·SQLite 무결성 복구도 확인했다.
- M1 103/0, M0 47/0, 새 환경 Go 검사와 빌드를 통과했다. 새 도구의 60.382초 실측은 요청 1,998건·의도적 연결 종료 60건·예상 밖 오류 0이다. 아래의 기존 68분 32초 부하 실험과 합산하지 않는다.
- 공개 준비 중 재시작 시도의 공유 cgroup 충돌과 WSL 배포판 재시작 뒤 빈 cgroup 잔존을 찾아 고쳤다. 최종 바이너리 SHA256 `b99450aee8bfa9cae9185f6249cb6ba3f572ea09ae5ec4971ec148743ea6f564`로 전체 수명주기·즉시 정리·실제 QA 배포판 재시작·마지막 정리를 통과했다. 최종 부하 60.377초, 요청 1,998건·의도적 종료 60건·예상 밖 오류 0, 최대 sw RSS 33,896 KiB다. 실제 populated cgroup의 EBUSY 거절도 확인했다.
- QA의 전용 containerd·Pod·네트워크를 정지하고 검사 자료와 VHDX는 보존했다. 기본 환경의 앱은 건드리지 않았다. 로컬 배포 후보는 저장소 밖 검증 폴더에 만들며, 공개 저장소·GitHub Actions 원격 실행·Release 발행과 구분한다.
- 공개 문서의 개인 경로와 내부 식별자는 일반화했다. 원문과 실제 운영 경로는 저장소 밖의 검증 기록에 보존했다. Git 이력을 다시 쓰지는 않았으므로 과거 커밋에는 일반 경로와 저자 메타데이터가 남아 있다.
- 실제 기본 서버의 기존 웹 앱은 유지한다. 비어 있는 런타임을 전제로 하는 M2–M6/alpha 검사를 그 기본 서버에 실행하면 안 된다. 낯선 미추적 M1 파일 네 개도 그대로 둔다.
- 이번 변경의 딥시크 검토를 2026-11-13에 받았다: `docs/meetings/2026-11-13-alpha-release-deepseek.md`, 판정 CONCERNS. 지적 P1·P3는 반영했고 P2(근거 파일 경로)는 소유자 판단, P4(`dist/sw`가 `main` 빌드가 아님)와 P5(미추적 4개 파일 제외)는 공개 전 확인, P6(내부 작업 기록 노출)은 선택이다. 그 세션은 Go 검사·gitleaks 재실행·GitHub Actions를 실행하지 못했다. 이전 마일스톤의 의견을 이번 코드의 승인으로 취급하지 않는다.

## 다음 세션 첫 프롬프트

> Shearwater 공개 알파 후속 작업을 이어간다. 먼저 docs/HANDOFF.md와 docs/alpha-release.md만 읽는다. 레포 전체나 상위 작업 폴더의 다른 프로젝트를 탐색하지 않는다. README/quickstart/compatibility와 변경 대상 파일만 필요한 만큼 읽는다. Apache-2.0과 프로젝트 이름 shearwater는 사용자 확정이다. 실제 기본 WSL의 기존 앱을 보존한다. 공개/push/이력 재작성은 별도 지시가 있어야 한다. 딥시크 검토(2026-11-13, CONCERNS)는 받았고 P1·P3를 반영했다. 사용자가 정한 공개 방식: 주소 `ChoYugyeong/shearwater`, **비공개 생성 → CI 초록 확인 → 공개 전환**, 첫 push 이력은 **스냅숏 1커밋**(로컬 브랜치 `public-alpha`, 트리는 로컬 `main`과 동일), 실행은 코덱스. 남은 것은 CI 결과 확인과 공개 전환·GitHub 비공개 취약점 제보 설정이다. 로컬 커밋은 경로를 명시하고 git add -A를 쓰지 않는다.

아래는 이전 마일스톤의 실측 기록이야. 현재 준비 상태는 위 내용과 alpha-release.md를 기준으로 봐.

## 이전 실환경 검증 상태

- **2026-11-12 실제 재부팅·장시간 검사 완료**: 배포판 종료와 WSL VM 전체 종료 뒤 `sw up`으로 같은 Pod·Service IP, 인증 정보, SQLite 무결성 복구를 확인했다. 실제 68분 32초 동안 Service 208,033건·Windows 6,295건에서 예상 밖 오류 0, 최대 관측 sw RSS 36.76MiB. 부하 후 FD 22·파이프 0, doctor OK. 누적 재시작은 재부팅 때문에 2이며 부하 중 추가 재시작은 없다. WSL 시간 보정과 Windows 정밀도 문제를 검사 도구에서 고쳤다. 상세·재현은 [재부팅·부하 검사 기록](reboot-soak.md).

- **2026-11-12 실제 앱 QA**: `local-apps/sample-web`이 기본 서버에서 실행 중이다. 새 [실제 배포 QA 기록](deployment-qa.md)에 27개 수명주기·장애·HTTP 검사와 M1 103/0, M0 47/0을 기록했다. 사용 중인 앱이 있으므로 빈 런타임을 요구하는 M2–M6 스크립트를 바로 실행하지 않는다. 접속·최종 배포 경로는 `<private-evidence>\shearwater-sample-web-20261112\deployment.md`에서 읽는다.
- 프로젝트 이름 Shearwater, 바이너리 `sw`. 폴더는 아직 `<workspace>/shearwater`. Go 모듈 이름은 `shearwater`.
- 핵심 원칙(사용자): **경량화와 사용자 편의성.** 설계 0.1절.
- 설계는 `docs/design-stage1.md`에 있다. M6까지 구현했다. 시작은 [README](../README.md), 명령·정지·복구 설명은 [운영 안내](operations.md), 완료 기준별 검사는 [1단계 검증](stage1-verification.md)에서 본다. Service의 사용 예제와 내부 흐름은 [Service 네트워킹 안내](service-networking.md)에 있다.
- git: 로컬 `main`만. 원격 없음. push는 사용자가 말할 때만. 커밋은 경로를 지정해서 묶음별로 한다. M1 커밋은 `483e994..`(테스트 자료)부터 문서 커밋까지.

## M1에서 만든 것

| 경로 | 내용 |
|---|---|
| `internal/patch` | JSON merge patch(RFC 7386), JSON patch(RFC 6902), strategic merge patch. SMP는 apimachinery의 `mergeMap`을 단계별로 따라 짰고, 스키마는 우리 타입의 `patchStrategy`·`patchMergeKey` 태그에서 읽는다 |
| `internal/protobuf` | `kubectl create <종류>`의 Protobuf 요청 본문을 JSON으로 바꾼다. 와이어 리더(`protobuf.go`)와 필드 번호 표(`tables.go`, v1.36.6 `generated.proto`에서 옮김) |
| `internal/store` | watch 허브(`Changed`), 변경 로그 읽기(`Changes`), 정리(`compact`, 쓰기 트랜잭션 안에서 1분에 한 번, 최근 1시간 또는 1만 줄 중 더 많이 남는 쪽) |
| `internal/api/watch.go` | watch: 커서 뒤의 변경 로그를 배치로 읽어 보냄, 셀렉터 전이, watch-list 끝 북마크, 410, `timeoutSeconds`, 1분 북마크 |
| `internal/api/table.go` | Table 응답(pods, deployments, replicasets, services, nodes, namespaces, events, endpointslices), `includeObject` |
| `internal/api/handlers.go` | PATCH 3종. PUT과 PATCH가 `replace` 하나로 모인다(읽기 → 변경 → 기본값·검증 → 조건부 커밋이 한 트랜잭션) |
| `internal/apis/grades.go` | 저장만 필드 목록과 `Warning` 문구 |
| `hack/m1-check.sh` | 실제 kubectl 확인(코덱스 작성). `hack/testdata/m1/`에 자료와 `RESULTS.md` |
| `hack/m1-capture.sh` | kubectl이 보내는 Protobuf 본문 캡처 |
| `hack/t.sh` | gofmt 확인 + vet + 테스트를 한 번에(`bash hack/t.sh [패키지]`) |

테스트 자료(구현보다 먼저 커밋): `internal/patch/testdata/smp.json`(73개), `internal/protobuf/testdata/`(본문 10종 × kubectl 2버전 + 기대 JSON), `internal/api/testdata/watch-transitions.json`.

## 실제로 돌려 확인한 것 (2026-11-11, 이 WSL)

- `bash hack/t.sh ./...`: gofmt, `go vet`, `go test` 통과(api, apis, patch, protobuf, store).
- `WIN_KUBECTL=/mnt/c/ProgramData/chocolatey/bin/kubectl.exe bash hack/m1-check.sh`: **103개 통과**(kubectl v1.36.6으로 91개, kubectl.exe v1.36.0으로 12개). 내용: Table과 `-o wide`, `get -w`(Table)와 셀렉터 watch의 ADDED/DELETED 전이, `apply` 반복(컨테이너 추가·삭제·순서 변경, env 삭제, RollingUpdate→Recreate, emptyDir→hostPath, 매번 같은 apply는 unchanged이고 리비전 불변), `edit`, `patch` 3종, `label`, `annotate`, `set image`, `create deployment`·`create service clusterip`·`create namespace`·`debug --copy-to`(Protobuf 본문), 저장만 경고, `apply --server-side` 415, finalizer 삭제 대기, `wait`, watch-list 끝 북마크.
- 같은 바이너리로 `hack/m0-check.sh` 47개 통과.
- kubectl 실측 추가분은 `docs/kubectl-trace.md` 26~41번.
- 바이너리 18,073,666바이트(strip 안 함, M0보다 약 355KB 증가). 유휴 메모리·디스크 쓰기는 다시 재지 않았다(M6).

## M1 종료 당시 돌려 보지 않은 것

- M1에서는 `kubectl rollout status`와 `logs -f`를 돌리지 않았다. M4에서 `rollout status`의 watch-list를 실측했다.
- `go test -race`: 이 환경은 `CGO_ENABLED=0`이라 돌리지 못했다.
- 윈도우 kubectl은 12개 표본만 돌렸다(리눅스 91개 전체를 반복한 것이 아니다).
- 변경 로그 1만 줄·1시간 정리는 단위 테스트(작은 값)로만 확인했다. 실제 크기로는 돌리지 않았다.

## M6에서 만든 것

- `sw up`은 SQLite 파일시스템, containerd·도구, IPv4 전달, 대역과 iptables를 확인한다. 준비되면 전용 잠금 아래 서버를 시작하고 인증된 접속을 확인해 kubeconfig의 사용법을 출력한다. 누락된 준비물은 무엇과 조치 방법을 알려 준다.
- `sw down`은 실제 PID 핸들·시작 시각·인스턴스 식별자를 대조해 해당 서버만 멈춘다. `--all`은 소유한 컨테이너·네임스페이스·전용 장치와 iptables의 정확한 규칙만 제거한다. SQLite와 IP 예약, 볼륨·인증서·로그는 보존한다. 다른 DB가 살아 있는 Pod를 인수하거나 다른 DB의 kubeconfig를 덮어쓰지 못하게 한다.
- `sw status`와 `sw tree`는 같은 리비전의 인증된 API 목록으로 Node·Pod 요약과 UID 소유 관계, Service 연결 대상 수를 그린다. `sw doctor`는 호스트를 변경하지 않고 검사와 경로, 최근 서버 오류를 출력한다. 스케줄·시작·시작 실패 이벤트는 전이할 때만 저장하며 Pod 수명에 묶인다.

## M6 실측 (2026-11-11, WSL Ubuntu-24.04)

- `hack/e2e-stage1.sh` 통과. `/var/lib/shearwater/check/stage1.zv7JxeeO/results.txt`에 번호별 결과가 있다. SIGKILL 뒤 같은 컨테이너 ID·PID·재시작 횟수로 인계, 서버 중단 중 태스크 종료 뒤 재시작·종료 시각 기록, `down --all` 뒤 빈 런타임 디렉터리로 같은 Pod UID·IP와 ClusterIP의 HTTP 복구를 실측했다.
- `bash hack/t.sh ./...` 통과. 격리 네트워크에서 외부 규칙이 들어간 동명 iptables 체인이 보존되는지와 Service 경로 충돌 검사도 통과했다. 일반 사용자로 `--runtime=none`의 시작·조회·진단·정지를 실행했고, 두 번째 DB의 설정 디렉터리 재사용을 거절했다(`~/.cache/shearwater-dev/ops-none.u2p4IRm5`).
- Linux와 Windows kubectl로 M1 103/0과 M0 47/0을 확인했다. `~/.cache/shearwater-dev/m1-20261111-224805-789`, `m0-20261111-224835`에 기록이 있다.
- 최종 바이너리로 M2–M5 통합 회귀도 통과했다. 기록은 `/var/lib/shearwater/check/`의 `m2.M9VDCerH`, `m3.m7mMbMZi`, `m4.OzGJKqJh`, `m5.sIfuamGB`다.
- `hack/measure.sh` 통과. Pod 10개, 60.003초 유휴에서 디스크 쓰기 0 B·파일 변경 알림 0건, CPU 0.1167%, 최대 RSS 34,762,752 B, 앱 외 Pod 태스크 0개를 관측했다. 바이너리 35,664,141 B, 시작 55.2 ms. 원본 `/var/lib/shearwater/check/measure.to6905k7/measure.json`. `go version -m` 결과 의존 모듈 46개, `k8s.io/*` 0개(`~/.cache/shearwater-dev/m6-codex/modules-m6.txt`).
- M6 최초 검증 때는 실제 WSL 재부팅을 하지 않았다. 11-12에 실제 배포판 및 VM 재시작을 추가 확인했다([후속 기록](reboot-soak.md)). Docker 네임스페이스와 설정 파일은 읽거나 변경하지 않았으므로 직접 전후 비교는 하지 않았다.

## M5에서 만든 것

- Service 생성과 같은 SQLite 트랜잭션에서 자동 또는 명시 ClusterIP를 예약한다. Finalizer가 있으면 객체가 실제로 삭제될 때 반납한다. 시작할 때 M4 Service의 기존 지정 IP부터 예약하고 빈 주소를 채운다.
- 같은 네임스페이스의 Ready이고 삭제 중이 아닌 Pod로 EndpointSlice를 맞춘다. 이름 포트가 Pod마다 다른 숫자로 풀리면 포트 조합별 Slice를 만든다.
- `sw-svc0`에 붙인 `ClusterIP:port`에서 Go TCP 프록시가 연결을 받고, 매 연결마다 Pod 후보를 돌아가며 고른다. 실패한 TCP 연결은 다음 후보를 시도한다. Service UID·generation과 Pod의 UID·IP 예약·라벨·Ready를 확인하고 바이트를 넘긴다. 기존 호스트 주소나 경로와 겹치는 IP는 붙이지 않는다.
- Pod의 첫 컨테이너 실행 시도 직전에 같은 네임스페이스 Service의 `_SERVICE_HOST`·`_SERVICE_PORT`·이름 포트 환경변수를 저장한다. 컨테이너 재시작에도 처음 값을 사용하고 명시한 `env`가 우선한다.

## M5 실측 (2026-11-11, WSL Ubuntu-24.04)

- `hack/m5-check.sh` 통과. 자동 `10.96.0.1`과 지정 `10.96.0.40`, 중복 주소 거절, 이름이 같은 `targetPort`를 8080·8081로 푼 두 Slice, 새 TCP 연결 분산, Pod 환경변수와 명시값 우선을 확인했다. Pod 안의 `wget`이 자기 Service를 거쳐 자기 HTTP 서버에서 응답을 받았다.
- Selector 변경과 삭제 대기 중 Pod 제외, `sw server` 종료·재시작 후 같은 ClusterIP로 연결 복구, Service 삭제 후 주소 제거와 재할당을 확인했다. `kubectl expose deployment`로 만든 Service는 재할당된 Pod IP `10.244.0.3`으로 `curl`을 전달했다. 재사용 때 남았던 호스트 브리지의 IP→MAC 캐시 항목을 새 veth 연결 직전에 지운다. 기록은 `/var/lib/shearwater/check/m5.1m3YInPC`에 있다.
- `bash hack/t.sh ./...` 통과. 독립 코덱스 리뷰에서 확인한 IP 예약 확인과 Ready 조기 전이 등을 반영했다. 별도 root 테스트 `TestServiceAddressesProtectHostRoutes`에서 격리 네임스페이스의 충돌 경로를 추가하고, 충돌한 ClusterIP만 거절하거나 제거하며 다른 IP는 유지하는 것을 확인했다.
- 같은 바이너리로 M4·M3·M2 회귀를 확인했다. WSL 기록은 `/var/lib/shearwater/check/m4.5l5IEf4m`, `/var/lib/shearwater/check/m3.IEFShO5R`, `/var/lib/shearwater/check/m2.9ltfReYq`다. Linux와 Windows kubectl로 M1 103/0, M0 47/0을 다시 확인했다(`~/.cache/shearwater-dev/m1-20261111-215046-726`, `m0-20261111-215117`).

## M4에서 만든 것

- `internal/controller`가 현재 Pod에서 서지·가용성을 계산한다. 종료 중인 Pod도 서지 슬롯을 잡고, Ready이면서 삭제 중이 아닌 Pod만 가용하다. 정수·백분율 한도와 Recreate를 지원한다.
- Deployment와 ReplicaSet의 `/scale`에 GET·PUT·세 가지 PATCH를 구현했다. 부모의 `spec.replicas`만 바꾸며 `uid`·`resourceVersion` 검사, dry-run, 변경 없는 쓰기를 처리한다. 실측 kubectl의 PUT은 JSON이고 `Content-Type`을 생략한다.
- 현재 ReplicaSet과 Deployment의 리비전을 맞추고 이전 템플릿의 ReplicaSet을 재사용하면 새 번호를 준다. 마지막 할당 번호는 SQLite에 보관한다. 롤아웃 완료 뒤 `revisionHistoryLimit`에 맞춰 옛 세트를 정리한다. kubectl이 읽는 `Available`·`Progressing` 조건과 `kubernetes.io/change-cause`도 갱신한다.
- 일시 정지 중에는 새 템플릿을 만들지 않는다. 복제본 수를 바꾸면 기존 세트 중 최신 세트부터 조정한다. 진행 중 스케일은 새 리비전을 우선한다. `minReadySeconds`와 `progressDeadlineSeconds`는 기존 설계대로 저장만 한다.

## M4 실측 (2026-11-11, WSL Ubuntu-24.04)

- `hack/m4-check.sh` 통과. 복제본 3개, `maxSurge: 25%`, `maxUnavailable: 0`인 롤아웃에서 Pod 변경 이벤트 18개를 검사했다. 종료 중 Pod를 포함해 최대 4개였고 Ready는 최소 3개였다. kubectl v1.36.6의 `rollout status`에서 watch-list 요청 10건이 200이었다.
- `maxSurge: 0`에서 일시 정지 중 Pod 삭제와 ReplicaSet Background 삭제를 각각 재현했다. 두 경우 모두 종료 중 Pod를 포함해 최대 2개였다. 리비전 주석 덮어쓰기가 번호를 바꾸지 않았고, ReplicaSet을 다시 만들 때 리비전 4가 할당됐다.
- Deployment·ReplicaSet `/scale` GET·PUT·PATCH 200을 추적 로그에서 확인했다. `--current-replicas` 성공·불일치 거절과 서버 dry-run을 확인했다. `rollout history --revision=1`, 이전 ReplicaSet을 재사용한 `rollout undo`(리비전 3), `revisionHistoryLimit: 0`으로 옛 세트 제거도 통과했다. 증거는 `/var/lib/shearwater/check/m4.RGkL1Zqn`에 있다.
- `bash hack/t.sh ./...` 통과. 단위 테스트에서 서지·가용 수, Recreate, 실패한 중간 리비전, 잠시 Ready를 잃은 기존 Pod 보존, 리비전 전제조건과 동시 스케일 쓰기를 확인했다.
- M3·M2 회귀 통과. WSL 기록은 `/var/lib/shearwater/check/m3.wVWc1nPJ`와 `/var/lib/shearwater/check/m2.rn8JhUfc`다. M1 103/0, M0 47/0 기록은 `~/.cache/shearwater-dev/m1-20261111-194330-766`과 `~/.cache/shearwater-dev/m0-20261111-194400`이다.

## M3에서 만든 것

- `internal/controller`가 저장소 변경 알림을 받아 현재 상태를 SQLite 쓰기 트랜잭션 안에서 읽고 조정한다. 5분 재확인과 오류 백오프가 있다. M3에서는 단일 노드의 현재 객체를 한 번에 읽는다. 키별 큐는 설계 후속 작업이다.
- 노드가 Ready면 `spec.nodeName`이 빈 Pod를 묶는다. Deployment는 템플릿 해시와 소유 참조가 있는 ReplicaSet을 만들고, ReplicaSet은 매칭 Pod의 입양·생성·교체·축소를 수행한다.
- GC는 Background·Foreground·Orphan을 처리한다. 컨트롤러에서 Pod를 지울 때 API 삭제와 같은 유예 종료 경로를 쓴다.
- API 전용 모드에서는 노드 에이전트와 조정 루프가 꺼져 있다.

## M3 실측 (2026-11-11, WSL Ubuntu-24.04)

- `hack/m3-check.sh`로 `kubectl create deployment` → Ready ReplicaSet·Pod, 자동 노드 바인딩, Pod 삭제 후 새 UID로 교체를 확인했다. Background·Foreground 연쇄 삭제와 Orphan 자식 보존도 확인했다. 실행 기록은 `/var/lib/shearwater/check/m3.JaX2L8ub` 아래에 남는다.
- `bash hack/t.sh ./...` 통과. 단위 테스트에서 NotReady 상태와 Ready 이후 바인딩, 정착 후 리비전 유지, 입양·교체, Foreground가 손자 Pod의 finalizer를 기다리는 흐름을 확인했다.
- 독립 검토에서 selector가 `pod-template-hash`를 제한할 때의 반복 생성과 Foreground 소유권 순환을 찾아 고쳤고, 두 경우를 단위 테스트에 추가했다.
- M2 회귀 통과. 실행 기록은 `/var/lib/shearwater/check/m2.WHjXmCUg` 아래에 남는다. M1 회귀는 103/0이고 실행 디렉터리는 `~/.cache/shearwater-dev/m1-20261111-180255-1031`이다.
- M0 회귀 47/0. 실행 디렉터리는 `~/.cache/shearwater-dev/m0-20261111-180328`이다.

## M2에서 만든 것

- `internal/runtime`에 containerd 네이티브 클라이언트, rtnetlink Pod 네트워크, 테스트용 가짜 구현을 추가했다. IPv4 주소는 SQLite에 보관한다. 노드 에이전트의 런타임은 `shearwater` 네임스페이스를 쓰며 한 인스턴스만 실행할 수 있다.
- `internal/node`에서 이 노드에 바인딩된 Pod를 실행하고, 종료를 감지해 정책과 백오프로 재시작한다. Node Ready, 유예 삭제, 고아 정리를 구현했다.
- shim 파일을 `kubectl logs`에 연결했다. 최근 두 시도의 로그를 보관하고 `--previous`를 지원한다.
- 사용자 결정에 따라 `hostPath.type`의 런타임 검사를 추가했다. `DirectoryOrCreate`는 디렉터리를, `FileOrCreate`는 부모 디렉터리가 있는 경로에 파일을 생성한다. Pod 삭제 후에도 hostPath는 유지한다.
- WSL에서 `sw server`를 root로 실행하면 기본값 `--runtime=auto`가 노드 에이전트를 켠다. API만 실행하려면 `--runtime=none`을 쓴다.

## M2 실측 (2026-11-11, WSL Ubuntu-24.04)

- [첫 확인](m2-first-checks.md) 37개 통과. shim 로그, `DOCKER-USER`, MTU 1500, 네임스페이스 재생성 시 같은 IP를 확인했다. M2 당시 같은 IP 확인은 재부팅 모의 실험과 가짜 런타임 테스트로 수행했다. 실제 WSL 재부팅은 11-12 [후속 검사](reboot-soak.md)에서 확인했다.
- `bash hack/t.sh ./...` 통과. 가짜 런타임으로 재시작과 백오프, 삭제 시 이미지 풀 취소, 교체된 네임스페이스에서 재실행, 고아 정리를 검증했다.
- `hack/m2-check.sh` 통과. Ready Node, Running Pod 두 개(`10.244.0.2`, `10.244.0.3`), 현재·이전 로그, `DirectoryOrCreate` 생성·마운트와 삭제 후 보존, Pod 간 ICMP와 외부 ICMP·TCP를 실측했다. SIGKILL 후 재시작, 서버 재시작 후 기존 태스크 인계, 서버 중단 중 종료된 태스크의 복구, TERM을 무시하는 태스크의 유예 대기와 정리, 고아 스냅숏 정리도 확인했다. 실행 기록은 `/var/lib/shearwater/check/m2.NAi33gTy`에 있다.
- M1 103/0, M0 47/0. Linux kubectl v1.36.6과 Windows kubectl.exe v1.36.0을 썼다. 실행 디렉터리는 `~/.cache/shearwater-dev/m1-20261111-173713-665`, `~/.cache/shearwater-dev/m0-20261111-173747`이다.
- 빌드 크기 34,942,234바이트(strip 전). `go version -m`에 containerd 클라이언트 v2.3.7과 의존 모듈 46개가 기록됐고 `k8s.io/*`는 없다. 전체 출력은 `~/.cache/shearwater-dev/m2-codex/modules.txt`에 있다.
- 독립 코덱스 검토에서 이미지 풀 취소, 네임스페이스 식별, 유예 시간 오버플로, 유예 삭제 검증에 대한 지적을 받아 반영했다.

## 설계와 달라진 점 (설계 문서에 반영함)

- 저장만 경고는 생성 때와, 수정·패치가 그 필드를 새로 넣거나 바꿀 때만 낸다. `dnsPolicy: ClusterFirst`·`sessionAffinity: None`은 경고하지 않는다.
- Protobuf 본문의 프로브·affinity·securityContext(저장만 등급의 메시지 필드)는 표를 만들지 않고 거절한다.
- 패치 적용 오류는 422. 모르는 `$` 지시어와, 쿠버네티스 라이브러리가 패닉하는 입력(병합 키 없는 객체 목록에 `$setElementOrder`)도 422.
- `Store.Changes`는 0 이하의 limit을 error로 거절한다(딥시크가 panic을 찾음).

## 고정 자료의 기대값이 나온 곳

- `internal/patch/testdata/smp.json`의 expected/error와 `internal/protobuf/testdata/kubectl-v1.36.6/*.json`은 실제 `k8s.io/apimachinery` v0.36.6으로 뽑았다(2026-11-10).
- 뽑는 프로그램(`hack/oracle`, `k8s.io`를 가져오는 별도 Go 모듈)은 **사용자 결정(2026-11-11)으로 레포에서 뺐다. 자료만 남긴다.** sw 소스의 import와 실제 바이너리에는 `k8s.io` 의존이 없다. 상위 containerd가 선언한 전체 모듈 메타데이터에는 CRI 서버용 의존성이 있으므로 `go list -m all`과 실제 링크 목록은 구분한다.
- 사례를 추가하려면 기대값을 쿠버네티스에서 다시 얻어야 한다. 프로그램은 git 기록에 있다: `git show 483e994:hack/oracle/main.go`. 되살릴 때는 레포 밖(예: `~/.cache/shearwater-dev/`)에 두고 돌린다.

## M1 마감 검수의 경계 4건 (2026-11-11, 전부 고침)

코덱스가 `hack/testdata/m1/boundaries.py`로 재현한 네 건이다. 사용자 지시로 넷 다 고쳤다. 아래 "남은 일" 0번의 네 항목은 이것으로 끝났다.

| 경계 | 고친 내용 |
|---|---|
| 삭제 중인 객체에 새 finalizer 추가 | `deletionTimestamp`가 있으면 finalizer는 빼거나 순서를 바꾸는 것만 된다. 새로 넣으면 422(`metadata.finalizers`, FieldValueForbidden). PUT과 패치 3종이 `replace` 한 곳을 지난다 (`4f83c7c`) |
| `fieldValidation`에 잘못된 값 | 모르는 필드가 있든 없든 Strict·Warn·Ignore·빈 값이 아니면 400. POST·PUT·PATCH와 `/status`까지 `writeParams`에서 먼저 본다 (`4f83c7c`) |
| Deployment의 annotations만 바꿔도 generation 증가 | Deployment는 spec 또는 `metadata.annotations`가 바뀌면 generation이 1 오른다(`Resource.AnnotationsMoveGeneration`). labels만 바뀌면 안 오르고 `/status`는 올리지 않는다. ReplicaSet·Pod·Service는 spec만 본다. 설계 3.4 반영. Deployment의 규칙은 코덱스가 v1.36.6 `pkg/registry/apps/deployment/strategy.go`의 `PrepareForUpdate`와 대조해 확인했다. 다른 종류의 규칙은 소스로 확인하지 않았다 (`18555de`) |
| Strict에서 중복 JSON 키 | 중복 키를 경로와 함께 모아(`duplicate field "spec.replicas"`, `spec.template.spec.containers[0].name`) 모르는 필드와 같은 규칙으로 처리한다. Strict 400, Warn(기본)은 `Warning` 헤더 + 마지막 값, Ignore는 조용히 마지막 값. 설계 3.3 반영 (`18555de`) |

중복 키 검사가 닿는 곳(정한 것):

- **POST·PUT 본문**: 전체를 본다. 단 최상위 `status` 아래는 보지 않는다(주 리소스 쓰기는 status를 받지 않으므로 모르는 필드와 똑같이 판단 대상이 아니다).
- **PATCH**: 패치 문서 자체를 세 형식 모두 본다. merge·strategic은 객체 경로(`spec.replicas`), JSON patch는 배열 경로(`[0].value`)로 알린다. 패치를 적용한 결과 객체에는 중복이 생길 수 없다.
- **`/status` PUT**: 본문에서 실제로 받는 `status` 아래의 중복만 본다. `/status` PATCH는 위 PATCH 규칙을 그대로 탄다.
- **통째로 저장하는 "저장만" 필드(affinity, 프로브, securityContext, tolerations) 안쪽도 본다.** 검사가 타입과 무관하게 JSON 토큰을 훑는다(`apis.DuplicateFields`).
- **Protobuf 본문**: 해당 없음(JSON으로 옮긴 뒤에는 중복이 없다).

아직 조용히 넘어가는 경로(고치지 않음):

- **`/status` PUT의 모르는 필드**: `status` 아래에 타입이 모르는 필드가 있어도 경고 없이 버린다(M0부터 `/status`는 느슨하게 읽는다). 컨트롤러와 노드 에이전트가 쓰는 경로라 M2에서 같이 본다.
- **DELETE 본문(DeleteOptions)**: 중복 키와 모르는 필드를 검사하지 않는다. kubectl은 이 본문에 `fieldValidation`을 붙이지 않는다.

확인(2026-11-11): 단위 테스트 `internal/api/boundary_test.go`(4건), `internal/apis/duplicate_test.go`. `bash hack/t.sh ./...` 통과, `hack/m1-check.sh` 103개와 `hack/m0-check.sh` 47개 그대로 통과, `boundaries.py`는 "4 passed, 0 failed"(실행 기록 `~/.cache/shearwater-dev/m1-boundaries-20261111-033313-4531`).

## 회의

- 1차(설계 검토): `docs/meetings/2026-11-10-design-review.md`.
- M1 분담·리뷰: `docs/meetings/2026-11-11-m1-review.md`. 사용자 지시로 코덱스(`review-agent`)가 `m1-check.sh`, 딥시크(`review-agent`)가 watch·Table 테스트와 저장소 리뷰를 맡았다.
- 코덱스 M0 리뷰 원문은 2026-11-11에 `docs/meetings/2026-11-10-m0-review.md`로 복구했다(원 대상 `dd3e581`, 최초 답장은 queued만 확인). 딥시크 M0 답과 M1 전문도 `docs/meetings/2026-11-11-m1-review-deepseek.md`로 확보했다. `3598a0b`에서 남은 네 항목은 경계 검사로 재현했고(0/4, M1 회의록 참고) `4f83c7c`·`18555de`에서 고쳤다.
- M1 마감 검수(코덱스 `dev-codex-40`, 대상 `18555de`): `docs/meetings/2026-11-11-m1-closure-codex.md`. 경계 4/4, 관련 테스트 함수 5/5, `m1-check` 103개, `m0-check` 47개 통과. 클로드도 `boundaries.py`와 `go test -count=1 ./...`를 따로 돌려 같은 결과를 봤다. 이 판정은 쿠버네티스 API 전체와 같다는 뜻이 아니다.
- 코덱스에게 더 넓은 경계 사례 테스트(generation, 중복 키, 삭제·finalizer, 쿼리 검증, 패치, Protobuf 변형, watch, Table, 동시성, 강제 종료 뒤 지속성, 필드 등급)를 요청했다(memdex #121, 2026-11-11). 결과는 `hack/testdata/m1/`과 `docs/meetings/`에 새 파일로 오기로 했다. 아직 받지 못했다. Reason `send_message`는 이 프로젝트 세션에서 가드에 막히므로 코덱스에는 memdex 경로로 보낸다.
- 딥시크는 pull 모드라 Reason 메시지를 늦게 본다. 답장이 프로토콜 오류로 안 온 적이 있다(2026-11-11). 급하면 사용자가 붙여 넣어 전달한다.

## 남은 일

0. 코덱스의 넓은 경계 사례 테스트 결과가 오면 직접 돌려 확인하고, 결함은 고치거나 "알려진 차이"로 적는다. 복구한 M0 리뷰의 후속 네 건(generation, 삭제 중 finalizer, 중복 JSON 키, fieldValidation 값)은 끝났다(위 "경계 4건"). 통과 기록을 전체 호환성 PASS로 확대하지 않는다.
1. 이후 단계는 2단계 에이전트 인터페이스다. 1단계 명령 다섯 개와 kubectl은 지금 사용할 수 있다. 설계 8절의 나머지 명령·정책·MCP는 아직 구현하지 않았다.
2. 공개 저장소 이름은 `shearwater`로 확정했다. 현재 로컬 작업 폴더는 이 세션에서 이동하지 않는다.

## 주의

- root 용도는 `sw server` 실행과 `/opt/shearwater`, `/var/lib/shearwater`, `/run/shearwater`, `/etc/shearwater`, `/var/log/shearwater` 아래 작업으로 한정한다. M0·M1 확인은 root 없이 돈다.
- containerd 네임스페이스 `moby`, `moby_history`(Docker)는 읽지도 쓰지도 않는다. Docker와 containerd 설정 파일도 고치지 않는다.
- SQLite 파일은 `/mnt/c`가 아니라 WSL ext4에 둔다.
- `~/.cache/shearwater-dev/` 아래에 확인 실행의 흔적이 실행마다 하나씩 남는다(`m0-*`, `m1-*`, `m1-capture-*`, `smoke-*`, `ref/`는 받아 둔 proto, `oracle`은 레포에서 뺀 기대값 생성기의 바이너리). 치울 때는 safe-ops 규칙으로.
- `hack/m1-check.sh`와 `hack/m0-check.sh`는 같은 16443 포트를 쓴다. 다른 에이전트가 돌리는 중이면 `SW_PORT`를 바꾼다(m1-check는 포트가 쓰이고 있으면 거절한다).
- watch 요청을 `httptest.NewRecorder`로 부르면 끝나지 않는다. 스트림 테스트는 `httptest.NewServer`로 한다(`internal/api/watch_test.go`).
- Git Bash의 heredoc에서 `\\`가 `\`로 줄고, 작은따옴표 개수가 홀수면 명령이 깨진다. 역슬래시나 따옴표가 든 글자는 Write/Edit 도구로 쓴다.
- WSL에서 `pkill -f 패턴`을 `bash -c '...'` 안에 쓰면 그 셸 자신이 죽는다(명령줄에 패턴이 들어 있어서).
- 윈도우 `kubectl.exe`는 표준 입력을 읽는다. 루프 안에서 부를 때는 `</dev/null`.
- Go 소스는 커밋 전에 `bash hack/t.sh`로 gofmt를 확인한다.
- 이 프로젝트에서 사용자는 일이 늘어도 직접 짜는 쪽을 골라 왔다. 기존 것을 가져다 쓰는 안은 대안으로만 제시한다.

이전 M6 후속 프롬프트는 위의 최신 첫 프롬프트로 대체했어. 실제 운영 인스턴스의 정확한 경로·식별자는 비공개 실행 기록에서 확인해.
