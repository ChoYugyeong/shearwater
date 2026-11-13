# Shearwater

Shearwater는 작은 컨테이너 서비스를 운영하는 단일 바이너리 `sw`야. `kubectl`로 Pod, Deployment, Service를 다루고, containerd가 실행을 맡고, SQLite가 상태를 보관해. 필요한 API·컨트롤러·네트워크 부분을 직접 구현하고, `sw`는 Kubernetes Go 패키지(`k8s.io/*`)를 import하거나 바이너리에 포함하지 않아. containerd 네이티브 클라이언트를 사용하며 CRI·CNI 플러그인을 사용하지 않아. 참고·적응한 프로토콜과 패치 구현의 출처는 NOTICE에 적었어.

**현재 단계는 실험적 알파**야. 실제로 검증한 운영 환경은 **WSL 2의 Ubuntu 24.04, Linux amd64, 단일 노드, IPv4, TCP ClusterIP**야. 관리자 한 명이 신뢰하는 앱을 실험하는 용도이며, Kubernetes 인증 제품이나 운영 서비스용 보안 격리 도구가 아니야. 특히 **프로브와 securityContext는 저장만 하고 실행에 반영하지 않아.** Ready는 앱의 정상 응답 여부가 아니라 컨테이너 실행 여부를 뜻해. [지원 범위](docs/compatibility.md)를 먼저 확인해.

Shearwater is an experimental single-node container orchestrator with a small Kubernetes-compatible API, native containerd integration, and SQLite storage. The tested runtime target is WSL 2 / Ubuntu 24.04 / amd64. See the Korean [quickstart](docs/quickstart.md) and [compatibility table](docs/compatibility.md) for the exact limits.

## 처음 시작하기

Go 없이 시작하려면 [알파 실행 파일 설치 안내](docs/install.md)를 따라 해.
현재 릴리스는 **v0.1.0-alpha.3**이며, `sw version`도 `0.1.0-alpha.3`를 표시해.
[릴리스 변경 사항과 버전 검증 방법](docs/releases.md)도 확인할 수 있어.

alpha.3부터 `sw get/logs/events/why`로 목록·앱 로그·이벤트·실패 이유를 조회할 수 있어.
[초보자용 문제 해결 안내](docs/troubleshooting.md)에 각 명령의 예제와 오류 해석을 정리했어.

처음이라면 **[설치부터 HTTP 응답까지 따라 하기](docs/quickstart.md)**를 사용해. Go 1.27.3과 kubectl 1.36.6, containerd·runc·iptables, cgroup v2와 IPv4 전달이 준비된 WSL에서는 다음 순서로 실행해. 기존 Shearwater 인스턴스가 없는 환경을 사용해.

```bash
. hack/env.sh
bash hack/build.sh
sudo "$PWD/dist/sw" up
sudo "$(command -v kubectl)" --kubeconfig /etc/shearwater/kubeconfig get nodes
sudo "$(command -v kubectl)" --kubeconfig /etc/shearwater/kubeconfig apply -f examples/web.yaml
sudo "$(command -v kubectl)" --kubeconfig /etc/shearwater/kubeconfig rollout status deploy/web --timeout=120s
```

예제는 저장소 안의 `examples/web.yaml`만으로 실행돼. 사용자 PC의 별도 앱이나 파일은 필요 없어. Service IP를 조회해 WSL에서 `curl`로 접속하고 `kubectl logs deploy/web`으로 기록을 보는 단계는 quickstart에 이어져 있어. Service DNS와 `kubectl port-forward`는 구현되지 않았어.

| 하고 싶은 일 | 명령 |
|---|---|
| 서버와 노드가 정상인지 보기 | `sw status` |
| Deployment가 만든 Pod와 Service 보기 | `sw tree` |
| 문제가 있는 가지에 집중하기 | `sw tree --problems` |
| containerd·네트워크·로그 확인하기 | `sw doctor` |
| 객체 목록·상세 보기 | `sw get pods -A` / `sw get pod/NAME --full` |
| 현재·직전 앱 로그 읽기 | `sw logs pod/NAME` / `sw logs pod/NAME --previous` |
| 현재 객체의 이벤트 읽기 | `sw events --for=pod/NAME` |
| Pod의 실패 이유와 다음 확인 보기 | `sw why pod/NAME` |
| 서버를 잠시 멈추기 | `sw down` |
| Pod와 전용 네트워크도 내리기 | `sw down --all` |

기본 경로의 인증 정보는 root 소유야. 표의 `sw`를 빌드한 파일 경로로 바꾸고 시작한 사용자로 실행해. [운영 안내서](docs/operations.md)에 명령별 예제와 복구 순서가 있고, [Service 안내서](docs/service-networking.md)에 연결 구조가 있어. SQLite는 `/mnt/c`가 아닌 WSL의 Linux 파일시스템에 둬. WSL 배포판을 여러 개 만들더라도 네트워크가 공유될 수 있으므로 기본 설정의 Shearwater를 동시에 띄우지 마.

## 개발·검증·기여

```bash
. hack/env.sh
bash hack/t.sh ./...       # 소스 수정 없이 포맷·정적 검사·Go 테스트
bash hack/build.sh         # dist/sw와 SHA256, 포함 모듈 정보 출력
```

[기여 방법](CONTRIBUTING.md) · [보안 범위와 제보](SECURITY.md) · [공개 알파 검사 절차](docs/alpha-release.md) · [실제 재부팅·부하 기록](docs/reboot-soak.md)

GitHub Actions 설정은 root가 필요 없는 소스 검사를 수행해. 실제 컨테이너·네트워크·재부팅 검사는 별도 환경에서 수행하며 CI 배지가 대신 보증하지 않아. 테스트 결과에는 실행한 환경·시간·실패 수와 생략한 항목을 함께 남겨.

`go list -m all`에는 상위 containerd 모듈의 CRI 서버용 Kubernetes 의존성이 나타날 수 있어. 그것이 `sw`에 링크됐다는 뜻은 아니야. `go list -deps ./cmd/sw`와 `go version -m dist/sw`로 실제 import·빌드 범위를 확인해. 이번 Linux 바이너리의 외부 모듈은 46개이고 `k8s.io/*`는 0개야.

## 라이선스

Shearwater 원본 코드는 [Apache License 2.0](LICENSE)이야. 저작권은 The Shearwater Authors로 표기해. 참고·수정한 코드와 라이브러리의 출처는 [NOTICE](NOTICE), [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)에 따로 기록해. containerd·runc·kubectl·컨테이너 이미지는 별도 배포물이며 각각의 라이선스를 따라.

## 실제 리소스 사용량

WSL Ubuntu-24.04, Linux 6.6.87.2, Go 1.27.3에서 2026-11-11에 측정했어. BusyBox Pod 10개가 Ready가 된 뒤 API 요청 없이 60.003초 동안 `sw server`를 관찰했어. MB는 1,000,000바이트 기준이야.

| 항목 | 실측 | 설계 목표 |
|---|---:|---:|
| 유휴 디스크 쓰기 | **0 B**, DB·WAL·서버 로그 변경 알림 0건 | 쓰기 없음 |
| 유휴 CPU | **0.1167%** (논리 코어 하나 기준) | 0에 가깝게 |
| 최대 RSS | **34.76 MB** (34,762,752 B) | 50 MB 이하 |
| 바이너리 (기호 제거 전) | **35.66 MB** (35,664,141 B) | 40 MB 이하 |
| `sw up` 시작부터 HTTPS `/readyz` | **55.2 ms** | 1초 이하 |
| Shearwater가 Pod마다 추가한 태스크 | **0개** (Pod 10개, 앱 태스크 10개) | 0개 |

시작 시간은 이미 실행 중인 containerd 위에서 빈 설정·데이터 경로로 측정했어. 이미지 받는 시간과 WSL 부팅 시간은 포함하지 않았어. RSS와 CPU는 `sw server`만 측정했으므로 containerd, shim, 앱 컨테이너의 사용량은 별도야. 유휴 시작과 끝의 저장소 리비전은 모두 77이었어. 전체 측정값 원본은 저장소 밖 검증 기록에 보관했고, 재현 방법과 검사 범위는 [1단계 검증](docs/stage1-verification.md)에 있어.
