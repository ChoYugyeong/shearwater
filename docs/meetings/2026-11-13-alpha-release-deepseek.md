# 공개 알파 준비 변경 검토 (딥시크)

작성: 딥시크(이 세션). 대상: 로컬 `main` `4c20177`(2026-11-12, 80개 파일 +9,870/−422). 검토일 2026-11-13.
이 문서는 **공개 준비 변경의 검토**이고, 1단계 런타임 검증([공개 알파 기록](../alpha-release.md))을 대체하지 않아. push·공개·릴리스는 하지 않았다.

## 1. 범위

공개 저장소로 내보낼 때 문제가 되는 것만 봤어: 추적 파일의 개인정보·비밀, 라이선스·고지의 정합성, CI가 실제로 돌 수 있는 구성인지, 문서가 가리키는 경로와 수치가 지금 트리와 맞는지. 런타임 동작(M2–M6, 재부팅 복구)은 다시 검증하지 않았어.

## 2. 확인한 것

| 항목 | 확인 방법 | 결과 |
|---|---|---|
| 이 커밋의 코드 변경 | `git show 4c20177` | Go 변경은 `internal/patch/strategic.go`·`internal/protobuf/{protobuf,tables}.go`의 **라이선스 헤더뿐**. 동작 변경 없음 |
| 추적 파일 수 | `git ls-files` | 239개, 이력 60커밋, 저자 1명(`cho4639@gmail.com`), 태그 없음 |
| 추적 파일 비밀 유사 문자열 | 전체 이력 `git log -p`에 개인키·`AKIA…`·`ghp_…`·`sk-…`·`xox…` 패턴 grep | 일치 0건. (gitleaks 재실행이 아니라 패턴 검사야) |
| 이력의 큰 파일 | `git cat-file --batch-check` | 최대 187,026 B(`internal/patch/testdata/smp.json`). 바이너리·DB·이미지 없음 |
| `LICENSES/SHA256SUMS` | 37개 항목을 PowerShell `Get-FileHash`로 대조 | **37/37 일치**, 누락 0 |
| CI 액션 고정 | GitHub API `git/ref/tags` | `actions/checkout@v7.0.1`= `3d3c42e5…` ✓, `actions/setup-go@v7.0.0`= `b7ad1dad…` ✓ |
| gitleaks 체크섬 | GitHub API 릴리스 자산 `digest` | v8.30.1 `linux_x64` tar.gz = `sha256:551f6fc8…`, ci.yml의 값과 **일치** |
| Go 1.27.3 | go.dev `dl/?mode=json` | 존재함. `linux-amd64` SHA256 `65efbc80…` = quickstart 44행의 값 **일치** |
| 고지 정합성 | `NOTICE`·`THIRD_PARTY_NOTICES.md`·코드 헤더 표본 대조 | `strategic.go`(2014, apimachinery v0.36.6)와 `tables.go`·`protobuf.go`(v1.36.6 generated.proto)의 출처·수정 범위 표기가 서로 맞음 |
| 현재 트리 개인정보 | `git grep`(`cho46`, `ChoYugyeong`, `/home/`, `C:\`) | 문서는 일반화됨. 아래 P1·P2 두 곳만 남음 |

## 3. 찾은 것

**P1. `docs/quickstart.md:7`에 로컬 폴더 이름이 남아 있어.**
`소스 폴더 이름이 `agentkube`여도 제품명은 Shearwater야.` → 공개 문서에서 읽는 사람은 `agentkube`를 모르니 의미가 없고, 내부 저장소 이름만 드러나. `소스 폴더 이름이 달라도 제품명은 Shearwater야.`로 고치는 게 맞아. **반영함.**

**P2. `hack/testdata/m1/checks.observed.jsonl` 98–100행에 절대 경로가 있어.**
`"WK apply -f C:\\Dev\\agentkube\\hack\\testdata\\m1\\apply-00.json"` 처럼 Windows 작업 경로가 그대로 들어 있어. 판정 값(`passed`, `number`)은 근거라 건드리면 안 되지만 명령 문자열은 `hack/testdata/m1/apply-00.json` 같은 상대 경로로 바꿔도 근거의 뜻이 유지돼. 바꾸면 `hack/testdata/m1/RESULTS.md`에 "경로만 일반화, 판정 값은 그대로"를 한 줄 남겨야 해. **아직 안 바꿈**(근거 파일이라 소유자 판단이 필요해).

**P3. `README.md:66`이 저장소 밖 증거 경로를 가리켜.**
`/var/lib/shearwater/check/measure.to6905k7/measure.json`은 이 작업 환경에만 있는 파일이야. 다른 문서는 `<private-evidence>`로 쓰는데 여기만 실제 경로가 남았어. `전체 측정값 원본은 저장소 밖 검증 기록에 보관했고…`로 바꾸는 게 일관적이야. **반영함.**

**P4. `dist/sw`가 지금 `main`의 빌드가 아니야.** (릴리스 절차 문제)
로컬 `dist/sw`는 2026-11-12 22:36 빌드, SHA256 `cc6e7130…`, 35,625,551 B. `main` 커밋은 22:47이고, 문서에 기록한 최종 바이너리는 `b99450ae…`, 35,664,141 B(M6 측정 기준)야. 헤더 커밋 전에 만든 바이너리라서 당연히 달라. **릴리스에 바이너리를 올린다면 `4c20177` 이상의 트리에서 `bash hack/build.sh`로 다시 빌드하고 그 SHA256을 적어야 해.** 지금 `dist/sw`를 그대로 올리면 문서의 해시와 맞지 않아.

**P5. 미추적 파일 4개는 공개하면 안 돼.**
`docs/meetings/2026-11-11-m1-extended-codex.md`, `hack/testdata/m1/extended-{results,sources}.json`, `hack/testdata/m1/extended.py`. `extended-sources.json`에는 `C:\Dev\_verification\…`, 문서에는 `/home/cho46/…`가 있어. 지금 추적되지 않으니 그대로 두면 되지만, 공개 전에 `git add -A`를 쓰면 섞여 들어가. 핸드오프의 "경로를 지정해 커밋" 규칙을 지켜야 하는 이유가 이거야.

**P6. 공개 문서에 내부 작업 흔적이 남는 것은 선택이야.**
`docs/HANDOFF.md`, `docs/meetings/*`, `docs/deployment-qa.md`에는 에이전트 이름·memdex 번호·`<private-evidence>` 경로·`/opt/shearwater/deployments/…` 같은 내부 운영 기록이 있어. 개인정보는 아니지만 "공개 알파 저장소"로서는 노이즈가 커. 남길지, `docs/measurements/`처럼 성격을 구분할지, 아니면 저장소 밖으로 뺄지는 소유자 결정이야. 나는 **남기는 쪽을 권해** — 검증 근거를 숨기지 않는 편이 이 프로젝트의 문서 태도와 맞아. 대신 `docs/HANDOFF.md` 맨 위에 "내부 작업 기록" 한 줄을 두면 읽는 사람이 오해하지 않아.

**P7. `internal/protobuf/tables.go` 헤더의 저작권 줄이 겹쳐.** (사소함)
`Copyright The Kubernetes Authors.` 다음에 `2015`·`2017` 줄이 또 있어. 법적 문제는 아니고 업스트림 파일 헤더를 옮기다 생긴 모양이야. 한 줄로 정리하면 깔끔해.

## 4. 확인하지 못한 것

- **Go 검사·빌드·`hack/notices.py`·`alpha-check.sh`를 이 세션에서 못 돌렸어.** 이 Windows 세션에는 Go가 없고 `wsl.exe`가 `E_ACCESSDENIED`로 막혀 있었어. `go test`, `hack/t.sh`, `notices.py`, 실제 컨테이너 검사는 이전 기록(`alpha-release.md`)이 마지막 근거야.
- **kubectl v1.36.6 다운로드 URL은 확인 못 했어.** 이 세션에서 `dl.k8s.io`로 나가는 연결이 막혔어(`curl` 000, `Invoke-WebRequest` 연결 종료). CI가 첫 실행에서 확인해야 해.
- **gitleaks를 직접 돌리지 않았어.** 위 표의 비밀 검사는 내 정규식 검사고, gitleaks 8.30.1의 60커밋 전체 스캔은 아직이야.
- **GitHub Actions는 실행 전이야.** 저장소가 없으니 당연해.

## 5. 공개 전 남은 결정 (내가 정하지 않은 것)

1. **주소**: `github.com/ChoYugyeong/shearwater`(개인 계정, 기존 공개 저장소와 같은 패턴) / 새 조직 / 기존 조직 중 어디로. 지금 `ChoYugyeong`에 `shearwater` 저장소는 없어.
2. **이력**: 전체 60커밋을 공개하면 `C:\Dev\agentkube` 65줄, `/home/cho46` 56줄, `cho46/dev-*` 14줄, `memdex #` 7줄이 함께 공개돼. 현재 트리에는 `C:\Dev\agentkube`가 P2의 근거 파일 3줄에만 남아 있어. 새 스냅숏 1커밋으로 시작하면 이 흔적은 사라지고, 대신 3일치 작업 이력이 없어져.
3. **비공개 취약점 제보**: 저장소를 만든 뒤 Security → Private vulnerability reporting을 켜야 `SECURITY.md`의 안내가 실제로 동작해.

## 6. 판정

**CONCERNS — 고친 뒤 공개할 수 있어.** 공개 구성 자체(라이선스·고지·CI 고정·문서)는 일관돼 있고, 이 커밋이 동작을 바꾸지 않는 것도 확인했어. 남은 것은 P1–P3(문서 정리)와 P5(미추적 파일 제외), 그리고 이력·주소 결정이야. 첫 push 뒤 CI가 실제로 초록인지, gitleaks가 60커밋을 통과하는지는 그때 확인해야 해. 이 문서는 런타임 검증의 승인도, 보안 보증도 아니야.
