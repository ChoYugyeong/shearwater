# Shearwater

[English](README.md) · **한국어**

*AI 에이전트에게 서버 배포를 맡겨도 돼요. 단, 승인한 변경만, 딱 한 번, 실제로 응답할 때까지는 "완료"가 아니에요.*

이제 AI 에이전트가 MCP로 서버를 바꿀 수 있어요. Shearwater는 에이전트와 Linux 서버 한 대 사이에 들어가요. 에이전트는 맡긴 앱 하나만 건드릴 수 있고, 사람이 정확한 계획을 승인해요. 그 계획은 모델이 바뀌거나 서버가 재시작돼도 딱 한 번만 실행돼요. 그리고 새 사본 하나하나가 기대한 응답을 HTTP로 돌려주기 전에는 에이전트가 성공을 보고할 수 없어요.

**이미 쓰고 있는 Docker Compose 프로젝트** 앞에 붙일 수 있어요(root 불필요, compose 파일은 고치지 않아요). Kubernetes API 일부를 직접 구현한 **자체 런타임**으로도 돌릴 수 있어요.

[Docker Compose로 시작하기](#docker-compose로-시작하기) · [자체 런타임으로 시작하기](#자체-런타임으로-시작하기) · [측정 결과](#측정-결과)

<p align="center">
  <img src="docs/media/broken-update-caught.gif" width="576" alt="에이전트의 v2 업데이트가 승인을 기다렸다가 승인 뒤 검증을 통과해요. 다음 이미지는 HTTP 200을 돌려주지만 기대한 문구가 없어서 서버가 healthy 대신 http_failed로 보고하고, 복구는 마지막으로 검증된 이미지로 돌아가요.">
</p>

<details>
<summary>데모 내용을 글로 읽기</summary>

새 GitHub 호스팅 VM에서 [빠른 시작 검사](hack/compose-quickstart-check.py)가 아래 단계를 대본으로 짠 MCP 에이전트와 함께 따라간 기록이에요. 화면의 값은 모두 그 실행의 기록에서 가져왔어요. [워크플로](.github/workflows/compose-quickstart.yml)

1. 에이전트가 `web`을 v2로 업데이트해 달라고 해요. 서버는 계획을 저장하고 `awaiting_approval`을 돌려줘요. 아직 아무것도 안 바뀌어요.
2. 운영자가 `sw task approve`로 그 계획의 해시를 승인해요.
3. 에이전트가 `continue`를 불러요. v2가 적용되고 새 컨테이너가 모두 기대한 문구와 함께 HTTP 200을 돌려줘서 `healthy`예요.
4. 다음 이미지도 같은 방식으로 승인·적용돼요. HTTP 200은 돌려주지만 기대한 문구 대신 점검 페이지가 나와요.
5. 서버는 `healthy`가 아니라 `http_failed`로 보고해요. 에이전트는 이 배포를 완료라고 보고할 수 없어요.
6. `recover`가 마지막으로 통과한 이미지(v2)로 돌아가는 계획을 세워요. 승인 뒤 `healthy`로 검증돼요.

자체 런타임에서 승인 흐름을 재생한 이전 기록은 [에이전트 운영 검증](docs/agent-operations-verification.md)에 있어요.

</details>

### 다른 방법과 무엇이 다른가요?

- **플랫폼의 MCP 서버(Coolify, Dokploy)**: 에이전트가 팀의 여러 앱에 걸쳐 플랫폼 API를 부를 수 있어요. Shearwater는 에이전트를 기한이 있는 앱 하나로 묶고, 해시로 검토하는 계획에서 멈추고, 결과를 확인한 뒤에야 성공을 보고해요.
- **범용 MCP 승인 프록시**: 도구 호출 전에 사람에게 물어봐요. 그 뒤에 배포가 실제로 됐는지는 몰라요. Shearwater는 새 사본 하나하나를 HTTP로 확인하고 증거를 남겨요.
- **에이전트에게 `docker compose up`을 그냥 맡기기**: 에이전트가 Docker 소켓을 쥐게 되고, 남는 건 "아마 떠 있을 거예요"뿐이에요.

## Docker Compose로 시작하기

Docker와 `docker compose` 플러그인이 있는 Linux amd64 서버, 그리고 `docker` 그룹에 속한 운영자 계정이 필요해요. root는 필요 없고, Shearwater는 containerd나 compose 파일을 바꾸지 않아요. 이 단계는 바뀔 때마다 새 GitHub 호스팅 VM에서 공개 릴리스로 그대로 돌려 봐요. 설치는 약 2초, 위임 뒤 첫 승인 업데이트가 검증되기까지 약 3.5초 걸렸어요(3회 실행). [빠른 시작 검사](.github/workflows/compose-quickstart.yml)

**1. `sw` 설치.** 빈 임시 폴더에 설치 도우미를 받고 체크섬을 확인한 뒤, 이 버전을 전용 폴더에 설치해요.

```bash
(
  set -eu
  version=v0.1.0-alpha.16
  release="https://github.com/ChoYugyeong/shearwater/releases/download/$version"
  download_dir=$(mktemp -d)
  cd "$download_dir"
  curl -fsSLO "$release/install.py"
  curl -fsSLO "$release/SHA256SUMS"
  sha256sum --check --strict --ignore-missing SHA256SUMS
  python3 install.py --version "$version" --apply
  "$HOME/.local/share/shearwater/$version/sw" version
)
```

출력이 `sw 0.1.0-alpha.16`로 시작하면 돼요. 아래 명령은 `sw`가 `PATH`에 있다고 가정해요.

**2. Compose 프로젝트 하나 앞에 Shearwater 띄우기.** 계속 켜 두세요.

```bash
sw compose serve --file /srv/shop/compose.yaml --project shop --listen 127.0.0.1:16444
```

**3. 서비스 하나를 에이전트에게 맡기기.** 이 예시는 `web` 서비스의 업데이트와 복구를 허용하고, 응답에 `Welcome`이 들어 있어야 통과로 봐요. 에이전트는 다른 OS 계정에서 돌리는 걸 권장해서, 넘겨줄 폴더를 홈 밖에 만들어요.

```bash
sudo install -d -o "$USER" -m 0755 /srv/sw-handoff   # 처음 한 번만
sw task create --config-dir ~/.config/shearwater-compose --listen 127.0.0.1:16444 \
  -n shop --name web --port 80 --allow update,recover --expect-body "Welcome" \
  --out /srv/sw-handoff/web --ttl 2h
```

**4. 에이전트 연결.** 에이전트의 MCP 클라이언트에 이 명령을 등록해요. 에이전트는 이 서비스 하나에 묶인 토큰만 받고, Docker 소켓이나 운영자 토큰은 받지 않아요.

```bash
sw mcp --workflow=/srv/sw-handoff/web/work.json --allow-write
```

**5. 제안을 검토하고 승인하기.** 에이전트의 계획이 `awaiting_approval`로 돌아오면, 운영자 터미널에서 확인하고 정확한 해시로 승인해요. 에이전트가 이어서 실행하면 HTTP 증거를 읽을 수 있어요.

```bash
sw task review  --config-dir ~/.config/shearwater-compose --listen 127.0.0.1:16444 --id=OPERATION_ID
sw task approve --config-dir ~/.config/shearwater-compose --listen 127.0.0.1:16444 --id=OPERATION_ID --confirm=EXACT_PLAN_HASH
sw task evidence --config-dir ~/.config/shearwater-compose --listen 127.0.0.1:16444 --id=OPERATION_ID
```

Compose는 업데이트할 때 컨테이너를 새로 만들어서 짧게 끊길 수 있어요. 무중단 롤링이 필요하면 아래 자체 런타임을 쓰세요. compose 파일 여러 개, 에이전트 계정 분리, 실패 처리는 [Compose 안내](docs/compose-backend.md)에 있어요.

## 자체 런타임으로 시작하기

Shearwater에는 작은 단일 노드 런타임도 있어요. Kubernetes API 일부(Pod, Deployment, Service, Secret)를 직접 구현해서 `kubectl`을 그대로 쓸 수 있고, `k8s.io` 패키지는 하나도 가져오지 않아요. 들어오면 CI가 빌드를 실패시켜요. 롤링 업데이트(아래 측정에서 실패 요청 0건)와 내장 공개 HTTPS 진입점이 더해져요. containerd, runc, 네트워크 도구, 관리자 권한이 필요해요. [설치 준비](docs/install.md)

새 Ubuntu·Debian 서버라면 `sudo python3 install.py --version v0.1.0-alpha.16 --host --apply`가 containerd·runc가 없을 때 설치하고 systemd 서비스를 시작해요. 그다음 `sw setup`으로 인증 정보를 준비하고 첫 앱 안내를 따라가세요. [인증 정보 준비](docs/onboarding.md) · [첫 웹 앱 실행](docs/first-try.md)

**v0.1.0-alpha.16 내려받기** · [설치 도우미](https://github.com/ChoYugyeong/shearwater/releases/download/v0.1.0-alpha.16/install.py) · [Linux amd64 묶음](https://github.com/ChoYugyeong/shearwater/releases/download/v0.1.0-alpha.16/shearwater-v0.1.0-alpha.16-linux-amd64.tar.gz) · [SHA256SUMS](https://github.com/ChoYugyeong/shearwater/releases/download/v0.1.0-alpha.16/SHA256SUMS)

소스에서 빌드하려면 저장소 루트에서 `. hack/env.sh` 뒤에 `bash hack/build.sh`를 실행해요. [빌드 안내](docs/quickstart.md)

## 앱 하나를 AI에게 맡기기

1. **앱과 권한을 정해요.** 운영자가 `sw task create`로 앱 하나, 허용할 동작, 만료 시간을 정해요. AI에는 그 작업의 `work.json`을 연결해요. 외부 AI의 MCP나 내장 Qwen 대화를 쓸 수 있어요.
2. **정확한 계획을 승인해요.** 운영자 터미널의 `sw task review`에서 대상, 변경 전후 이미지, 검사 조건을 읽어요. `sw task approve`에 그 계획의 해시를 지정해야 AI가 적용할 수 있어요.
3. **결과를 확인하고 이어가요.** 서버는 앱 변경과 적용 기록을 SQLite에 함께 저장해요. 모델이 바뀌거나 서버가 재시작돼도 `continue`는 같은 변경을 한 번만 적용해요. 새 Pod 각각과 내부 Service가 HTTP 200 및 지정한 본문 조건을 통과했는지 `sw task evidence`로 확인할 수 있어요.

복구할 때는 이전에 HTTP 검사까지 통과한 이미지로 새 계획을 만들고 다시 승인해요. [위임·승인·복구 명령 예제](docs/agent-operations.md)

## alpha.16에서 새로 생긴 것

- **여러 서버로 써요.** 컨트롤 플레인을 `sw up --advertise-address IP`로 시작하고, `sw node token`이 출력하는 명령을 각 워커에서 실행해요(`sw join`). Pod는 여러 노드에 나뉘어 배치되고(nodeSelector, node affinity, taint, 요청 자원), VXLAN Pod 네트워크로 서로 닿고, 서버가 응답하지 않으면 다른 노드에서 다시 만들어져요. 컨트롤 플레인은 한 대예요(고가용성은 아직 없어요). [여러 서버](docs/multi-node.md)
- **여러 사람과 에이전트가 나눠 써요.** RBAC(Role, ClusterRole, 바인딩, `kubectl auth can-i`·`whoami`), 네임스페이스 하나만 다루는 kubeconfig를 만드는 `sw user add NAME --namespace NS`, ResourceQuota와 Pod Security를 지원해요. 쿼터와 Pod Security는 Deployment가 만드는 Pod에도 적용돼요. [접근 제어](docs/access-control.md)
- **클러스터 DNS와 Ingress.** 모든 노드에서 `sw`가 `<service>.<namespace>.svc.cluster.local`에 답해요. Ingress는 공개 진입점에서 호스트와 경로로 연결하고, 호스트는 먼저 차지한 네임스페이스의 것으로 남아요. [DNS](docs/dns.md) · [Ingress](docs/ingress.md)
- **영속 볼륨과 NetworkPolicy.** 기본 `local-path` StorageClass로 PVC를 쓰면 Pod가 데이터가 있는 노드에 배치돼요. NetworkPolicy는 모든 노드에서 적용돼요.

서버 세 대를 흉내 낸 환경(서버마다 별도 network namespace·containerd·cgroup)에서 워커 장애를 포함한 9단계로 확인했어요. 실제로 서로 다른 서버와 클라우드 네트워크에서는 아직 확인하지 않았어요. VXLAN은 암호화하지 않으니 사설망이나 VPN 위에서 써요. [릴리스 기록](docs/releases.md)

### alpha.15에서 추가된 것

- **실패한 검사가 어디서 왜 실패했는지 알려 줘요.** 배포가 HTTP 검사를 통과하지 못하면(`http_failed`) 에이전트는 어느 검사가 몇 개 사본에서 왜 실패했는지 받아요. 예: `HTTP 200, expected text missing`. 기대한 문구 자체는 적지 않아요. `diagnose`도 마지막 실패 검사를 시각과 함께 보여 줘요.
- **증거가 무엇을 보여 줬는지 말해요.** `sw task evidence`가 저장된 증거의 결과(`passed`/`failed`), 확인 시각, 목표 세대를 보여 줘서, 방금 검증된 앱이 미확인처럼 읽히지 않아요.
- **공개 진입점 인증서 갱신이 더 안정적이에요.** 갱신 시기가 되면 바로 깨어나고, 새 인증서가 안전하게 유효해질 때까지 이전 인증서를 계속 써요. `sw edge status`가 만료를 표시하고, 백업에 진입점 설정·ACME 계정 키·인증서가 들어가요.
- **시작·검증 출력이 더 분명해요.** 시작이 실패하면 `sw up`이 서버 로그의 마지막 줄을 보여 주고, verify의 사실 문구가 백엔드마다 실제로 검사한 대상(Pod와 Service, 또는 컨테이너와 호스트 포트)을 적어요.

실패 설명은 새 GitHub 호스팅 VM의 실제 Docker에서 확인해요. README대로 설치한 공개 릴리스와 현재 소스로 빌드한 실행 파일, 두 경우 모두 확인해요(빠른 시작 검사). 인증서 변경은 ACME 시험 서버 Pebble로 확인했어요. [릴리스 기록](docs/releases.md)

### alpha.14에서 추가된 것

- **기존 Docker Compose 앱.** `sw compose serve`를 Compose 프로젝트 앞에 띄우면 서비스 하나를 같은 위임·승인·HTTP 증거 흐름으로 맡길 수 있어요. compose 파일은 고치지 않아요. [Compose 안내](docs/compose-backend.md)
- **공개 HTTPS 진입점.** Service에 `shearwater.io/public-host` 주석을 달고 `sw edge enable`을 켜면 80·443 포트로 내보내요. 인증서는 ACME(HTTP-01)로 받아요. [공개 진입점 안내](docs/public-entry.md)
- **Secret.** 컨테이너가 `env[].valueFrom.secretKeyRef`와 `envFrom[].secretRef`로 값을 읽어요. 값은 base64로 저장할 뿐 암호화하지 않아요.
- **새 서버를 한 번에.** 새 Ubuntu·Debian 서버에서 `sudo python3 install.py --version v0.1.0-alpha.16 --host --apply`를 실행하면, containerd가 없을 때만 containerd·runc를 설치하고 IPv4 전달을 켠 뒤 systemd 서비스를 시작해요. [설치 안내 0절](docs/install.md)

이 기능들은 이 PC의 격리된 시험 환경과 새로 만든 로컬 Ubuntu VM에서 확인했어요. 실제 kubectl, 실제 Docker Compose, ACME 시험용 서버 Pebble을 썼어요. 공인 서버와 실제 Let's Encrypt 인증서로는 아직 확인하지 않았어요. [릴리스 기록](docs/releases.md)

## 측정 결과

같은 Ubuntu 24.04 VM(2 vCPU · 2,560 MiB)에서 같은 nginx 웹 앱을 시험했어요. alpha.12는 앞선 여섯 회의 기준값이에요. alpha.13은 공개 태그에서 새로 빌드한 배포 파일로 다시 쟀어요. alpha.14·alpha.15·alpha.16에서는 다시 재지 않았어요.

| 측정 항목 | 공개 alpha.12 | 공개 alpha.13 |
| --- | ---: | ---: |
| 사본 10개 업데이트 중앙값 (6회) | 22.80초 | **2.83초** |
| 업데이트 중 오류 / 전체 요청 | 0 / 13,762 | **0 / 1,750** |
| 120 Pod 직접 연결 오류 / 전체 요청 (30초 × 2회) | 27,474 / 72,000 | **0 / 72,000** |
| 120 Pod 직접 연결 오류 / 전체 요청 (별도 90초) | 미측정 | **0 / 108,000** |

업데이트 중에는 초당 100개의 새 연결을 보내고, 사본 열 개와 내부 Service가 모두 새 본문을 반환하면 시간을 멈췄어요. alpha.13의 교체 시간이 이 장비에서 87.6% 줄어 구간 내 요청 수도 줄었어요. 밀도 시험에서는 120개 Pod 각각에 초당 10건을 재시도 없이 직접 보냈어요. 120개는 이 시험에서 가장 높은 단계예요. [회차별 결과와 조건](docs/connection-verification.md) · [측정값 CSV](docs/benchmarks/alpha13.csv) · [실행 파일과 체크섬](https://github.com/ChoYugyeong/shearwater/releases/tag/v0.1.0-alpha.13)

### 실제 리소스 사용량

이전 WSL 2 시험(2026-11-11)에서는 BusyBox Pod 10개를 띄우고 60초 동안 유휴 상태를 관찰했어요. `sw server`의 최대 RSS는 34.76 MB, 디스크 쓰기는 0 B였어요. containerd와 앱이 쓰는 자원은 별도예요. [측정 범위와 재현 방법](docs/stage1-verification.md)

2026-11-17에는 alpha.9를 Docker Compose, K3s와 같은 장비에서 같은 앱으로 돌려 메모리·배포 준비 시간·업데이트 중 오류를 쟀어요. 세 도구는 기능 범위가 달라서 순위로 읽으면 안 돼요. [비교 조건과 결과](docs/benchmarks/runtime-comparison-alpha9.md)

같은 측정을 이제 GitHub Actions에서 minikube까지 넣어 돌려요(수동 실행과 매주 자동 실행). 2026-11-24에는 4코어 공유 러너에서 네 도구를 하나씩 3회 반복으로 쟀어요. [CI 비교와 결과](docs/benchmarks/runtime-bench-ci.md)

## 어떤 환경에 맞나요?

관리자 한 명이 신뢰하는 작은 HTTP 앱을 맡기는 **실험적 알파**예요. 단일 노드·IPv4·TCP ClusterIP를 지원해요. 선택 기능인 공개 진입점이 주석을 단 Service의 TLS를 맡지만, 공개 도메인 연결과 로그인은 따로 확인해야 하고, 이미지 복구에는 DB 데이터 복원이 포함되지 않아요. `securityContext`는 저장만 하며 다중 사용자 격리와 RBAC는 지원 범위 밖이에요. [호환표](docs/compatibility.md)

이미 Docker Compose로 앱 몇 개를 돌리고 있다면 Compose는 그대로 두고 그 앞에 Shearwater를 붙이세요. 여러 노드, Ingress, PVC, Helm이 필요하면 Kubernetes나 K3s를 살펴보세요. [도구별 용도 비교](docs/choosing-shearwater.md)

[문제 진단하기](docs/troubleshooting.md) · [체험 결과 남기기](docs/first-try-feedback.md) · [기여하기](CONTRIBUTING.md) · [보안 제보](SECURITY.md) · [Apache 2.0 라이선스](LICENSE) · [출처 고지](NOTICE)
