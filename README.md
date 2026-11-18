# Shearwater

*Human-approved updates for single-node containers.*

**“웹 앱을 새 이미지로 바꾸고, 새 버전이 실제로 응답하는지 확인해 줘.”**

Shearwater는 Linux 서버 한 대에서 작은 웹 앱을 운영하는 오픈소스 도구예요. AI가 변경을 제안하면 사람이 대상과 내용을 보고 그 계획 하나만 승인해요. 대화나 서버가 중간에 끊겨도 같은 작업을 이어가고, 새 앱 사본(Pod)과 내부 접속 주소(Service)에 HTTP 요청을 보내 결과를 확인해요.

[처음 시작하기](#처음-시작하기) · [앱 하나를 AI에게 맡기기](#앱-하나를-ai에게-맡기기) · [측정 결과](#측정-결과)

<p align="center">
  <img src="docs/media/approved-update.gif" width="576" alt="실제 업데이트 기록 재생. 승인 대기, 승인 후 미적용, 새 배포 요청 거절, 원래 계획의 HTTP 검증 통과 순서예요.">
</p>

**기록 재생** · alpha.12 개발 중 Qwen과 nginx로 진행한 시험의 MCP 요청·응답을 발췌했어요. 이 시험에서는 운영자 역할의 검사 프로그램이 계획을 확인하고 승인했어요. [시험 조건과 결과](docs/agent-operations-verification.md)

<details>
<summary>네 장면을 글로 읽기</summary>

1. AI가 `update`를 요청하자 `awaiting_approval`이 돌아왔어요. `approved`와 `applied`는 모두 `false`였어요.
2. 같은 계획을 승인한 뒤 `verify`로 읽은 상태는 `approved_pending`이었어요. `approved`는 `true`, `applied`는 아직 `false`였어요.
3. 그 사이 AI가 새 `deploy`를 요청했어요. 실행을 기다리는 계획이 있어서 서버가 `409 OperationRejected`로 거절했고, 상태는 `Failure`였어요.
4. `continue`로 원래 계획을 이어간 결과는 `healthy`였어요. 검사한 새 Pod 수(`verifiedPods`)는 1이었고, 내부 Service까지 검사한 결과(`http.state`)는 `passed`였어요.

</details>

## 처음 시작하기

WSL 2의 Ubuntu 24.04(amd64)에서 체험할 수 있어요. 컨테이너를 실행할 containerd·runc, 네트워크 도구와 관리자 권한이 필요해요. [설치 준비물](docs/install.md)

**v0.1.0-alpha.13 다운로드** · [설치기](https://github.com/ChoYugyeong/shearwater/releases/download/v0.1.0-alpha.13/install.py) · [Linux amd64 압축 파일](https://github.com/ChoYugyeong/shearwater/releases/download/v0.1.0-alpha.13/shearwater-v0.1.0-alpha.13-linux-amd64.tar.gz) · [SHA256SUMS](https://github.com/ChoYugyeong/shearwater/releases/download/v0.1.0-alpha.13/SHA256SUMS)

Ubuntu Bash에서 아래 명령을 실행하세요. 빈 임시 폴더에 설치기를 받아 체크섬을 검사해요. 설치기는 압축 파일과 그 안의 `sw`도 검증한 뒤 버전별 폴더에 설치해요.

```bash
(
  set -eu
  version=v0.1.0-alpha.13
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

버전 출력에서 `0.1.0-alpha.13`을 확인하세요. 첫 앱은 `sw setup`으로 인증을 준비한 뒤 예제 nginx의 첫 HTTP 응답까지 따라가면 돼요. [인증 준비](docs/onboarding.md) · [첫 웹 앱 띄우기](docs/first-try.md)

소스에서 빌드하려면 저장소 루트에서 `. hack/env.sh` 다음 `bash hack/build.sh`를 실행하세요. [개발 환경과 빌드 안내](docs/quickstart.md)

## 앱 하나를 AI에게 맡기기

1. **앱과 권한을 정해요.** 운영자가 `sw task create`로 앱 하나, 허용할 동작, 만료 시간을 정해요. AI에는 그 작업의 `work.json`을 연결해요. 외부 AI의 MCP나 내장 Qwen 대화를 쓸 수 있어요.
2. **정확한 계획을 승인해요.** 운영자 터미널의 `sw task review`에서 대상, 변경 전후 이미지, 검사 조건을 읽어요. `sw task approve`에 그 계획의 해시를 지정해야 AI가 적용할 수 있어요.
3. **결과를 확인하고 이어가요.** 서버는 앱 변경과 적용 기록을 SQLite에 함께 저장해요. 모델이 바뀌거나 서버가 재시작돼도 `continue`는 같은 변경을 한 번만 적용해요. 새 Pod 각각과 내부 Service가 HTTP 200 및 지정한 본문 조건을 통과했는지 `sw task evidence`로 확인할 수 있어요.

복구할 때는 이전에 HTTP 검사까지 통과한 이미지로 새 계획을 만들고 다시 승인해요. [위임·승인·복구 명령 예제](docs/agent-operations.md)

## 측정 결과

2026-11-18, 같은 Ubuntu 24.04 Hyper-V VM(2 vCPU, 2,560 MiB)에서 공개 alpha.12와 **공개 전 로컬 수정본**을 번갈아 시험했어요. nginx 사본 10개의 업데이트를 여섯 쌍 비교했어요. 공개 alpha.13 바이너리의 재시험 결과는 해당 버전의 릴리스 기록에서 확인할 수 있어요. [alpha.13 릴리스 기록](https://github.com/ChoYugyeong/shearwater/releases/tag/v0.1.0-alpha.13)

| 측정 항목 | 공개 alpha.12 | 로컬 수정본 |
| --- | ---: | ---: |
| 사본 10개 업데이트 · 중앙값 | 22.80초 | **2.82초** |
| 업데이트 중 요청 · 6회 합계 | 0 / 13,762 | 0 / 1,694 |
| 120 Pod 직접 연결 · 30초 × 2회 | 27,474 / 72,000 | **0 / 72,000** |
| 120 Pod 직접 연결 · 별도 90초 | 미측정 | **0 / 108,000** |

분수는 **오류 수 / 전체 요청 수**예요. 업데이트 중에는 초당 100건을 보내고 모든 사본과 Service가 새 본문을 돌려주면 멈췄어요. 빨리 끝난 쪽의 요청 수가 적은 이유예요. 120 Pod 시험은 Pod마다 초당 10건을 재시도 없이 직접 보냈어요. 120개는 이 VM에서 시험한 가장 높은 단계예요. [측정 조건과 판정 기준](docs/connection-verification.md) · [준비 검사와 연결 처리](docs/connection-continuity.md)

### 실제 리소스 사용량

이전 WSL 2 시험(2026-11-11)에서는 BusyBox Pod 10개를 띄우고 60초 동안 유휴 상태를 관찰했어요. `sw server`의 최대 RSS는 34.76 MB, 디스크 쓰기는 0 B였어요. containerd와 앱이 쓰는 자원은 별도예요. [측정 범위와 재현 방법](docs/stage1-verification.md)

## 어떤 환경에 맞나요?

관리자 한 명이 신뢰하는 작은 HTTP 앱을 맡기는 **실험적 알파**예요. 단일 노드·IPv4·TCP ClusterIP를 지원해요. 내부 HTTP 검사로는 공개 도메인·TLS·로그인까지 확인할 수 없고, 이미지 복구에는 DB 데이터 복원이 포함되지 않아요. `securityContext`는 저장만 하며 다중 사용자 격리와 RBAC는 지원 범위 밖이에요. [호환표](docs/compatibility.md)

단일 서버에서 앱 몇 개를 빌드하고 실행하려면 Docker Compose도 좋은 선택이에요. 여러 노드, Ingress, PVC, Helm이 필요하면 Kubernetes나 K3s를 살펴보세요. [도구별 용도 비교](docs/choosing-shearwater.md)

[문제 진단하기](docs/troubleshooting.md) · [체험 결과 남기기](docs/first-try-feedback.md) · [기여하기](CONTRIBUTING.md) · [보안 제보](SECURITY.md) · [Apache 2.0 라이선스](LICENSE) · [출처 고지](NOTICE)
