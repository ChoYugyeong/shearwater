# Shearwater를 시작하고, 상태를 보고, 다시 살리기

Shearwater에서 **서버**는 `kubectl` 요청을 받고 저장된 설정대로 Pod를 맞추는 `sw server` 프로세스야. **Pod**는 containerd가 실행하는 앱이야. 서버와 앱이 별개라서 서버를 잠깐 끄거나 다시 띄울 때 앱을 계속 실행할 수 있어.

## 시작 전 준비

- 명령은 WSL Ubuntu-24.04에서 실행해. 바이너리는 일반 사용자로 [README의 명령](../README.md#처음-시작하기)을 따라 빌드해.
- Pod를 실행하려면 기존 containerd 소켓과 `runc`, `iptables`, IPv4 전달, cgroup v2가 필요해. Shearwater는 containerd에서 전용 이름표 `shearwater`를 사용해. Docker가 같은 containerd를 사용해도 설정 파일은 변경하지 않아.
- 명령 예제의 `sw`는 빌드한 파일의 경로로 바꿔. 기본 위치에 실제 Pod를 실행하는 예제는 WSL의 root에서 실행해. Windows PowerShell에서 `wsl.exe -d Ubuntu-24.04 -u root`로 들어갈 수 있어.

## 운영 명령

```bash
sw doctor
sw up
kubectl --kubeconfig /etc/shearwater/kubeconfig get nodes
sw status
sw tree
```

**`sw up`**은 먼저 API 포트와 파일시스템을 확인하고, containerd가 응답하는지와 호스트 주소 대역이 겹치는지 확인해. 문제가 있으면 오류와 다음 조치를 표시하고 멈춰. 정상이라면 현재 터미널과 분리된 `sw server`를 시작하고, 인증된 API가 열렸는지 확인한 후 kubectl 명령을 출력해. 똑같은 옵션으로 한 번 더 실행하면 같은 서버를 이용해. 아래 표에 있는 인증서와 kubeconfig를 처음에 만들어. 인증을 건너뛰는 설정은 필요 없어.

**`sw status`**는 서버와 노드가 준비됐는지, Pod가 몇 개 준비됐는지 보여 줘. **리비전**은 서버가 읽은 저장 상태의 번호야. API가 꺼져 있으면 캐시된 상태를 정상으로 보여 주는 대신 `NOT_RUNNING`을 표시해. 두 읽기 명령은 목록들의 리비전이 맞을 때만 출력해. 배포가 너무 빨리 바뀌어 `CONFLICT retry=safe`가 나왔다면 명령을 다시 실행하면 돼.

**`sw tree`**는 Deployment → ReplicaSet → Pod의 소유 관계를 들여쓰기해 보여 줘. Service는 소유하는 Pod가 없으므로 옆 줄에 IP·포트와 연결 대상 수가 나와. 트리는 이름이 같은 다른 객체에 붙이지 않도록 각 객체의 고유 ID(UID)를 기준으로 연결해.

```bash
sw tree -n default
sw tree -n default --root deploy/web
sw tree --problems
```

`-n`은 네임스페이스를 고르고, `--root`는 보고 싶은 가지를 골라. `--problems`는 준비되지 않았거나 삭제 중인 객체와 그 위치를 설명해 주는 상위 객체만 보여 줘. 앱이 이미지를 받는 중이거나 새 버전으로 교체되는 중에도 잠깐 나타날 수 있으니 원인 열을 함께 봐.

**`sw doctor`**는 호스트를 바꾸지 않고 확인해. `OK`는 통과, `WARN`은 확인할 점, `FAIL`은 시작하거나 운영하기 전에 해결할 문제야. 기본 서버가 꺼져 있어도 파일이나 네트워크를 확인할 수 있어. 예를 들어 containerd 소켓을 찾을 수 없으면 소켓 경로와 고치는 방법이 나오고, 네트워크 대역이 겹치면 충돌한 경로가 나와. IPv4 전달이 꺼져 있으면 켜 달라고 안내하고 호스트 공통 설정을 직접 바꾸지 않아. 이미 적용 중인 정책으로 오해하지 않도록 1단계에서는 로컬 관리자 토큰만 사용한다는 점도 표시해.

## 정지와 복구

| 상황 | Pod와 Service에는 무슨 일이 생기나 | 다음 명령 |
|---|---|---|
| 서버만 정지: `sw down` | 실행 중인 Pod와 직접 Pod IP 통신은 유지돼. API와 Service TCP 프록시는 멈춰. | 같은 옵션으로 `sw up` |
| 서버가 강제 종료됨 | 정상 정지처럼 containerd가 앱을 계속 돌려. PID 기록만 남을 수 있어. | 같은 옵션으로 `sw up` |
| 서버 중단 중 컨테이너도 종료됨 | 서버가 돌아올 때까지 자동 재시작이 없어. 돌아오면 종료 시각·코드를 읽고 재시작 정책에 따라 처리해. | `sw up` 후 `kubectl get pods`, `kubectl logs --previous` |
| 전부 정지: `sw down --all` | 컨테이너와 전용 네트워크 장치·규칙을 제거해. SQLite 객체·IP 예약·인증서·볼륨·로그는 남아. 이 명령은 앱의 유예 종료를 기다리는 배포 삭제와 달라. | 같은 옵션으로 `sw up` 후 재시작 정책에 따라 앱을 다시 띄워 |

일반 정지 후 재시작하면 컨테이너 ID와 `RESTARTS`가 그대로야. 강제 정지로 기존 태스크와 네임스페이스가 없어진 경우 노드 에이전트는 SQLite에 예약된 **같은 Pod IP**로 네임스페이스를 다시 만들어. 예를 들어 Deployment의 `Always` Pod는 다시 시작하고, 이미 실행을 마친 `Never` Pod는 재시작하지 않아.

각 실행 시도는 `/sys/fs/cgroup/shearwater/<Pod UID>/<컨테이너 이름>_<시도 번호>`라는 별도 cgroup을 사용해. cgroup은 커널이 프로세스를 묶고 CPU·메모리 제한을 적용하는 단위야. `_`는 유효한 컨테이너 이름에 들어갈 수 없어서 구버전의 이름만 있는 경로와도 겹치지 않아. 이전 시도를 지울 때 새 시도의 프로세스나 제한과 섞이지 않게 경로를 분리했어. 구버전에서 같은 경로를 쓰던 태스크가 남아 있어도 전체 정리에서는 실행 중인 시도를 먼저 내린 뒤 종료된 시도를 지워.

WSL 배포판만 종료하면 커널은 계속 살아 있어서 이전 시도의 빈 cgroup이 남기도 해. 소유 Pod의 태스크를 모두 제거한 뒤에는 이런 바로 아래의 빈 디렉터리도 정리해. 중첩 경로를 재귀 삭제하거나, 정리를 위해 다른 프로세스를 종료·이동하지 않아. 아직 프로세스나 자식 cgroup이 있으면 커널이 삭제를 거절하고 오류를 알려 줘.

PID는 운영체제가 프로세스에 붙이는 번호야. 다른 프로세스에 그 번호를 재사용할 수 있어서 `sw down`은 파일에 적힌 숫자만 읽고 신호를 보내지 않아. 시작 시각·인스턴스 식별자와 Linux의 프로세스 핸들(pidfd)을 대조해. PID 파일이 남아 있어도 현재 프로세스가 일치하지 않으면 신호를 보내지 않아. [Linux pidfd 설명](https://man7.org/linux/man-pages/man2/pidfd_open.2.html)

검사용으로 직접 실행한 `sw server`는 해당 터미널의 Ctrl+C나 원래 실행한 프로세스의 정상 종료로 멈춰. `sw down`이 관리하는 대상은 `sw up`이 띄운 서버야. 이 방식으로 같은 포트의 다른 프로그램을 건드리지 않아.

## 파일과 인증

| 기본 경로 | 내용 |
|---|---|
| `/var/lib/shearwater` | SQLite `state.db`, Pod 디렉터리, 볼륨과 IP 예약 |
| `/etc/shearwater` | CA, 서버 인증서, 관리자 토큰, `kubeconfig` |
| `/run/shearwater` | PID 기록, 프로세스 잠금, Pod 네트워크 네임스페이스, 노드 소유 기록 |
| `/var/log/shearwater` 또는 `/var/lib/shearwater/log` | 백그라운드 서버의 `server.log`, 컨테이너 로그 |

호스트의 `/var/log`에 다른 그룹의 쓰기 권한이 있으면 Shearwater는 자체 데이터 디렉터리에 로그를 둬. `sw doctor`에서 선택된 경로를 확인할 수 있어.

kubeconfig는 kubectl이 접속할 서버와 그 서버를 확인할 인증서, 관리자 토큰을 담아. 읽을 수 있는 사람은 이 인스턴스를 관리할 수 있으니 공유 폴더나 커밋에 넣지 마. 일반 사용자로 옮겨 쓸 때는 root에서 `sw kubeconfig`로 출력한 내용을 사용자만 읽을 수 있는 파일로 저장해. 토큰이나 인증서를 화면에 복사할 필요는 없어.

관리되는 인스턴스의 설정 디렉터리는 DB 하나에만 연결돼. 같은 경로를 서로 다른 DB에 쓰면 이전에 출력된 kubeconfig가 다른 서버를 가리킬 수 있으므로 `sw up`이 거절해. 새 DB를 시험할 때는 `--config-dir`도 새 경로로 지정해.

다른 경로로 시작했다면 `down`과 `doctor`에도 같은 옵션을 사용해. `status`와 `tree`는 서버 주소(`--listen`)와 인증 경로(`--config-dir`)만 맞추면 돼. DB는 `/mnt/c`가 아닌 Linux 파일시스템에 두고, root용 운영 경로는 신뢰하는 디렉터리 아래에 지정해. 동시에 실제 Pod를 운영할 수 있는 인스턴스는 하나야. API만 시험할 때는 일반 사용자 소유의 Linux 디렉터리를 만들고 `--runtime=none`과 다른 로컬 포트로 별도 실행할 수 있어.

아래 예제는 일반 사용자로 실행해. API 객체를 저장하고 조회할 수 있고, Pod를 실행하거나 네트워크를 만들지는 않아.

```bash
swbin="$HOME/.cache/shearwater-dev/bin/sw"
instance=$(mktemp -d "$HOME/.cache/shearwater-dev/api.XXXXXXXX")
opts=(--runtime=none --listen=127.0.0.1:16443 --data-dir="$instance/data"
      --config-dir="$instance/etc" --run-dir="$instance/run" --log-dir="$instance/log")
"$swbin" up "${opts[@]}"
kubectl --kubeconfig "$instance/etc/kubeconfig" get namespaces
"$swbin" down "${opts[@]}"
```

`sw down --all`은 초기화나 삭제 명령이 아니야. 원래 데이터를 계속 쓸 수 있어. 데이터를 아예 폐기하는 일은 백업한 뒤 별도로 판단해.

## 문제가 생겼을 때

개발 버전의 `sw get`, `logs`, `events`, `why`를 사용하는 상세 예제·출력 해석은
[문제 해결 안내](troubleshooting.md)에 있어. alpha.2 실행 파일을 사용 중이라면
아래 kubectl 명령을 사용해. 조회용 새 CLI를 빌드하는 일과 서버 교체는 별개야.

1. `sw doctor`로 어떤 검사에서 막혔는지 확인해. `server.log`의 위치와 최근 오류가 출력돼.
2. `sw status`로 서버·노드를 보고 `sw tree --problems`로 앱의 문제가 어디에 있는지 좁혀.
3. `kubectl describe pod <이름>`의 Events에서 배치와 시작을 확인하고, `kubectl logs <이름>` 또는 `kubectl logs <이름> --previous`로 현재 또는 직전 컨테이너 로그를 확인해. 반복 이벤트는 묶이고 그 Pod가 삭제되면 정리돼.
4. 앱은 동작하지만 Service 연결만 실패하면 [Service 안내서의 확인 순서](service-networking.md#연결이-안-될-때-확인할-순서)를 따라가.

## 검증 재현

WSL 일반 사용자로 `. hack/env.sh` 뒤 `bash hack/t.sh ./...`를 실행하고 `go build`로 바이너리를 만들어. 아래 두 스크립트는 기존 Shearwater 워크로드가 없는 WSL에서 root로 **하나씩** 실행해.

```bash
bash hack/e2e-stage1.sh /absolute/sw /absolute/kubectl
bash hack/measure.sh /absolute/sw /absolute/kubectl
```

두 스크립트는 `/var/lib/shearwater/check/` 아래에 별도 데이터와 로그를 만들고, 검증용 인스턴스만 정지해. 출력의 `Evidence:` 경로를 보면 돼. 측정 스크립트는 처음 `sw up`부터 HTTPS `/readyz`까지 시간을 재고, Pod 10개가 Ready가 된 뒤 60초 동안 서버의 CPU·RSS·디스크 쓰기를 잰다. **RSS**는 그 프로세스가 현재 물리 메모리에 올린 페이지의 크기야. CPU는 논리 코어 하나를 100%로 보고, 파일 변경은 DB·WAL·서버 로그의 알림과 크기·수정 시각, `/proc/<pid>/io`의 쓰기 바이트를 함께 본다. 파일 알림은 합쳐질 수 있어서 쓰기 횟수로 환산하지 않아. [Linux 프로세스 통계](https://docs.kernel.org/filesystems/proc.html) · [inotify 설명](https://man7.org/linux/man-pages/man7/inotify.7.html)

## 코드를 따라가려면

| 파일 | 읽을 내용 |
|---|---|
| `cmd/sw/lifecycle.go` | 호스트 사전 검사 뒤 서버를 시작하는 순서, PID가 재사용됐을 때 종료를 거부하는 방법 |
| `cmd/sw/ops.go`, `cmd/sw/inspect.go`, `internal/agent/inspect.go` | CA로 서버를 확인한 뒤 읽기만 하고, 같은 리비전의 결과로 트리를 만드는 이유 |
| `cmd/sw/doctor.go`, `internal/runtime/host_linux.go` | DB 파일시스템·주소 범위 검사와 전용 네트워크 정리 범위 |
| `internal/node/pod.go`, `internal/objects/events.go` | 실행 중인 태스크의 인계, 종료 시각 복구, 상태가 바뀔 때만 진단 이벤트 기록 |
| `hack/e2e-stage1.sh`, `hack/measure.py` | 검증용 인스턴스의 생성부터 정리까지, 측정에 포함하는 값과 제외하는 값 |
