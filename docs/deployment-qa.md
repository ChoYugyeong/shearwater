> 공개 준비 과정에서 개인 경로·내부 세션 식별자·로컬 앱 이름을 일반화했어. 측정값과 당시 검증 범위는 유지했고, `<private-evidence>`는 공개 저장소에 없는 원본 기록을 뜻해.

# 검증용 웹 앱 실제 배포를 이용한 QA (2026-11-12)

## 무엇을 확인했나

실제 앱이 있는 기본 Shearwater 서버에서 추가 테스트 Deployment를 실행했다. 기존 M2–M6 검사는 containerd의 `shearwater` 네임스페이스가 비어 있어야 한다. 새 `hack/deployment-check.py`는 이미 실행 중인 서버를 사용하면서 별도 Kubernetes 네임스페이스에 자신의 객체를 만든다.

여기서 **containerd 네임스페이스**는 모든 Shearwater 컨테이너를 Docker와 구분하는 `shearwater`다. **Kubernetes 네임스페이스**는 그 안의 앱 객체를 구분하는 단위다. 실제 앱은 `local-apps`, 자동 검사는 `deployqa-<고유 번호>`, 브라우저 검사는 `sample-web-browser-qa-20261112`를 사용했다. 라벨이 같아도 다른 네임스페이스의 Pod를 Service에 섞지 않는지 검사했다.

## 결과

| 검증 | 실제 관측 |
|---|---|
| 새 배포 검사 | 최종 인프라 검사 **27 통과, 0 실패** |
| HTTP | HTML·CSS·ES 모듈 원본 바이트와 MIME, HEAD, Range 206, 없는 파일 404, 모듈 조건부 요청 304 |
| 업데이트 캐시 | HTML의 이전 조건부 요청에도 200으로 새 내용을 보냄. 모듈은 내용 해시 경로에서 캐시 |
| 동시 연결 | 동시 6개로 60회, 모든 응답 본문 일치. 중앙값 52.96ms, 최대 82.70ms |
| 복제본 수 | 2개 생성 후 3→1→0→2. 각 단계의 Ready Pod와 EndpointSlice 개수, HTTP 성공/실패 확인 |
| Pod 삭제 | 새 UID의 Pod로 대체, HTTP 회복 |
| 컨테이너 SIGKILL | 같은 Pod IP, 증가한 재시작 수, `logs --previous`, HTTP 회복 |
| selector 변경 | 일치하는 Pod가 없으면 연결 거절. 원래 selector로 복원하면 같은 ClusterIP로 회복 |
| 실패한 롤아웃 | 없는 실행 파일로 새 Pod 실패. `maxUnavailable: 0`에서 기존 2개 Ready 및 HTTP 유지 |
| 롤백 | `rollout undo/status/history` 성공 |
| Namespace 삭제 | 비어 있지 않은 네임스페이스는 Conflict로 거절 |
| 정상 서버 재시작 | `sw down` 중 직접 Pod HTTP 유지. `sw up` 뒤 UID·IP·컨테이너 ID·재시작 수 보존 |
| 서버 SIGKILL | PID 핸들·시작 시각·실행 파일을 확인해 sw에만 신호. 같은 태스크 인계와 Service 복구 |
| 실제 앱 보존 | 테스트 전후 실제 앱의 태스크 식별자 보존, `sw doctor` 성공 |
| 정리 | 생성 당시 Namespace UID를 대조하고 자체 객체만 제거. 검사 자료는 남김 |
| 기존 회귀 | `bash hack/t.sh ./...`, M1 Linux+Windows **103/0**, M0 **47/0** |
| 최종 실제 교체 | 검증용 웹 앱 `18e6512`를 `local-apps`에 롤링 교체. 교체 전후 HTTP 200회 중 **200회 성공**, ClusterIP `10.96.0.1` 유지 |

60회 요청은 기능과 짧은 동시 연결 검사다. 장시간 부하나 최대 처리량을 측정한 결과는 아니다. `sw down` 동안 Pod 프로세스와 직접 Pod HTTP는 유지되지만, 같은 프로세스의 API·Service TCP 프록시는 잠시 중단된다.

## 증거 경로

- 최초 앱으로 검사한 26개 결과: `/var/lib/shearwater/check/deployment.r56wikez/`
- 버전별 웹 자산과 nginx 설정을 추가한 뒤 27개 결과: `/var/lib/shearwater/check/deployment.jpar5y8w/results.json`
- 명령·응답: 같은 폴더의 `commands.jsonl`, 실패한 롤아웃 상태: `failed-rollout.json`
- M1: `$HOME/.cache/shearwater-dev/m1-20261112-164426-10549/`
- M0: `$HOME/.cache/shearwater-dev/m0-20261112-164640/`
- 브라우저 기능·스크린샷·최종 배포 기록: `<private-evidence>\shearwater-sample-web-20261112\`
- 앱 자체의 수정과 검사 설명: `<app-source>\docs\deployment-qa.md`

테스트 스크립트의 초기 한 번은 Python 임시 폴더명에 들어간 `_`를 Namespace 이름에 써서 생성 단계에서 거절됐다. 이를 고유 해시로 바꾸고 다시 실행했다. 이 실패는 `/var/lib/shearwater/check/deployment.770_t87v/`에 남아 있으며 통과 수에 포함하지 않았다. 새 스크립트 이외 Shearwater의 런타임·API 구현은 이번 QA에서 수정하지 않았다.

최종 배포는 `/opt/shearwater/deployments/sample-web/18e6512/deployment.yaml`로 적용했다. `rollout-check.json`에 교체 중 200회 요청의 결과가 있다. 일반 `kind: List`로 묶은 JSON을 `kubectl apply`하면 클라이언트 검증이 미지원 `/openapi/v2`를 요청해 404로 중단되는 것도 관측했다. 검증을 끄지 않고, 객체마다 `---`로 구분한 YAML 문서로 배포 파일을 고쳤다. 이번에 OpenAPI v2 지원을 추가한 것은 아니다.

과거 캐시 정책으로 저장된 첫 HTML은 재방문 시 한 번 남아 있을 수 있다. 실제 `18080` 주소에서 보통 새로고침 후 새 해시 경로의 모듈, 기존 북마크 4개, 썸네일을 확인했다. 이후 HTML은 새 `no-cache` 정책을 따른다. QA 네임스페이스와 `18081` 중계는 정리했고 이 QA 직후 실제 앱은 `1/1 Ready`, 재시작 0회였다. 이후 [실제 WSL 재부팅 검사](reboot-soak.md) 두 번으로 재시작 수가 2가 됐으며 같은 Pod·IP로 복구했다.

## 다시 실행하기

WSL의 실제 앱이 `local-apps/sample-web` Service로 실행 중이어야 한다. 다른 클러스터에 실수로 요청하지 않도록 스크립트가 kubeconfig의 서버 주소 `https://127.0.0.1:6443`을 먼저 확인한다. SQLite는 기존 WSL ext4 경로를 사용한다. 예시는 이번에 검증한 묶음의 경로다.

```powershell
wsl.exe -d Ubuntu-24.04 -u root --cd $SOURCE_DIR -- python3 -B -I hack/deployment-check.py --sw $HOME/.cache/shearwater-dev/sample-web-build.oGBdyL5b/sw --kubectl $HOME/.local/bin/kubectl --site-dir /opt/shearwater/deployments/sample-web/qa-20261112-3/site --nginx-config /opt/shearwater/deployments/sample-web/qa-20261112-3/nginx.conf
```

이 명령은 서버를 재시작하지 않는다. 공유 서버 재시작까지 검사하려면 끝에 `--restart-server`를 붙인다. 실제 앱의 Service도 잠깐 중단되므로 재시작이 가능한 시간에 실행한다. 이 옵션을 썼을 때 27개를 검사하며, 생략하면 서버 재시작 관련 3개가 빠진다. 정상 종료와 오류 시 모두 자신의 Deployment·Service·자식 Pod를 정리하고 증거 디렉터리는 보존한다.

root는 Shearwater의 인증된 API, `shearwater` 컨테이너의 정확한 태스크 하나, sw 서버 PID 핸들, `/var/lib/shearwater/check` 및 지정 배포 폴더에 사용한다. Docker·containerd 설정 파일, `moby`·`moby_history`는 읽거나 변경하지 않는다. 실행 중인 실제 앱이 있으므로 기존 `m2-check.sh`~`e2e-stage1.sh`를 곧바로 실행하지 않는다.

## 범위

Windows ego에서 실제 연결한 것은 로컬 주소다. 인터넷 공개 배포·TLS 도메인·NodePort·port-forward를 구현한 것이 아니다. 이 문서의 초기 QA에서는 실제 WSL/Windows 재부팅, Electron 패키지, 유튜브 OAuth 로그인, 장시간 부하를 실행하지 않았다. 실제 WSL 재부팅과 60분 부하의 후속 관측은 [별도 기록](reboot-soak.md)에서 확인한다. 준비 상태는 현재 1단계 구현의 컨테이너 상태 기준이며 일반적인 앱 준비 프로브나 무중단 보장을 의미하지 않는다.

## 다음 세션 첫 프롬프트

`<workspace>/shearwater\docs\HANDOFF.md`와 `docs/deployment-qa.md`, `<private-evidence>\shearwater-sample-web-20261112\deployment.md`를 먼저 읽어. 실제 검증용 웹 앱이 기본 서버에서 실행 중이므로 빈 런타임을 전제로 하는 검사를 돌리지 마. 레포 전체나 <workspace>의 다른 프로젝트를 재탐색하지 말고 요청과 관련된 파일만 읽어.
