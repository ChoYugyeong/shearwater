# 처음 설치해서 웹 페이지와 로그 보기

목표는 저장소에 들어 있는 nginx 예제를 실행하고, IP로 접속하고, 로그를 읽고, 서버를 다시 시작해 같은 앱이 이어지는 것까지 확인하는 거야. Windows PowerShell과 WSL Bash는 다른 셸이므로 코드 블록의 위치를 구분해 줘.

## 1. 사용할 환경 고르기

검증 대상은 WSL 2의 Ubuntu 24.04, amd64야. Windows에서 `wsl --list --verbose`로 배포판 이름과 버전을 확인하고 `wsl -d Ubuntu-24.04`로 들어가. 아래부터는 WSL Bash 명령이야. 저장소 소스를 내려받아 압축을 풀거나 체크아웃한 뒤 그 폴더에서 실행해. 소스 폴더 이름이 달라도 제품명은 Shearwater야.

**한 네트워크 공간에서 Shearwater는 하나만 실행해.** WSL 배포판이 서로 다른 디스크를 써도 호스트 네트워크를 공유할 수 있어. 이미 `sw-br0`나 `sw-svc0`, 기존 Shearwater 앱이 있다면 이 안내의 새 인스턴스를 겹쳐 띄우지 말고 기존 운영 안내를 사용해. API 전용 실험은 [운영 안내](operations.md)의 `--runtime=none`을 사용해.

SQLite는 기본 `/var/lib/shearwater`에 생성돼. 이 경로는 WSL ext4에 두고 `/mnt/c`로 바꾸지 마. 소스 파일은 Windows 쪽 디렉터리에 있어도 괜찮아.

## 2. 준비물 확인하기

| 준비물 | 확인 명령 | 용도 |
|---|---|---|
| Go 1.27.3 | `go version` | 소스에서 sw 빌드 |
| kubectl v1.36.6 | `kubectl version --client` | API 조작과 로그 조회 |
| containerd와 runc | `containerd --version`, `runc --version` | 컨테이너 실행 |
| iptables와 iproute2 | `iptables --version`, `ip -V` | 전달 규칙과 진단 |
| cgroup v2 | `findmnt -n -o FSTYPE /sys/fs/cgroup` | `cgroup2`가 나와야 함 |
| IPv4 전달 | `cat /proc/sys/net/ipv4/ip_forward` | `1`이어야 함 |

기존 containerd를 쓰는 환경에서는 설정 파일을 바꾸거나 Docker를 재설치할 필요가 없어. 기본 소켓은 `/run/containerd/containerd.sock`야. 소켓이 다르면 `sw up --containerd-socket=/실제/소켓`을 쓰고 이후 `down`·`doctor`에도 같은 옵션을 줘.

아무 런타임도 없는 **새 전용 Ubuntu**에서만 다음 패키지 설치를 사용해. 기존 Docker가 제공하는 containerd 위에 다른 패키지를 덮어쓰는 명령으로 사용하지 마.

```bash
sudo apt-get update
sudo apt-get install --no-install-recommends ca-certificates curl git python3 iproute2 iptables containerd runc
```

systemd를 사용하는 새 Ubuntu에서는 `sudo systemctl start containerd`로 시작할 수 있어. systemd가 없는 경우에는 containerd를 별도로 실행하는 운영 절차가 필요해. Shearwater 자체가 containerd를 설치하거나 자동 실행하지는 않아. 패키지 버전은 저장소 시점마다 달라지므로 검증 기록에 실제 버전을 남겨.

IPv4 전달이 꺼져 있다면 Pod가 밖으로 나갈 수 없어. `sw doctor`도 이를 알려 줘. 이 호스트를 Pod 네트워크용으로 쓰기로 결정한 운영자는 `sudo sysctl -w net.ipv4.ip_forward=1`로 현재 부팅 동안 켤 수 있어. 이는 호스트 공통 설정이므로 Shearwater가 몰래 변경하지 않아. 재부팅 뒤 값이 다시 꺼졌다면 복구 때 다시 확인해.

Go와 kubectl이 없으면 [Go 공식 배포](https://go.dev/dl/)와 [kubectl 공식 설치 안내](https://kubernetes.io/docs/tasks/tools/install-kubectl-linux/)에서 아래의 검증 버전을 준비할 수 있어. 사용자 폴더의 **새 디렉터리**에만 풀고 기존 설치를 덮어쓰지 않는 예제야.

```bash
tools="$HOME/.local/share/shearwater-tools-1.27.3"
test ! -e "$tools" || { echo '이미 존재하는 도구 폴더야. 기존 설치를 확인해.'; exit 1; }
mkdir -p "$tools/bin" || exit 1
curl -fL https://go.dev/dl/go1.27.3.linux-amd64.tar.gz -o "$tools/go.tar.gz" || exit 1
echo '65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40  '"$tools/go.tar.gz" | sha256sum -c - || exit 1
tar -xzf "$tools/go.tar.gz" -C "$tools" || exit 1
curl -fL https://dl.k8s.io/release/v1.36.6/bin/linux/amd64/kubectl -o "$tools/bin/kubectl" || exit 1
curl -fL https://dl.k8s.io/release/v1.36.6/bin/linux/amd64/kubectl.sha256 -o "$tools/kubectl.sha256" || exit 1
echo "$(cat "$tools/kubectl.sha256")  $tools/bin/kubectl" | sha256sum -c - || exit 1
chmod 755 "$tools/bin/kubectl" || exit 1
export PATH="$tools/go/bin:$tools/bin:$PATH"
```

다운로드나 체크섬 검사가 실패하면 그 터미널 작업을 중단하도록 했어. 원인을 확인한 뒤 다시 시작해. 이 PATH 설정은 현재 터미널에만 적용돼. 새 터미널에서는 다음 두 줄로 이미 설치한 도구를 선택하거나 본인의 셸 설정에 경로를 등록해.

```bash
tools="$HOME/.local/share/shearwater-tools-1.27.3"
export PATH="$tools/go/bin:$tools/bin:$PATH"
```

ARM64 바이너리를 받거나 다른 Go/kubectl 버전을 쓰는 경우에는 여기 기록한 검증 범위를 벗어나므로 별도 확인이 필요해.

## 3. 빌드하고 시작하기

저장소 루트에서 일반 사용자로 빌드해.

```bash
. hack/env.sh
bash hack/t.sh ./...
bash hack/build.sh
sw="$PWD/dist/sw"
kubectl_bin="$(command -v kubectl)"
sudo "$sw" doctor
sudo "$sw" up
```

처음의 doctor는 아직 서버가 없다는 경고를 낼 수 있어. containerd·네트워크 준비물의 FAIL부터 해결해. `up`이 성공하면 서버 주소와 kubeconfig 위치를 출력해. 실행 파일 하나가 API, 조정 루프, 노드 에이전트와 Service 프록시를 담당해. containerd는 이미 실행 중인 별도 프로세스야.

설정에는 관리자 토큰이 들어 있어서 기본 경로는 root 소유야. 예제에서는 모든 kubectl 호출에 정확한 설정 경로를 넣어 다른 Kubernetes 클러스터를 실수로 조작하지 않게 해.

```bash
k() { sudo "$kubectl_bin" --kubeconfig /etc/shearwater/kubeconfig "$@"; }
k get nodes
k apply -f examples/web.yaml
k rollout status deployment/web --timeout=120s
k get pods,service,endpointslices -o wide
```

처음 이미지를 받는 시간은 네트워크에 따라 달라져. `ImagePullBackOff`가 보이면 `k describe pod <이름>`으로 메시지를 확인해. `Ready`가 되어도 nginx가 HTTP를 받기까지 잠깐의 간격이 있을 수 있어. 프로브는 아직 실행하지 않으므로 다음 HTTP 검사가 실제 서비스 확인 단계야.

## 4. IP로 접속하고 로그 읽기

```bash
service_ip=$(k get service web -o jsonpath='{.spec.clusterIP}')
curl --noproxy '*' --fail --retry 10 --retry-connrefused --retry-delay 1 "http://$service_ip/"
k logs deployment/web --tail=20
sudo "$sw" status
sudo "$sw" tree
```

HTML에 `Welcome to nginx!`, 로그에 `GET / HTTP`가 나오면 Service → Pod → nginx 응답과 로그 경로를 확인한 거야. ClusterIP는 클러스터 내부 주소야. Windows 브라우저에서 그 IP로 곧바로 접속되는 것까지 보장하지 않아. 이 예제의 `curl`은 WSL에서 실행해. Service DNS가 없으므로 `http://web/` 대신 조회한 IP를 사용해.

`examples/web.yaml`의 Deployment는 한 개의 Pod를 유지해. Service는 `app: web` 라벨을 가진 준비된 Pod를 찾아 연결해. Pod 이름·IP와 Service IP는 역할이 다르고, Service는 앱의 복제본을 만들지 않아. 이름 포트 `http`는 Pod에 적힌 숫자 80으로 해석돼.

## 5. 서버를 다시 시작해 보기

```bash
k get pods -o wide
sudo "$sw" down
sudo "$sw" up
k get pods -o wide
curl --noproxy '*' --fail "http://$service_ip/"
```

서버만 정지하면 containerd의 앱은 유지되고 Service 프록시와 API는 멈춰. 다시 시작하면 기존 태스크를 이어받으므로 이 경우 Pod의 RESTARTS는 증가하지 않아. 실제 WSL 재부팅은 앱 프로세스와 네트워크도 사라지는 경우야. 부팅 후 containerd·IPv4 전달을 준비하고 **같은 경로/옵션으로** `sw up`을 실행하면 저장된 Pod UID·IP·Service IP를 복구하고 재시작 정책에 따라 앱을 다시 띄워. OS 자동 시작 설정은 제공하지 않아.

재부팅 실험은 다른 WSL 앱에도 영향을 줄 수 있으므로 별도 계획과 명시한 배포판에서만 진행해. [실제 측정 기록](reboot-soak.md)은 배포판 종료와 WSL VM 종료를 구분해서 적었어.

## 6. 예제를 마치기

```bash
k delete -f examples/web.yaml --wait=true --timeout=60s
sudo "$sw" down --all
```

첫 줄은 예제의 Deployment와 Service를 삭제하고 소유 Pod가 유예 종료되는 과정을 기다려. 둘째 줄은 해당 Shearwater가 관리하는 나머지 컨테이너와 전용 네트워크를 내리는 명령이므로, 다른 앱을 이 인스턴스에 추가했다면 그것도 중단된다는 점을 확인해. SQLite·인증서·볼륨·로그를 지우는 명령은 아니야.

문제가 생기면 `doctor` → `tree --problems` → `describe pod` → `logs` 순서로 확인해. 버그를 제보할 때는 [기여 안내](../CONTRIBUTING.md)대로 버전과 최소 재현 절차를 적고 관리자 인증 자료는 빼 줘.
