# 운영 복구·상태 검사·AI 연결 검증 (2026-11-13)

이 기록은 공개 alpha.3에서 이어진 **`0.1.0-alpha.4-dev` 소스**에 관한 것이야.
새 공개 릴리스를 발행하거나 사용 중인 서버를 교체한 기록이 아니야.
기존 alpha.3 서버와 앱은 그대로 유지하면서 별도 WSL QA에서 새 기능을 검증했어.

## 무엇을 구현했나

| 기능 | 구현과 사용 안내 |
|---|---|
| 자동 시작·서버 복구 | `sw service render/install/start/stop/status/disable`, systemd가 추적하는 `sw supervise`; [안내](automatic-start.md) |
| 백업·복원 | 온라인 DB·인증 snapshot, 중지 후 볼륨 백업, 체크섬 검증, 새 폴더 복원; [안내](backup-restore.md) |
| 앱 상태 검사 | HTTP/HTTPS·TCP·exec, startup/readiness/liveness 일정·임계값·유예 종료; [안내](health-probes.md) |
| AI 연결 | stdio MCP 조회 6개, 선택적 `sw_plan`/`sw_apply`, CLI의 `plan/approve/apply`; [안내](agent-mcp.md) |

추가 Go 의존성은 없고 실제 링크 대상에 `k8s.io/*`는 없어.
SQLite 스키마는 1을 유지해. 프로브가 없는 앱에 대해서 alpha.3와 개발 버전 사이의
실행 파일 교체·복귀를 실측했지만, 구버전이 새 프로브 의미까지 구현한다는 뜻은 아니야.

## 환경과 격리

Linux amd64, WSL Ubuntu 24.04, Go 1.27.3, containerd 2.2.1,
Linux kubectl 1.36.6과 Windows kubectl 1.36.0을 사용했어.
SQLite와 테스트 앱 데이터는 WSL ext4에 뒀어.

WSL 배포판끼리 바깥 네트워크가 공유되므로 QA의 containerd와 sw는 별도 네트워크
네임스페이스에서 실행했어. QA 전용 소켓과 `shearwater` containerd 네임스페이스만 사용했고,
기존 Docker 네임스페이스나 Docker/containerd 설정 파일은 읽거나 변경하지 않았어.
자동 시작 테스트의 systemd wrapper는 이 격리 공간을 준비하는 QA 장치야.
제품 서비스는 기본적으로 `sw supervise`를 직접 실행해.

## 실제로 수행한 검사

| 검사 | 관찰 결과 |
|---|---|
| `. hack/env.sh` 뒤 `bash hack/t.sh ./...` | 포맷·vet·전체 Go 검사 통과. 가짜 런타임으로 프로브 임계값, startup 차단, readiness, 종료 유예와 정책, 재시작 상태 인계 검사 포함 |
| Python 기존 검사 | 40개 통과 |
| `hack/m1-check.sh` | 103 통과 / 0 실패, Linux·Windows kubectl 포함 |
| `hack/m0-check.sh` | 47 통과 / 0 실패 |
| `hack/e2e-stage1.sh` | 실제 Pod 실행·재시작·삭제, scale·롤링, Service HTTP, 서버 SIGKILL 후 인계, cold start, 정리 통과 |
| `hack/diagnostics-check.sh` | 이미지 실패·crash·hostPath 종류 오류·잘못된 실행 파일·다중 컨테이너·UID 교체 등 실제 장애 검사 16개 통과 |
| `hack/mcp-check.py` | 실제 stdio/API에서 조회 전용 경계·인자 주입 거절·dry-run·정확한 승인·실제 생성·재실행 거절·큰 계획 원문 조회 등 10개 통과 |
| MCP와 실제 컨테이너 | MCP로 Deployment와 Service를 계획·승인·배포하고 조회 도구 6개를 실행. 미승인 적용과 소비한 계획의 재실행 거절 |
| HTTP readiness | 건강 파일을 숨기자 Ready와 EndpointSlice 대상에서 빠짐. 컨테이너는 재시작하지 않음. 파일 복구 후 다시 준비됨 |
| TCP startup·exec liveness | 초기 HTTP 서버가 늦게 뜨는 동안 startup 검사가 기다림. liveness 파일을 숨기자 재시작하고 복구 후 정상화 |
| exec timeout | 50초 검사에 1초 제한을 주고 반복 관찰. 앱 본체의 ID·재시작 수 유지, 검사 프로세스가 누적되지 않음 |
| startup 종료 유예 | TERM을 무시하는 앱을 startup 실패로 종료. 유예 뒤 KILL의 종료 코드 137 확인. Never 정책은 재시작하지 않음 |
| OnFailure 정책 | liveness 실패 뒤 앱의 TERM 처리기가 코드 0으로 끝나도 실패 원인을 유지하고 재시작함 |
| systemd 복구 | sw만 SIGKILL한 뒤 자동으로 새 서버 PID가 뜸. 기존 앱의 UID/IP/컨테이너 ID/재시작 수는 같음 |
| 실제 QA WSL 재부팅 | QA 배포판만 실제 terminate 후 다시 시작. 수동 `sw up` 없이 앱·같은 Pod UID/IP·Service IP·인증 해시·HTTP 데이터 복구 |
| 백업·복원 | 실행 중 DB/인증 백업 검증, 실행 중 볼륨 백업 거절. 중지 후 DB/인증/hostPath 백업을 새 경로로 복원해 실제 HTTP 응답 확인 |
| 실행 파일 복귀 | 프로브 없는 앱에서 alpha.3 → 개발 버전 → alpha.3 → 개발 버전. 같은 Pod/Service UID·IP, 컨테이너 ID/PID/재시작 수, 인증과 데이터 유지 |

백업 단위 검사는 기존 대상 덮어쓰기·손상된 파일·손상된 서버 PID 기록을 거절하는지도 확인해.
배포 계획 단위 검사는 DB 변경 없는 dry-run, 타인 변경으로 낡아진 계획, 다른 서버 인증,
내용 변조, 만료, 두 번째 쓰기 실패 뒤 정확한 부분 결과와 재실행 거절을 검사해.
네트워크 오류의 부분 실패 검사는 의도적으로 두 번째 HTTP 쓰기에 오류를 주입한 것이며,
실제 인터넷 단절 실험으로 표현하지 않아.

M1의 “프로브는 저장만 한다” 경고는 이제 맞지 않아. 그 경고 검사는 여전히 저장만 하는
`securityContext`로 옮겼어. 진단 장애 검사도 프로브 없는 앱에 “앱 응답은 확인하지 않았다”는
설명이 남는지 확인하도록 갱신했어. 검사를 없애거나 실패를 무시해서 통과시킨 것은 아니야.

## 검증에서 고친 문제

1. **foreground 실행의 run 디렉터리 누락:** 새 systemd 실행에서 PID 기록 경로를 먼저
   준비하도록 수정했어. 기존 `up`으로 만들어진 디렉터리에 의존하지 않아.
2. **파일 생성 권한과 nginx 403:** QA wrapper가 restrictive umask를 containerd에 전달하면
   새 이미지 루트가 `0700`이 되어 nginx worker가 읽지 못했어. 서비스 기본 마스크를 `0022`로
   조정하고 새 nginx 루트 `0755`·HTTP 200을 실측했어. 인증 파일은 별도의 비공개 권한을 써.
   Pod의 hosts/hostname/resolv.conf도 앱 사용자가 읽을 수 있는 `0644`를 명시해.
3. **시간 초과 exec 정리 경쟁:** 신호 직후 삭제하지 않고, containerd의 종료 대기 후 삭제
   절차를 사용해. 정상 exec가 앱 본체를 죽이지 않는 것과 반복 timeout을 실제 확인했어.
4. **큰 계획의 검토 경로:** MCP 응답 제한을 넘는 계획도 해시와 대상 목록을 유지하고,
   `sw plan --plan=HASH`로 원문 전체를 읽을 수 있게 했어. 일부만 읽고 승인하도록 만들지 않아.
5. **부분 배포 식별:** 생성 결과 기록에 collection 경로만 남지 않도록 실제 객체 이름까지
   기록해. 같은 종류의 여러 객체 중 무엇이 적용됐는지 구별할 수 있어.

## 직접 다시 확인하기

일반 사용자로 먼저 실행해. 아래 MCP 검사는 임의의 빈 loopback 포트와 새 비공개 폴더에서
`--runtime=none` 서버만 만들고, 끝나면 자신이 띄운 프로세스를 정지해. 로그와 인증 파일이
들어 있는 증거 폴더는 보존하며 경로를 출력하니 공개 Git에는 넣지 마.

```bash
. hack/env.sh
bash hack/t.sh ./...
python3 -m unittest discover -s hack -p 'test_*.py'
bash hack/build.sh
python3 hack/mcp-check.py "$PWD/dist/sw"
bash hack/m1-check.sh
bash hack/m0-check.sh
```

컨테이너 검사는 사용 중인 클러스터가 없는 **별도 QA 공간**에서만 실행해.
기존 `hack/e2e-stage1.sh`와 `hack/diagnostics-check.sh`는 빈 런타임을 요구해.
새 프로브·백업·서비스의 수동 장애 재현 순서는 기능별 문서에 있고, 이번 호스트 전용
시나리오와 원본 로그는 비공개 검증 폴더에 보관했어. 호스트별 소켓·네임스페이스를
하드코딩한 사설 실험을 다른 사람의 PC에서 그대로 실행하게 배포하지 않아.

재부팅 실험에서는 대상 QA 배포판만 종료해. Windows 전체 또는 다른 WSL 배포판까지
멈추는 명령은 필요하지 않아. systemd 서비스만으로 WSL이 계속 켜져 있지는 않으므로
[WSL 수명 주기 조건](automatic-start.md)을 읽고 터미널/배포판 유지 조건을 준비해.

## 기존 앱 보존과 남은 한계

사용 중인 alpha.3 서버는 PID와 바이너리가 그대로였고 앱 컨테이너도 같은 PID/ID와
재시작 수 3을 유지했어. 설정 파일 8개와 앱 파일 32개의 해시·권한·소유자,
DB 선언·소유 관계·IP 할당을 이전 alpha.3 기준과 대조했고 SQLite 무결성은 `ok`였어.
Pod IP·Service IP·Windows 접속 주소에서 모두 HTTP 200과 같은 5,303바이트 본문 해시를 확인했어.
이번 작업에서는 브라우저 저장소를 다시 읽지 않았으므로 그 내용을 재검증했다고 하지 않아.

QA 종료 후 시험용 컨테이너·containerd·네트워크를 정리하고 자동 시작 unit을 비활성화했어.
백업·증거·unit 파일과 QA 배포판은 보존했어.

- MCP 검사를 기존 source CI에 추가했지만 GitHub Actions 원격 실행·공개 릴리스·운영 서버
  업그레이드는 이번 작업에 포함하지 않았어.
- 독립 동료 검토를 요청할 수 있는 같은 작업의 연결 세션을 찾지 못해 **자체 검증**으로 기록해.
- Windows 로그인 자동 시작, 전체 OS 재부팅, 다른 CPU 아키텍처·여러 노드·실제 폰,
  모든 상용 MCP 앱의 호환성은 이번에 검사하지 않았어.
- 새 버전의 장시간 부하·자원 사용량을 다시 측정하지 않았어. 이전 버전의 숫자를 새 기능의
  성능 결과로 옮기지 않아. 프로브는 설정한 주기로 동작하므로 검사가 없는 유휴 조건과도 달라.
- `--include-volumes`는 소켓·심볼릭 링크·이미지 쓰기 레이어·외부 DB·브라우저 저장소의 백업이 아니야.
  여러 객체 배포도 원자적 트랜잭션이 아니며, 정확한 범위는 각 안내를 기준으로 해.
