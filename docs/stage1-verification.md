# 1단계 검증: 무엇을 어떻게 확인했나

이 기록은 Shearwater가 지원하는 **1단계 범위**를 확인한 결과야. 모든 Kubernetes 기능이나 모든 Linux 환경에서 동작한다는 뜻은 아니야. 실행 방법은 [운영 안내서](operations.md#검증-재현), 리소스 수치는 [README](../README.md)에 있어.

## 통합 검증이 확인하는 것

`hack/e2e-stage1.sh`는 WSL 안에 별도 데이터베이스와 kubeconfig를 만들고, 실제 kubectl로 nginx Deployment를 운영해. 2026-11-11 최종 실행은 `PASS` 23개였어. `PASS`는 명령이 끝났다는 뜻만이 아니라 아래 조건이 확인됐다는 뜻이야. 17번 측정은 앱을 바꾸는 작업의 영향을 피하려고 `hack/measure.sh`에서 따로 실행했고 목표를 충족했어.

| 설계 11절 | 검증한 결과 |
|---|---|
| 1–4 | kubectl이 HTTPS API에서 서버 버전·API 목록을 읽었고, Ready 노드 한 개와 nginx Pod 두 개를 확인했다. Deployment·ReplicaSet·Pod의 넓은 표에 상태와 Pod IP가 나왔다. |
| 5–8 | Pod IP에서 nginx HTTP 200을 받았다. 컨테이너를 죽인 뒤 같은 Pod에서 재시작 횟수가 1 늘었다. Pod를 삭제하자 새 UID의 Pod가 생겼고, `kubectl scale` 뒤 네 Pod가 준비됐다. |
| 9–11 | 이미지 변경을 `kubectl apply`로 반영해 새 ReplicaSet으로 교체했다. 롤아웃 이력 두 개와 환경변수 추가·삭제, 컨테이너 순서 변경을 확인했다. 실제 HTTP 접근 로그, 라벨 변화 watch, `describe`의 `Scheduled`·`Started` 이벤트가 나왔다. |
| 12–13 | EndpointSlice의 주소가 Ready Pod와 맞았고 ClusterIP에서 HTTP 200을 받았다. 서버를 SIGKILL해도 기존 태스크가 계속 실행됐다. 새 서버가 같은 SQLite 상태·컨테이너 ID·재시작 횟수를 읽었고 Service가 다시 응답했다. |
| 14–15 | 서버 dry-run 업데이트와 생성 결과는 저장되지 않았다. 낡은 리비전은 409, 거부 필드는 422, 저장만 필드는 경고와 200, 모르는 필드를 Strict로 보내면 400이었다. |
| 16 | Deployment와 Service를 지우자 테스트용 ReplicaSet·Pod·Slice, `shearwater` 컨테이너와 태스크, veth와 Pod 네임스페이스, IP 할당이 남지 않았다. Docker 네임스페이스의 직접 조회·전후 비교는 사용자 제한에 따라 하지 않았다. 코드와 테스트에서 `shearwater`만 지정하는 것을 확인했다. |
| 17–18 | Pod 10개를 띄워 별도로 60초를 측정했다. 빈 경로에서 `sw up`으로 만든 kubeconfig로 kubectl이 붙었다. 반복 `up`은 프로세스와 kubeconfig를 유지했다. 잘못된 containerd 소켓을 지정하자 설명과 해결법을 출력하고 서버를 만들지 않았다. |
| 19–21 | 모든 네임스페이스 목록, 셀렉터를 벗어난 Pod의 DELETED watch, 스케일 전제조건을 확인했다. 서버가 꺼진 동안 죽은 태스크는 복구 후 재시작했고 마지막 종료 시각이 남았다. 같은 이름으로 새 Pod를 만들었을 때 kubectl 삭제 대기는 옛 UID에서 끝났다. |
| 운영 명령 추가 검증 | `sw status`·`sw tree`가 실제 상태를 읽었고 `sw doctor`가 준비 상태를 확인했다. `down --all`로 Pod와 전용 네트워크를 정리한 뒤 SQLite·IP 예약·인증서·로그가 남았다. 빈 런타임 경로로 다시 시작했을 때 같은 UID, Pod IP, ClusterIP에서 HTTP가 돌아왔다. |

## 재시작을 세 가지로 나눠 본 이유

1. **서버만 죽었을 때**: containerd가 가진 태스크를 그대로 인계해야 해. 같은 컨테이너 ID·프로세스 ID·재시작 횟수와 응답을 비교했어.
2. **서버가 없는 동안 앱까지 죽었을 때**: 이미 실행 중인 태스크가 없으므로, 종료 시각을 읽고 필요한 새 컨테이너를 띄워야 해. Pod UID와 IP는 그대로고 컨테이너 ID만 바뀌는지 확인했어.
3. **일시적인 네트워크 상태도 사라졌을 때**: 새 veth·Pod 네임스페이스·Service 주소가 필요해. M6 최초 검증에서는 전용 장치와 규칙을 제거한 뒤 빈 `/run/shearwater` 하위 경로로 시작해서 재현했어. 2026-11-12에는 실제 WSL 배포판 종료와 VM 전체 종료를 각각 실행해 같은 UID·IP 복구도 확인했어. 커널 boot ID 변경, DB 검사, 60분 부하의 후속 기록은 [실제 재부팅·부하 검사](reboot-soak.md)에 있어.

## 별도 검사

- Go 검증: `bash hack/t.sh ./...`로 포맷, vet, 단위 테스트를 확인했다. 리비전이 섞인 읽기를 버리는지, 다른 프로세스와 공유 설정 경로를 거부하는지, 종료를 발견했을 때 Pod를 다시 실행하는지도 다룬다.
- WSL 격리 네임스페이스: 같은 이름의 방화벽 체인에 다른 규칙을 넣어 시작·정리가 그 규칙을 보존하는지 확인했다. 다른 네트워크 경로와 겹치는 Service IP만 빼는 검사도 돌렸다. 테스트가 만든 규칙과 주소는 사라지는 테스트 네임스페이스 안에만 있다.
- M0–M5 회귀: Windows와 Linux kubectl의 M0 47개·M1 103개, M2 런타임, M3 조정과 GC, M4 롤아웃, M5 Service를 각각 확인했다.

통합 실행의 `results.txt`와 번호가 붙은 명령 출력, 측정의 `measure.json`은 WSL의 `/var/lib/shearwater/check/` 아래에 있다. 구체적인 경로는 [핸드오프](HANDOFF.md)의 M6 실측에 남겨 뒀어.
