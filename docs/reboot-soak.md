> 공개 준비 과정에서 개인 경로·내부 세션 식별자·로컬 앱 이름을 일반화했어. 측정값과 당시 검증 범위는 유지했고, `<private-evidence>`는 공개 저장소에 없는 원본 기록을 뜻해.

# 실제 WSL 재부팅과 60분 이상 부하 검사

**실제 재부팅 두 번과 실제 시간 68분 32초의 연속 관측을 마쳤다.** Service 요청 208,033건, Windows 접속 6,295건이 예상대로 응답했고 예상 밖 오류는 0이었다. 부하 중 서버·Pod의 추가 재시작은 없었다. 이 범위에서 Shearwater 런타임을 수정해야 할 결함은 관측하지 않았으며, 검사 도구의 WSL 시간 보정 문제와 Windows 응답 시간 정밀도를 고쳤다.

## 검사 대상과 읽는 순서

2026-11-12, Windows의 Ubuntu-24.04 WSL에서 실행 중인 검증용 웹 앱을 사용했다. Shearwater는 로컬 `main`의 `617a4d4`이고 실행 바이너리는 M6 구현 `aed82d5`를 빌드한 `$HOME/.cache/shearwater-dev/sample-web-build.oGBdyL5b/sw`다. 이후 커밋은 실제 배포 검사와 문서 변경이다. 앱은 `18e6512`, 정적 자산 버전은 `0946c627287628186cdc`다.

이 검사는 [앞선 배포 QA](deployment-qa.md)의 짧은 요청 검사에 이어 실행했다. **재부팅 후 복구**와 **오래 사용했을 때 자원이 계속 쌓이는지**가 목적이다. 최대 동시 사용자 수나 최대 처리량을 찾는 벤치마크가 아니다.

읽을 때 다음 용어를 구분하면 좋아.

- **Pod UID**: 저장된 Pod의 고유 번호. 컨테이너가 다시 시작해도 같은 Pod라면 유지된다.
- **컨테이너 ID**: 실제 실행 시도 하나의 번호. WSL이 꺼져 프로세스가 사라지면 복구할 때 새 번호를 받는다.
- **Pod IP / ClusterIP**: 각각 컨테이너에 직접 연결하는 주소 / Service를 통해 연결하는 주소. 이번에는 `10.244.0.3` / `10.96.0.1`이다.
- **RSS**: 프로세스가 실제 RAM에 올려 둔 메모리. KiB는 1,024바이트, MiB는 1,024KiB다.
- **FD**: 프로세스가 연 파일·소켓·파이프의 개수. 요청 중에는 늘 수 있으므로 부하 종료 뒤 값도 비교한다.
- **p95 / p99**: 요청의 95% / 99%가 그 시간 안에 끝났다는 뜻이다. 평균만으로 드러나지 않는 느린 응답을 본다.

## 실제로 재부팅한 방법

현재 실행 중인 배포판이 Ubuntu-24.04 하나인지 먼저 확인했다. 다른 작업의 프로세스 이름·실행 여부를 확인하고, 호스트 변경 잠금 아래 다음 두 번을 실행했다. Docker·containerd 설정 파일이나 `moby`, `moby_history` 네임스페이스는 읽거나 수정하지 않았다.

| 구분 | 명령과 관측 | 복구 결과 |
|---|---|---|
| 배포판 정지·기동 | 19:39:38 KST `wsl --terminate Ubuntu-24.04`. 실행 중 배포판 목록이 비었고 `/run` 표식과 서버 상태 파일이 사라졌다. PID 1 시작 tick은 `5477` → `7401123` | `sw up` 후 같은 객체·IP 복구 |
| WSL VM 전체 정지·기동 | 19:40:57 KST `wsl --shutdown`. 커널 boot ID가 `0f812fee-977c-48f4-b19a-1c03e55b15bd` → `e369d7c8-9537-4c43-aef2-5aac0b19df59`로 바뀌었다. PID 1 시작 tick은 `85` | `sw up` 후 같은 객체·IP 복구 |

첫 번째 명령은 배포판을 실제로 껐지만 WSL이 커널을 유지했다. 그래서 첫 결과만으로 커널 재부팅까지 확인했다고 쓰지 않고, 두 번째 명령으로 새 커널 부팅을 따로 확인했다. Windows 자체를 재부팅하거나 물리 전원을 끊은 검사는 아니다.

`hack/reboot-snapshot.py`가 정지 전(`before`), 기동 직후(`booted`), `sw up` 뒤(`after`)를 따로 기록한다. 정지 전에는 SQLite의 **온라인 백업 기능**으로 일관된 사본을 만들었다. 실행 중 DB 파일 하나만 복사하면 WAL에 남은 변경을 놓칠 수 있기 때문이다. 인증 파일은 내용 대신 SHA-256 해시를 대조했다. 관리자 토큰을 로그에 출력하지 않는다.

두 번 모두 다음을 실제 비교했다.

- Pod·Deployment·Service의 이름 집합과 UID 유지.
- Pod `sample-web-138b594b-a506c591`의 `10.244.0.3`, Service의 `10.96.0.1` 유지.
- 새 컨테이너 ID, 증가한 `restartCount`, Pod Ready 복구. 재부팅 두 번 뒤 재시작 수는 **2**다.
- `admin.token`, CA 인증서, kubeconfig의 해시 유지.
- SQLite `quick_check`가 `ok`, `foreign_key_check` 결과 없음.

부팅 직후 `sw up`은 명시적으로 실행했다. **자동 시작을 설치하거나 검증한 결과는 아니다.** Windows 접속용 `shearwater-sample-web.service`는 일시적인 사용자 서비스라 재부팅 뒤 없어지는 것이 예상한 동작이다. 같은 명령으로 다시 시작하고 `http://127.0.0.1:18080/`의 HTTP 200을 확인했다. 재시작 명령은 아래에 있다.

## 60분 이상 부하의 구성

`hack/soak-check.py`의 `host` 모드는 WSL 안에서 Service의 TCP 프록시를 통과한다. `probe` 모드는 Windows에서 실제 사용자 주소를 통과한다. 두 모드를 동시에 실행한다.

| 경로 | 부하 |
|---|---|
| 최초 WSL → Service → Pod | 작업 스레드 6개. 3,600초를 세 구간으로 나눠 초당 20 → 60 → 20건 목표. 아래 시계 문제 때문에 이 실행의 시간만으로 실제 60분을 판정하지 않음 |
| Windows → `127.0.0.1:18080` → 중계 → Service → Pod | 초당 HTML 요청 1건. 처음 응답의 SHA-256과 계속 비교 |
| WSL → Service 연결 취소 | 초당 약 1회. CSS를 요청한 직후 TCP RST로 연결을 닫아 정리 경로 검사 |
| 보정 후 추가 WSL 검사 | 첫 검사를 중단하지 않고 RAW 시계 기준 45분 추가. 15분씩 초당 20 → 60 → 20건, 작업 스레드 6개 |
| 추가 Windows 검사 | 보정 후 검사와 함께 45분, 초당 1건. 최초 Windows 60분 검사와 겹쳐 실행 |

정상 요청은 HTML·JavaScript·CSS의 정확한 바이트, HEAD, Range 206, 없는 파일 404, ETag 조건부 요청 304를 순환한다. **예상한 404와 304는 성공**이다. 요청을 재시도해서 실패를 가리지 않는다. 의도적 연결 취소 횟수는 정상 HTTP 응답 수와 별도로 기록한다. 작업 스레드 절반은 매 요청 새 연결을 열고, 나머지는 8회마다 연결을 바꾼다.

부하 중 10초마다 서버·nginx master·측정 프로그램의 RSS, FD, 스레드, CPU tick, 디스크 쓰기와 로그 크기를 기록한다. nginx master 수치는 nginx worker 전체 사용량이 아니므로 앱 전체 메모리라고 해석하지 않는다. 1분마다 API의 Pod 목록도 비교한다. 같은 컨테이너와 상태가 유지됐는지 확인하며, 부하 중에는 다른 배포·스케일 작업을 하지 않는다.

종료 뒤 60초는 자원을 추가 관찰하는 시간이다. 이 시간을 **부하 시간에 더해서 채우지 않는다**. 최초 검사 종료 시에는 추가 검사가 계속되므로 그때를 유휴 상태라고 해석하지 않는다. 마지막 검사의 부하까지 끝난 뒤의 60초를 실제 휴지 구간으로 사용한다. 목표 요청 수의 80% 이상을 실제 응답으로 확인해야 통과한다. 정상 요청 오류 10회 누적, 취소용 연결 자체의 실패, 프로세스 교체, RSS 256MiB 초과, 디스크 여유 1GiB 미만이면 조기에 실패 처리한다. 256MiB는 이 검사의 중단 기준일 뿐 제품의 권장 메모리 수치는 아니다.

## 계측에서 발견해 고친 문제

최초 검사에서 WSL의 경과 시간과 Windows 진행 기록이 점점 벌어졌다. 같은 WSL 프로세스가 출력한 두 시각을 Windows에서 즉시 받아 비교했다. WSL의 `CLOCK_MONOTONIC`은 **15.000229초**, Windows의 실제 시계는 **13.831800초**, WSL의 `CLOCK_MONOTONIC_RAW`는 **13.831682초**였다. 원본은 `clock-comparison.json`이다. WSL 쪽 15초만 믿으면 실제 기다린 시간을 부풀릴 수 있었다.

검사 스크립트의 Linux 경과 시간을 **보정 전 시계인 `CLOCK_MONOTONIC_RAW`**로 바꿨다. 단순히 시작·종료 시간만 바꾸면 `sleep`과 스레드의 `Event.wait`가 여전히 먼저 깨어날 수 있다. 그래서 `pause`는 RAW 시계의 목표 시각까지 남은 시간을 다시 계산해 기다린다. 호스트 시계·시간 동기화 설정이나 Shearwater 런타임을 바꾼 것은 아니다.

Windows의 장시간 검사는 Windows `monotonic`으로 실제 시간을 잰다. 다만 이 Python에서 `time.get_clock_info`로 확인한 구현은 `GetTickCount64`, 정밀도는 15.625ms였다. 긴 시간 측정에는 충분하지만 수 ms의 응답을 0ms로 표시할 수 있다. 따라서 장시간 Windows 자료의 지연 값으로 미세한 성능을 주장하지 않는다. 이후 실행용 스크립트는 Windows에서 0.1µs 해상도의 `QueryPerformanceCounter`를 쓰는 `perf_counter`로 보완했다. 본문의 응답속도 수치는 RAW 시계로 잰 Service 요청 결과다.

수정 후 15초 예비 검사에서 **498건**, 오류 **0**을 확인했다. 이어 20:12 KST경 RAW 기준 45분 검사를 시작했다. 기존 부하를 계속 유지한 채 겹쳐 실행하여 실제 전체 부하 시간이 60분을 넘도록 했다. 따라서 이 기록은 서로 독립된 두 번의 60분 통과가 아니다. 겹친 구간의 서버 자원 수치는 두 생성기의 **합산 부하**에 대한 관측이다. 최초 검사 자료와 그때 실행한 소스도 남겨 시간 문제를 숨기지 않았다.

## 관측 결과

짧은 예비 검사: 15초 부하 + 5초 휴지, 정상 응답 **489건**, 의도적 취소 **15건**, 오류 **0**, Pod·Service 상태 유지. 이 결과를 장시간 검사의 결과로 대신하지 않는다.

최초 Service 생성기는 **118,108건**, 의도적 취소 **3,585건**, 오류 **0**으로 종료했다. 이것의 `load_elapsed_s: 3600.0096`은 보정 전 계측값이므로 실제 60분이라는 근거로 사용하지 않는다. Windows 검사는 실제 **3,600.078초**, **3,597건**, 오류 **0**, HTML SHA-256 유지로 종료했다.

보정 후 Service 검사는 RAW 기준 **2,700.005초(45분)**, **89,925건**, 의도적 취소 **2,699건**, 오류 **0**으로 종료했다. 함께 실행한 Windows 검사는 **2,700.172초**, **2,698건**, 오류 **0**이었다. 두 Windows 검사의 시작 시각은 각각 19:48:31.891 / 20:12:03.939 KST다. 서로 겹친 구간을 한 번만 세면 **4,112.221초 = 68분 32초**다. 최초 부하 생성기가 끝나기 전 추가 생성기가 이미 실행 중이었으므로 중간에 부하를 끊지 않았다.

| 항목 | 최종 관측 |
|---|---|
| Service 정상 검사 | **208,033건 / 예상 밖 오류 0**. 예상한 200·206·304·404를 포함 |
| Windows 실제 주소 | **6,295건 / 오류 0**, 모든 HTML 해시 동일 |
| 의도적 TCP 연결 취소 | **6,284회**, 취소용 연결 자체의 실패 0. 정상 HTTP 수에 포함하지 않음 |
| 정확한 응답 시간 | RAW 기준 89,925건에서 중앙값 **0.896ms**, p95 **1.705ms**, p99 **2.168ms**, 최대 **43.484ms** |
| sw RSS | 처음 **34.14MiB**, 10초 간격 표본 최대 **36.76MiB**, 60초 휴지 뒤 **36.18MiB** |
| sw CPU | 보정 후 45분과 휴지 60초를 포함해 코어 하나 기준 평균 **6.15%**. 겹친 생성기의 요청도 포함 |
| 열린 FD | 처음 **25**, 부하 중 표본 최대 **78**, 60초 휴지 직후 **46**, 부하 종료 약 2분 17초 뒤 **22** |
| 소켓·파이프 정리 | 부하 중 표본은 소켓 **17**·파이프 **34**. 마지막 유휴 표본은 소켓 **3**·파이프 **0** |
| 서버 디스크 쓰기 / 자체 로그 증가 | 측정 구간에서 각각 **0바이트 / 0바이트** |
| 앱 로그 증가 | **22,428,521바이트**. 요청·의도한 404에 따른 nginx 로그이며 서버 자체 쓰기와 구분 |
| 상태 유지 | 같은 sw 시작 tick, 같은 Pod UID·IP·컨테이너 ID, 같은 Service. 부하 중 추가 재시작 **0** |
| 최종 운영 점검 | `sw doctor` 모든 검사 OK, `sw tree` Pod **1/1**, Endpoint **1**, 누적 재시작 **2**, Windows HTTP **200**, 중계 서비스 active |

FD는 TCP 연결 수와 같은 뜻이 아니다. 60초 시점의 숫자만 보고 누수라고 판단하지 않고 파일·소켓·파이프 종류를 더 관찰했다. 실제로 파이프가 0개가 되고 전체 FD가 시작보다 낮아졌다. RSS가 시작 수치와 완전히 같아지는 것을 요구하지는 않았다. 이번 부하에서 약 36MiB 범위에 머물렀다는 관측이며 무한 기간의 무누수 증명은 아니다.

CPU 100%는 코어 **하나**를 계속 쓰는 양이다. PC 전체의 CPU 백분율과 직접 비교하지 않는다. RSS는 `sw` 프로세스 수치이며 nginx worker 전체나 WSL VM 전체의 메모리는 아니다. 마지막 `curl` 진단과 짧은 예비 검사의 요청은 위 장시간 요청 합계에 더하지 않았다.

Windows 정밀도 보완 후 별도 10초 검사도 **10건 / 오류 0**으로 통과했다. 이 짧은 검사는 새로운 시계의 사용과 요청 동작을 확인한 것으로, 장시간 성능 수치를 대신하지 않는다.

## 증거 파일

| 경로 | 내용 |
|---|---|
| `/var/lib/shearwater/check/reboot-20261112-01/` | 배포판 재시작 전·직후·복구 후 JSON, SQLite 사본 |
| `/var/lib/shearwater/check/reboot-20261112-02/` | VM 전체 재시작 전·직후·복구 후 JSON, SQLite 사본 |
| `/var/lib/shearwater/check/soak-preflight-20261112/` | 15초 예비 검사 |
| `/var/lib/shearwater/check/soak-20261112-60min/` | `baseline.json`, 10초 간격 `metrics.jsonl`, 분 단위 `progress.json`, 최종 `result.json` |
| `/var/lib/shearwater/check/soak-raw-preflight-20261112/` | 시계 보정 뒤 15초 예비 검사 |
| `/var/lib/shearwater/check/soak-20261112-raw45min/` | RAW 기준 45분 추가 검사, 실행한 스크립트 사본 |
| `<private-evidence>\shearwater-reboot-soak-20261112\` | Windows의 재부팅 명령·시각·종료 코드와 실행 스크립트 |
| 위 폴더의 `windows-soak-60min/` | Windows 경로 분 단위 진행 기록, 요청별 응답과 최종 결과 |
| 위 폴더의 `windows-soak-raw45min/` | 추가 45분 Windows 검사 |
| 위 폴더의 `clock-comparison.json`, `raw-protocol.json` | 시계 비교 원본, 추가 실행 시각·소스 해시·경로 |
| 위 폴더의 `final-summary.json`, `resource-summary.json`, `reboot-summary.json` | 완료 결과 집계, 구간별 자원 표본, 재부팅 증거 재검사 |
| 위 폴더의 `doctor-final.txt`, `tree-final.txt` | 부하 후 실제 운영 상태 |

`progress.json`은 최신 진행 상태 하나만 담는다. 최종 통과 여부는 `result.json`으로 판단한다. SQLite 사본에는 앱 리소스 정보가 있으므로 공개 저장소에 올리지 않는다. 모든 검사 자료는 그대로 보존한다.

실행 바이너리의 SHA-256은 `f2a58fb94532f1198ecb423d91c98236bbe6f7ba47e5a6aec46b0ae91d75d7e7`이며, `binary-build-info.txt`에 Go 1.27.3·`CGO_ENABLED=0`을 포함한 실제 빌드 정보를 남겼다. 이번 변경은 검사 도구와 문서다. Go/M0/M1 회귀는 앞선 [배포 QA](deployment-qa.md)의 결과를 참조한다. 이번에는 새 Python 도구의 문법 검사, 실제 재부팅 비교, 예비 부하, 장시간 검사, 정밀도 보완 후 10초 검사를 실행했다.

## 다시 실행하는 방법

현재 스크립트는 **이번 검증용 웹 앱 배포를 위한 검사 도구**다. 다른 앱에 그대로 적용하는 범용 벤치마크가 아니다. `local-apps/sample-web`, 배포 경로 `/opt/shearwater/deployments/sample-web/18e6512/site`, Service `10.96.0.1`, Windows 포트 `18080`을 확인한다. 새 폴더 이름을 사용해야 하며, 기존 결과는 덮어쓰지 않는다.

PowerShell 창 두 개에서 다음 명령을 각각 실행한다. 첫 창은 WSL 계측에 필요한 root로, 둘째 창은 Windows 일반 사용자로 실행한다.

```powershell
wsl.exe -d Ubuntu-24.04 -u root -- python3 -B -I $SOURCE_DIR/hack/soak-check.py host --seconds 3600 --cooldown 60 --output /var/lib/shearwater/check/soak-NEW-RUN
```

```powershell
python -X utf8 -B -I <workspace>/shearwater\hack\soak-check.py probe --seconds 3600 --output <private-evidence>\shearwater-reboot-soak-NEW-RUN
```

WSL 재시작 검사는 앱 연결을 끊는다. 먼저 `wsl --list --running --quiet`와 실행 작업을 확인하고, 중단 가능한 때에만 실행한다. `wsl --shutdown`은 다른 실행 중인 배포판에도 영향을 주므로 다른 배포판이 있으면 이 예시를 그대로 진행하지 않는다.

```powershell
# 기존 증거와 겹치지 않는 폴더를 지정한다.
wsl.exe -d Ubuntu-24.04 -u root -- python3 -B -I $SOURCE_DIR/hack/reboot-snapshot.py before /var/lib/shearwater/check/reboot-NEW-RUN
wsl.exe --shutdown
wsl.exe -d Ubuntu-24.04 -u root -- python3 -B -I $SOURCE_DIR/hack/reboot-snapshot.py booted /var/lib/shearwater/check/reboot-NEW-RUN
wsl.exe -d Ubuntu-24.04 -u root -- $HOME/.cache/shearwater-dev/sample-web-build.oGBdyL5b/sw up
wsl.exe -d Ubuntu-24.04 -u root -- $HOME/.local/bin/kubectl --kubeconfig=/etc/shearwater/kubeconfig --cache-dir=/var/lib/shearwater/check/reboot-NEW-RUN/discovery -n local-apps wait --for=condition=Ready pod -l app=sample-web --timeout=60s
wsl.exe -d Ubuntu-24.04 -u root -- python3 -B -I $SOURCE_DIR/hack/reboot-snapshot.py after /var/lib/shearwater/check/reboot-NEW-RUN
wsl.exe -d Ubuntu-24.04 -- systemd-run --user --collect --unit=shearwater-sample-web /usr/bin/python3 /opt/shearwater/deployments/sample-web/234d390/localhost_relay.py 10.96.0.1 80 18080
curl.exe --fail --silent --output NUL --write-out "%{http_code}" http://127.0.0.1:18080/
```

`sw up` 직후에 Ready를 기다리는 이유는 API가 준비된 시점과 Pod가 다시 실행된 시점이 다르기 때문이다. 중계 서비스가 이미 active면 `systemd-run`을 반복하지 않아도 된다. 이 절차는 데이터를 지우거나 배포 매니페스트를 다시 적용하지 않는다.

## 해석의 한계

현재 구성은 단일 노드·단일 정적 웹 Pod다. 정해진 60분과 요청 속도에서의 관측이며, 며칠·수개월 동안의 무누수 증명이나 DB 앱의 내구성 검사, 인터넷 공개 배포의 가용성 검사가 아니다. 실제 Windows 재부팅, 자동 시작 설치, 대규모 Pod 수, 네트워크 단절, 디스크 가득 참은 이번 부하의 범위가 아니다. 기존 M2–M6 통합 스크립트는 빈 런타임을 전제로 하므로 실제 앱이 실행 중인 이 서버에 그대로 실행하지 않는다.
