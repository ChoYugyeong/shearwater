# 공개 알파 준비와 검증 기록

> 이 문서는 2026-11-12에 공개 준비 과정에서 실행한 검사와 공개 절차를 구분해 기록한 거야. 공개 저장소 생성, push, GitHub Release 발행은 수행하지 않았어.

## 배포 범위

제품·저장소 이름은 **shearwater**, 자체 코드 라이선스는 **Apache-2.0**으로 정했어. 첫 공개 대상은 WSL 2 / Ubuntu 24.04 / Linux amd64에서 사용할 실험적 알파야. 지원·저장만 하는 필드·미지원 기능은 [호환표](compatibility.md)에 정리했어.

소스나 `sw` 바이너리를 배포할 때 `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`, `LICENSES/`를 함께 전달해. 호스트 도구와 nginx 이미지, 모듈 캐시, 운영 데이터베이스, 인증 자료, 비공개 원본 증거는 배포물에 포함하지 않아. 이미지 참조가 예제에 있다는 것과 이미지를 재배포한다는 것은 달라.

## 처음 쓰는 사람의 검사

[quickstart](quickstart.md)를 따라 빌드 → 시작 → 저장소의 `examples/web.yaml` 적용 → HTTP → 로그 → 서버 재시작 순서로 진행해. 자동으로 확인하려면 **비어 있는 전용 런타임**에서 다음을 사용해.

```bash
# 일반 사용자: 저장소 루트에서 빌드
. hack/env.sh
bash hack/build.sh

# 전용 검증 환경의 root에서만 실행
sudo bash hack/alpha-check.sh "$PWD/dist/sw" "$(command -v kubectl)"
```

스크립트는 `shearwater` 네임스페이스의 기존 컨테이너가 있으면 거절하고, 새 데이터·인증·로그 폴더와 별도 API 포트를 사용해. Pod HTTP, 현재·직전 로그, EndpointSlice, status/tree/doctor, 서버 재시작 인계, 컨테이너 강제 종료 후 자동 재시작을 확인해. 기본적으로 자신이 시작한 런타임은 `sw down --all`로 종료하고 증거 파일은 남겨. 다른 인스턴스의 데이터를 지우지는 않아.

별도 containerd 소켓을 사용할 때는 `sudo env SW_CONTAINERD_SOCKET=/실제/소켓 bash hack/alpha-check.sh ...`로 전달해. `SW_ALPHA_PORT`는 API 포트, `SW_ALPHA_KEEP=1`은 **성공한 검사 인스턴스만** 계속 띄워 두는 옵션이야. KEEP 상태의 정확한 경로는 출력된 `result.json`에서 확인해. 데이터·설정·런타임 경로가 서로 다르므로 일부만 기본값으로 돌려놓지 마.

## 새 WSL에서 실제로 확인한 것

기존 Ubuntu와 앱을 복제하지 않고 [Ubuntu 공식 WSL 이미지](https://releases.ubuntu.com/noble/)로 새 배포판을 import했어. 입력은 `ubuntu-24.04.5-wsl-amd64.wsl`, 388,975,696 B, SHA256 `bb415d824822c4b878125729af451a5d18fb13d1cf5cbed9a7393ad64ac6039e`였고 공식 SHA256SUMS와 일치했어. [WSL import 방식](https://learn.microsoft.com/en-us/windows/wsl/use-custom-distro)을 사용했고, 원래 배포판은 export하지 않았어.

새 환경은 Ubuntu 24.04.5, Go 1.27.3, kubectl v1.36.6, containerd 2.2.1, runc 1.3.4, iptables 1.8.10, Python 3.12.3이었어. Go 의존성·이미지·SQLite는 새 환경에서 다시 준비했어.

**이 호스트에서는 배포판끼리 네트워크가 공유됐어.** 그래서 새 배포판의 파일시스템만 분리한 뒤 바로 기본 sw를 실행하지 않았어. 검증 전용 네트워크 네임스페이스와 연결용 veth, 그 veth의 주소에만 적용되는 전달/NAT 규칙을 만들었어. sw의 `sw-br0`, `sw-svc0`, Pod·Service 주소는 그 내부에 있었어. containerd도 전용 소켓·root·state와 `/dev/null` 설정 입력으로 실행했으므로 기존 Docker/containerd 설정을 읽거나 바꾸지 않았어.

처음 `ip netns exec` 방식은 이 환경에서 cgroup 마운트를 가려 `sw up`의 사전 검사가 실패했어. cgroup v2 마운트를 유지하고 네트워크 공간만 전환하는 `nsenter --net`으로 검사 준비를 고쳤어. 이를 sw의 일반 실행 성공으로 숨기지 않고, 격리된 QA 환경의 준비 오류로 구분했어. 첫 버전 검사 스크립트가 status/tree에 런타임 전용 옵션을 넘긴 문제도 수정하고 재실행했어.

전체 수명주기 검사도 격리 환경에 맞춰 고쳤어. `e2e-stage1.sh`와 `m2-check.sh`가 `SW_CONTAINERD_SOCKET`을 받도록 하고, cold-start에서도 같은 소켓을 유지하게 했어. `/sys/class/net`은 sysfs를 마운트한 바깥 네트워크를 계속 보여 줄 수 있으므로 장치 확인은 현재 공간의 netlink를 사용하는 `ip`로 바꿨어. 첫 통합 검사의 16번 실패는 이 관찰 대상 오류였고, 그 실행의 전체 성공을 주장하지 않아.

| 검사 | 실제 결과 |
|---|---|
| 새 환경의 `bash hack/t.sh ./...`와 `hack/build.sh` | 통과. cgo 없는 Linux 바이너리 빌드 |
| `hack/alpha-check.sh` | HTTP·로그·EndpointSlice·상태/진단·재시작 관련 10개 항목 통과 |
| 기존 호환 회귀 | M1 **103 passed / 0 failed**, M0 **47 passed / 0 failed** |
| Python 도구 단위 검사 | Linux **33개 통과**. Windows **32개 통과**, 심볼릭 링크 권한이 필요한 1개 생략 |
| 새 예제의 짧은 부하 회귀 | 실제 부하 60.382초, 요청 **1,998건**, 의도적 연결 종료 **60건**, 예상 밖 오류 **0** |
| HTTP 종류 | 200 **800**, 206 **400**, 304 **399**, 의도한 404 **399**; HEAD·GET·Range·ETag·404 전부 수행 |
| 이 부하 중 지연 | p95 **1.765 ms**, p99 **2.364 ms**. 이 환경/부하의 값이며 처리량 한계 측정은 아님 |
| 이 부하 중 sw RSS | 최대 **32,608 KiB**. 앱·containerd·부하 생성기는 제외 |
| 새 배포판만 실제 `wsl --terminate` 후 복구 | before/booted/after 모두 통과. Pod UID·IP, Deployment/Service UID, ClusterIP, 인증 해시, SQLite 무결성 유지 |
| 커널/배포판 구분 | 커널 boot ID는 같고 배포판 init 시작 tick은 달라짐. 전체 WSL VM 재부팅으로 표현하지 않음 |
| 기존 실제 앱 보존 | 새 환경 준비·검사·재시작 뒤 원래 앱 HTTP 200 확인 |

새 예제는 별도 JS/CSS가 없는 615바이트 기본 nginx 페이지였어. 이전 실제 앱의 68분 32초 측정과 이번 60초 검사 결과는 다른 실험이므로 합산하지 않았어. 장시간 검증은 [이전 기록](reboot-soak.md)을 봐.

Python 검사는 가짜 API·로컬 HTTP 서버로 오류 응답·지원하지 않는 기능·기존 출력 보존·대상 Pod 범위·재부팅 비교를 확인해. `python -m unittest discover`에서 증거 경로 초기화가 빠진 결함을 발견해 고친 뒤 Linux와 Windows에서 같은 discovery 명령을 다시 통과시켰어. 가짜 재부팅 단위 검사와 위의 실제 WSL 종료 실험은 별개의 증거야.

## 공개 준비 중 찾은 런타임 결함

초기 smoke의 기능 검사는 통과했지만, 즉시 `sw down --all`로 정리하는 추가 실측에서 이전 실행 시도와 새 실행 시도가 같은 cgroup 경로를 쓰는 결함을 찾았어. 종료된 시도 0의 cgroup을 runc가 지우려는데 시도 1이 그 안에서 실행 중이어서 `device or resource busy`가 났고 전체 정리가 중단됐어. 이 실행은 전체 통과가 아니야.

새 시도는 `<컨테이너>_<시도 번호>`라는 서로 다른 leaf cgroup을 사용하도록 고쳤어. 독립 리뷰에서 구형 컨테이너 이름 `web-1`과 새 `web`의 시도 1이 겹치지 않도록 이름에 금지된 `_` 구분자를 사용하라는 문제를 확인해 반영했어. 구버전 태스크를 정리하는 경로에는 실행 중인·최신 시도를 먼저 제거하는 순서를 적용했어. 가짜 런타임에도 공유 cgroup의 busy 조건을 넣고 새 경로에서의 재시작, 다중 컨테이너의 구형 이름 충돌, 구버전 정리를 회귀 검사로 추가했어. 실패 상태를 그대로 남긴 실제 구버전 fixture는 수정 바이너리의 `sw down --all`로 정상 정리되는 것을 확인했어.

검사 도구도 수정했어. 기능 검사만 마치고 `passed:true`/`PASS`를 출력하던 위치를 정리 뒤로 옮겼고, `functional.json`의 `checksPassed`와 최종 `result.json`의 `passed`·`cleanup`·`exitCode`를 구분했어. `KEEP=0`에서는 정리까지 성공해야 전체 성공이야. 기존 네트워크가 있는데 다른 containerd만 비어 있는 경우를 거절하는 사전 검사도 실제로 확인했어.

`_` 구분자까지 반영한 첫 수정본은 새 WSL의 일반 사용자로 소스 아카이브를 풀어 `go mod verify`, `hack/t.sh ./...`, 빌드, 고지 검사, Python 33개 검사를 다시 통과했어. 이 소스 아카이브 빌드의 SHA256은 `8dce069735d9d3c88f6203613c67ff0307f2c34299dad9a27cf090a62a1b3071`이야. 고지 검사는 원본 모듈 캐시의 라이선스를 읽거나 추가로 내려받지 않고 동봉 원문 37개·검토 문서 68개·실제 링크 모듈 46개의 버전/체크섬을 대조했어.

같은 바이너리의 M2 실환경 검사는 Pod 간 ICMP, 외부 ICMP·TCP, hostPath 생성/마운트, 컨테이너 강제 종료 뒤 같은 Pod IP로 재시작, 서버 재시작 인계, 서버 중단 중 종료된 태스크의 복구, 유예 삭제와 IP 반환까지 통과했어. 위 표의 첫 60초 부하 수치는 이 수정 전 실험의 값이므로 최종 수정본의 성능 측정값으로 대체해서 읽으면 안 돼.

이 수정본으로 전체 수명주기 검사와 `alpha-check.sh`의 즉시 정리까지 통과한 뒤 실제 배포판 종료를 반복했어. UID/IP 복구는 통과했지만, **배포판만 종료하면 살아 있는 WSL 커널에 이전 시도의 빈 cgroup이 남는** 두 번째 경우를 찾았어. 재기동한 containerd에는 그 태스크가 없어서 runc의 정리 대상에도 포함되지 않았고, 빈 자식 디렉터리가 Pod 부모의 제거를 막았어.

`PodGone`은 검증된 해당 Pod 경로 바로 아래의 빈 cgroup부터 `rmdir`로 제거하도록 보완했어. 파일·심볼릭 링크를 지우거나 하위 경로를 재귀 탐색하지 않아. 프로세스가 있거나 더 아래에 자식 cgroup이 있으면 커널이 거절하며, 정리를 위해 프로세스를 죽이거나 옮기지 않아. 단위 검사에는 다른 Pod 보존, 반복 정리, 비어 있지 않은 경로·중첩 디렉터리·심볼릭 링크 보존, 잘못된 UID 거절을 넣었어. 실제 실패 상태를 그대로 둔 fixture도 수정본으로 정리되고 해당 Pod cgroup이 사라지는 것을 확인했어.

## 두 런타임 수정 뒤 최종 확인

최종 실측 파일은 새 WSL에서 소스 아카이브로 빌드한 Linux amd64 바이너리이며, SHA256은 `b99450aee8bfa9cae9185f6249cb6ba3f572ea09ae5ec4971ec148743ea6f564`야. 새 환경의 전체 Go 검사·모듈 검증·고지 검사를 통과했고, 테스트 이름을 명확히 다듬은 뒤에도 전체 Go 검사를 다시 통과했어.

| 최종 수정본 검사 | 결과 |
|---|---|
| `e2e-stage1.sh` | 범위 안의 전체 수명주기 검사, SIGKILL 인계, cold-start, 최종 `down --all` 통과 |
| `alpha-check.sh`, KEEP=0 | 10개 기능 검사와 즉시 정리 통과, `passed=true`, `cleanup=stopped`, 종료 코드 0 |
| 별도 KEEP=1 fixture | 같은 기능 검사 후 실제 배포판 종료/복구 검사에 사용 |
| 최종 부하 회귀 | 실제 부하 **60.377초**, 관찰 10초 추가, 요청 **1,998건**, 의도적 연결 종료 **60건**, 예상 밖 오류 **0** |
| 최종 HTTP 응답 | 200 **800**, 206 **400**, 304 **399**, 의도한 404 **399** |
| 최종 지연·RSS | p95 **1.859 ms**, p99 **2.379 ms**, sw 최대 **33,896 KiB** |
| 실제 populated cgroup 보호 | 실행 중인 이 QA Pod의 cgroup에 `rmdir`를 시도하면 **EBUSY**로 거절되고 프로세스가 유지됨 |
| 실제 QA 배포판 재시작 | before/booted/after 모두 통과. 같은 UID·IP·ClusterIP·인증 해시·SQLite 무결성 복구 |
| 재부팅 후 마지막 정리 | `doctor` 정상, `down --all` 성공, 컨테이너·태스크 없음, Pod cgroup·전용 장치 제거 확인 |
| QA 호스트 정리 | 전용 containerd 종료, QA netns·veth·정확한 전달/NAT 규칙 제거. VHDX와 증거는 보존 |

`e2e-stage1.sh`의 16번에서 금지된 다른 containerd 네임스페이스 비교는 생략했고, 17번의 별도 `measure.sh`는 이번 실행에서 생략했어. 리소스 숫자는 이 표의 짧은 부하 관찰과 README의 이전 유휴 측정으로 구분해. 최종 부하의 CPU는 관찰 시간을 포함해 논리 코어 하나의 **2.64%**, sw 프로세스의 추가 디스크 쓰기·서버 로그 증가는 **0 B**였어. 앱 로그 쓰기는 별도이며, 60초 검사만으로 장기 누수나 최대 처리량을 보증하지 않아.

위의 일반 임시 디렉터리 단위 검사가 cgroup 커널의 동작 자체를 증명하지는 않아. 실제 빈 orphan 제거와 populated 거절은 각각 별도 실측으로 확인했어. 별도 코덱스 리뷰도 이 구분과 정리의 비재귀 범위를 확인했어.

## 부하·재부팅 도구 재사용

`hack/soak-check.py host`는 Service selector에 맞는 모든 Pod를 비교하고, 다른 Pod는 변경 여부만 기록해. Service IP와 선택 Pod를 API에서 찾으므로 특정 사용자 이름·앱·고정 ClusterIP에 의존하지 않아. 첫 정상 HTML 바이트를 기준으로 삼고, 정확히 기대하는 파일이 있으면 `--expected-file`을 줄 수 있어. 외부 자산은 자동으로 내려받지 않아.

```bash
python3 hack/soak-check.py host --help
python3 hack/reboot-snapshot.py --help

# 일반적인 기본 경로 인스턴스의 예. 출력 폴더는 새 이름이어야 해.
sudo python3 hack/soak-check.py host --seconds 60 --cooldown 10 \
  --kubectl "$(command -v kubectl)" --namespace default --service web \
  --output /var/lib/shearwater/check/web-soak-01
```

HEAD·Range·ETag·404가 지원되지 않으면 자동으로 빼고 성공 처리하지 않아. 해당 기능이 없는 서버를 의도적으로 검사할 때만 `--skip-check 이름`을 반복해서 지정해. 결과에 생략한 검사가 남아. 별도 인스턴스에는 `--data-dir`, `--config-dir`, `--run-dir`, 필요하면 `--containerd-socket`과 `--output-root`를 실제 값으로 모두 지정해.

재부팅 도구는 **재부팅하거나 서버를 시작하지 않는 관찰 도구**야. 같은 증거 폴더와 같은 인자로 `before` → 운영자가 해당 환경 종료/기동 → `booted` → 운영자가 containerd와 sw 복구 → `after` 순서로 실행해. `booted`는 `/run`이 다시 생성되기 전에 검사해야 해. 결과 JSON의 `passed: true`와 프로그램 종료 코드 0을 함께 확인해. DB의 읽기 전용 백업과 인증 파일 해시를 남기므로 원본 증거 디렉터리는 공개하지 마.

Linux에서는 `CLOCK_MONOTONIC_RAW`, Windows 외부 HTTP 관찰에서는 `perf_counter`를 사용해. 이전 WSL 시간 보정 문제와 Windows 시계 해상도 차이를 반영한 선택이야. 모든 결과에는 실제 경과 시간과 부하 종료 뒤 관찰 시간을 따로 기록해.

## 소스·고지·CI 확인과 공개 직전 단계

Gitleaks 8.30.1의 공식 바이너리 체크섬을 확인한 뒤 `--log-opts=--all --redact=100`으로 당시 Git 이력 **57개 커밋**을 검사했고 탐지된 비밀정보는 없었어. 규칙 기반 검사 결과이지 비밀정보 부재의 수학적 보증은 아니야. 현재 문서의 개인 경로·내부 라우트·기존 앱 이름은 일반화했으며, 원문은 저장소 밖에 보존했어. **이전 Git 커밋의 일반 경로와 저자 메타데이터는 이력을 다시 쓰지 않았으므로 남아 있어.**

GitHub Actions는 소스 검사·Linux API 회귀·이력 비밀정보 검사로 나눴고 액션은 확인한 커밋 SHA에 고정했어. CI에 대응하는 명령을 로컬에서 실행하더라도 GitHub에서 워크플로가 실제로 실행됐다는 뜻은 아니야. root 런타임·실제 WSL 재시작은 이 워크플로에 포함하지 않아.

전체 모듈 그래프에는 상위 containerd가 선언한 Kubernetes/CRI 서버 의존성이 나타났지만, `go list -deps ./cmd/sw`와 실제 바이너리에는 `k8s.io/*` 패키지/모듈이 없었어. CI도 실제 import와 링크 결과를 검사하도록 고쳤어. Go 모듈 메타데이터, go.sum 목록, 실행 파일의 46개 모듈을 같은 숫자로 소개하지 않아.

공개 실행 단계에서는 저장소 소유자/주소를 확정하고 비공개 취약점 제보를 활성화한 뒤, 공개할 커밋과 배포물·체크섬을 검토해야 해. 과거 이력의 일반 경로·저자 정보까지 공개하는지 또는 새 소스 스냅샷으로 시작할지도 선택해야 해. 이력을 강제로 다시 쓰거나 push하는 작업은 이번 준비 작업에 포함하지 않았어.

라이선스·출처는 별도 코덱스 에이전트가 검토했고, 가변 경로 검사 도구도 별도 에이전트가 구현·검사했어. 이번 단계의 딥시크 검토는 2026-11-13에 받았고 결과는 [검토 기록](meetings/2026-11-13-alpha-release-deepseek.md)에 있어. 판정은 CONCERNS이고, 그 세션에서는 Go 검사·gitleaks 재실행·GitHub Actions 실행을 하지 못했어. 이전 마일스톤의 검토 의견을 이번 변경의 승인으로 간주하지 않아.
