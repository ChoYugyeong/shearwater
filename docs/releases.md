# 릴리스와 버전 확인

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
python3 hack/check_version.py dist/sw --tag v0.1.0-alpha.6
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
