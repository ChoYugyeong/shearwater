# Shearwater v0.1.0-alpha.5 설치와 첫 웹 앱

이 안내는 **WSL 2 / Ubuntu 24.04 / x86_64(amd64)**에서 처음 실행하는 사람을 위한 거야. 내려받은 바이너리를 사용하므로 **Go 설치나 소스 빌드는 필요 없어**. ARM64, Windows 네이티브 실행 파일, 다른 Linux 배포판은 이번 바이너리의 검증 대상이 아니야.

Shearwater는 신뢰하는 로컬 관리자가 사용하는 **실험적 알파**야. 단일 노드·IPv4·TCP ClusterIP를 지원해. DNS·RBAC·NetworkPolicy는 없고, securityContext는 저장만 해. 설정된 프로브는 실행하고 startup/readiness 결과를 Ready에 반영해. 프로브 없는 앱의 실제 응답은 아래 curl로 확인해.

무엇을 확인할지 먼저 정하고 싶다면 [첫 체험 안내](first-try.md)를 봐.
아래는 새 설치 절차야. 이미 실행 중인 인스턴스가 있다면 **기존 버전 업데이트** 단락을 먼저 읽어.

## 1. WSL과 준비물 확인

Windows PowerShell에서 `wsl --list --verbose`로 배포판을 확인해.
`NAME` 열의 이름을 아래 `$distro`에 그대로 써. 기본 설치는 `Ubuntu`일 수 있고,
`Ubuntu-24.04`는 예시 이름이야. 이름을 맞춘 뒤 Ubuntu에 들어가.

```powershell
wsl --list --verbose
$distro = 'Ubuntu-24.04'  # NAME 열의 실제 이름으로 바꿔
wsl -d $distro
```

**이후 명령은 Ubuntu의 Bash 터미널에서 실행해.**

```bash
uname -m                         # x86_64가 나와야 함
cat /etc/os-release              # Ubuntu 24.04 계열 확인
findmnt -n -o FSTYPE /sys/fs/cgroup  # cgroup2
cat /proc/sys/net/ipv4/ip_forward    # 1
for tool in containerd runc iptables iptables-restore ip curl sha256sum tar; do
  if command -v "$tool" >/dev/null; then
    printf '있음: %s\n' "$tool"
  else
    printf '없음: %s\n' "$tool"
  fi
done
```

한 네트워크 공간에서는 Shearwater 인스턴스를 하나만 띄워. WSL 배포판 이름이 달라도 호스트 네트워크를 공유할 수 있어. 기존 `sw-br0`, `sw-svc0`나 실행 중인 Shearwater 앱이 있다면 이 새 설치 예제를 겹쳐 실행하지 마. SQLite 데이터는 기본 `/var/lib/shearwater`의 WSL ext4에 두고 `/mnt/c`로 바꾸지 마.

아무 컨테이너 런타임도 없는 **새 전용 Ubuntu**라면 다음으로 필요한 패키지를 준비할 수 있어. 기존 Docker가 제공하는 containerd 위에 다른 패키지를 덮어쓰는 절차로 사용하지 마.

```bash
sudo apt-get update
sudo apt-get install --no-install-recommends ca-certificates curl iproute2 iptables containerd runc
sudo systemctl start containerd
```

마지막 명령은 systemd 환경용이야. systemd가 없는 환경에서는 별도 containerd 운영 절차가 필요해. 기존 런타임을 쓰는 경우에는 설정 파일을 바꾸지 말고 그 소켓을 사용해. 기본 소켓은 `/run/containerd/containerd.sock`야.

IPv4 전달 값이 0이고 이 호스트를 Pod 네트워크용으로 쓰기로 결정했다면 `sudo sysctl -w net.ipv4.ip_forward=1`로 현재 부팅 동안 켤 수 있어. 호스트 공통 설정이므로 다른 앱과 공유하는 환경에서는 영향을 먼저 확인해. 재부팅 후에는 값과 containerd 실행 상태를 다시 확인해야 해.

## 2. 다운로드·체크섬 확인·설치

아래 블록은 새 버전 폴더에만 설치하고, 기존 폴더가 있으면 멈춰. 실패하면 다음 단계로 넘어가지 마. 다운로드한 파일을 셸로 바로 실행하지 않고, 먼저 SHA-256을 확인한 뒤 압축을 풀어.

```bash
(
  set -euo pipefail
  version=v0.1.0-alpha.5
  base="https://github.com/ChoYugyeong/shearwater/releases/download/$version"
  archive="shearwater-${version}-linux-amd64.tar.gz"
  install_dir="$HOME/.local/share/shearwater/$version"
  [ "$(uname -m)" = x86_64 ] || { echo '이 파일은 Linux x86_64용이야.'; exit 1; }
  if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
    echo "이미 존재해: $install_dir — 기존 설치를 덮어쓰지 않아."
    exit 1
  fi
  download_dir=$(mktemp -d /tmp/shearwater-download.XXXXXXXX)
  curl --fail --location "$base/$archive" -o "$download_dir/$archive"
  curl --fail --location "$base/SHA256SUMS" -o "$download_dir/SHA256SUMS"
  (cd "$download_dir" && sha256sum --check --strict --ignore-missing SHA256SUMS)
  mkdir -p "$install_dir"
  tar -xzf "$download_dir/$archive" --strip-components=1 -C "$install_dir"
  (cd "$install_dir" && sha256sum --check --strict sw.sha256)
  "$install_dir/sw" version
  printf '설치 위치: %s\n다운로드 보존 위치: %s\n' "$install_dir" "$download_dir"
)
```

`...tar.gz: OK`와 `sw: OK`가 나와야 해. 해시가 다르면 실행하지 말고 다운로드 상태를 확인해. SHA256SUMS는 같은 릴리스의 파일 무결성을 확인하는 자료이며 별도의 디지털 서명은 아니야.

묶음에는 `sw`, `sw.sha256`, 이 안내(`INSTALL-ko.md`), 진단 안내(`TROUBLESHOOTING-ko.md`), 새 기능 안내 4개, `BUILDINFO.txt`, `examples/web.yaml`, `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`, `LICENSES/`가 들어 있어. 이미지·containerd·kubectl은 포함하지 않아. 바이너리를 다시 배포할 때 라이선스 고지도 함께 전달해.

**버전 확인:** 다음처럼 실제 릴리스 버전이 나와야 해. 다른 값이 나오면 실행 파일 경로를 먼저 확인해. 동봉 `BUILDINFO.txt`에는 공개 소스 커밋과 빌드 정보, 파일 체크섬도 있어.

```bash
"$HOME/.local/share/shearwater/v0.1.0-alpha.5/sw" version
# sw 0.1.0-alpha.5 (Kubernetes API v1.36.0+shearwater.0.1.0-alpha.5)
```

앞의 숫자는 Shearwater 제품 버전이고, `v1.36.0`은 Kubernetes API 호환 수준이야. 처음 배포한 alpha.1은 `sw 0.0.1`을 표시했지만 alpha.2부터 이 불일치를 고쳤어.

### 기존 alpha.1~alpha.4를 쓰고 있다면

위 다운로드는 새 버전 폴더에 설치하므로 기존 파일을 보존해. **기존 서버를 켜 둔 채 4절의 새 서버를 시작하면 안 돼.** 먼저 현재 서버의 실행 파일 경로와 `--data-dir`, `--config-dir`, `--run-dir`, `--log-dir`, `--containerd-socket`, `--node-name`, `--listen` 값을 기록해. 기본값을 썼는지 기억이 안 나면 기존 실행 절차부터 확인해.

교체 전에 SQLite의 일관된 백업과 인증·볼륨 등 앱 데이터의 복구 자료를 확보해.
실행 중인 SQLite의 DB 파일만 복사하면 WAL의 최근 변경을 놓칠 수 있으므로 SQLite backup API 같은 일관된 백업 방식을 사용해.
기존 실행 파일도 복구용으로 보존해. 다만 alpha.5에서 alpha.4 이하로 돌아갈 때는 새 커널 Service 규칙까지 원복해야 하므로 파일 교체만 역순으로 적용하는 절차는 충분하지 않아. `down --all`로 앱을 지워서 해결하지 마.

alpha.3에서 저장만 하던 **기존 프로브도 alpha.4에서 실행돼**. `sw get pods --full`과 `sw get deployments --full`로 설정을 읽고 실제 경로·포트·명령이 맞는지 먼저 확인해. 프로브가 있는 앱은 별도 QA에서 실행해 본 뒤 교체해. 구버전으로 실행 파일을 되돌리면 프로브 실행도 사라져.

기존 실행 파일로 `sw down`을 실행해 서버만 멈춘 뒤, 새 실행 파일로 **같은 경로·소켓·노드 이름·주소**를 전달해 `sw up` 해. 이때 `down --all`은 사용하지 않아. 이번 버전은 DB 형식을 바꾸지 않았고 실행 중인 컨테이너를 인계해. 이어서 `sw status`, `sw doctor`, 앱의 HTTP 응답과 로그를 확인해. `sw version`은 로컬 파일의 버전이고, `kubectl --kubeconfig /실제/kubeconfig get --raw /version`은 서버 버전이므로 둘 다 확인해.

**alpha.4 이하에서 처음 교체할 때는** 기존 프로세스가 소유한 TCP 소켓이 닫혀 요청이 실패할 수 있어. alpha.5로 교체한 뒤 같은 호스트에서 sw만 재시작하면 커널 Service 전달은 유지돼. API·상태 검사·자동 복구는 서버 중단 동안 멈춰. 컨테이너가 유지되는 것과
모든 요청의 무중단은 같은 의미가 아니야. `get pods`에서 UID·IP·재시작 수를 비교하고,
앱의 저장 데이터도 앱에 맞는 방법으로 확인해.

## 3. kubectl 준비

`kubectl`은 Shearwater API에 명령을 보내는 도구야. 이미 있다면 `kubectl version --client`로 확인해. 검증한 Linux 버전은 v1.36.6이야. 없을 때만 다음 블록으로 사용자 폴더의 새 위치에 받아.

```bash
(
  set -euo pipefail
  tool_dir="$HOME/.local/share/shearwater-tools/kubectl-v1.36.6"
  if [ -e "$tool_dir" ] || [ -L "$tool_dir" ]; then
    echo "이미 존재해: $tool_dir"
    exit 1
  fi
  mkdir -p "$tool_dir"
  curl --fail --location https://dl.k8s.io/release/v1.36.6/bin/linux/amd64/kubectl -o "$tool_dir/kubectl"
  curl --fail --location https://dl.k8s.io/release/v1.36.6/bin/linux/amd64/kubectl.sha256 -o "$tool_dir/kubectl.sha256"
  (cd "$tool_dir" && printf '%s  kubectl\n' "$(cat kubectl.sha256)" | sha256sum --check --strict)
  chmod 755 "$tool_dir/kubectl"
)
export PATH="$HOME/.local/share/shearwater-tools/kubectl-v1.36.6:$PATH"
kubectl version --client
```

PATH는 현재 터미널에만 적용돼. 새 터미널에서 이 kubectl을 쓰려면 위 `export PATH=...` 줄을 다시 실행해.

## 실행 파일 경로를 기억하기

이 안내는 `sw`를 시스템 PATH에 등록하거나 `/opt/shearwater/current/sw`를 만들지 않아.
따라서 그냥 `sw version`이 안 된다고 설치 실패는 아니야. 새 Ubuntu 터미널에서는 아래처럼
설치된 파일을 지정해. 다른 곳에 설치했다면 실제 경로로 바꿔.

```bash
sw="$HOME/.local/share/shearwater/v0.1.0-alpha.5/sw"
test -x "$sw" && "$sw" version
printf 'MCP에 지정할 Linux 절대 경로: %s\n' "$sw"
```

MCP 설정에도 이 **Linux 절대 경로**를 넣어. Windows의 `C:\...` 경로나 `$HOME`이라는
문자열을 그대로 넣는 것은 같은 뜻이 아니야. [AI 연결 안내](agent-mcp.md)에 Windows 명령이 있어.

## 4. 서버 시작과 웹 앱 배포

다음 변수와 함수도 현재 터미널에서만 유지돼. 새 터미널에서 작업을 이어갈 때 다시 선언해.

```bash
sw="$HOME/.local/share/shearwater/v0.1.0-alpha.5/sw"
example="$HOME/.local/share/shearwater/v0.1.0-alpha.5/examples/web.yaml"
export PATH="$HOME/.local/share/shearwater-tools/kubectl-v1.36.6:$PATH"
kubectl_bin="$(command -v kubectl)"
k() {
  [ -n "$kubectl_bin" ] || { echo 'kubectl을 못 찾았어. 설치 안내 3절부터 확인해.' >&2; return 1; }
  sudo "$kubectl_bin" --kubeconfig /etc/shearwater/kubeconfig "$@"
}

sudo "$sw" doctor --node-name=shearwater
sudo "$sw" up --node-name=shearwater
k get nodes
k apply -f "$example"
k rollout status deployment/web --timeout=120s
k get pods,service,endpointslices -o wide
```

처음 doctor는 아직 서버가 없다는 경고를 낼 수 있어. 그 메시지와 containerd·cgroup·네트워크 준비물의 FAIL을 구분해. 준비물 오류를 먼저 해결한 뒤 up을 실행해. 예제의 node-name을 고정해 호스트 이름의 대문자 같은 차이를 피했어. 이후 up/down/doctor에도 같은 이름을 써.

`sudo`는 노드 실행과 네트워크 준비에 필요한 관리자 권한을 사용해. 기본 kubeconfig는 `/etc/shearwater/kubeconfig`에 있고 관리자 토큰이 들어 있어. 위처럼 정확한 경로를 지정하면 다른 Kubernetes 클러스터를 실수로 조작하는 일을 피할 수 있어. kubeconfig나 토큰은 공유하지 마.

소켓이 기본값과 다르면 up/down/doctor 명령마다 `--containerd-socket=/실제/소켓`을 **똑같이** 추가해. 일반 status/tree는 API 조회 명령이므로 node-name이나 containerd 옵션을 받지 않아.

## 5. HTTP 응답과 로그 확인

```bash
service_ip=$(k get service web -o jsonpath='{.spec.clusterIP}')
curl --noproxy '*' --fail --retry 10 --retry-connrefused --retry-delay 1 "http://$service_ip/"
k logs deployment/web --tail=20
sudo "$sw" status
sudo "$sw" tree
```

페이지에 `Welcome to nginx!`, 로그에 `GET / HTTP`가 나오면 성공이야. 예제의 Deployment가 Pod 한 개를 유지하고, Service는 `app: web` 라벨의 Pod로 TCP 연결을 전달해. ClusterIP는 내부 주소이며 위 curl은 WSL에서 실행해. Windows 브라우저 접속이나 `http://web/` DNS 이름, `kubectl port-forward`는 이 예제에서 제공하지 않아.

### 새 진단 명령 네 개

`sw`만으로도 목록·앱 로그·이벤트·실패 이유를 조회할 수 있어. 다음 `pod` 변수는
위 nginx 예제의 실제 Pod 이름을 읽는 거야. 운영 중인 다른 앱에서는 그 이름과
네임스페이스(`-n`)로 바꿔. 이 네 명령은 서버를 변경하지 않아.

```bash
sudo "$sw" get pods -n default -l app=web
pod=$(k get pods -l app=web -o jsonpath='{.items[0].metadata.name}')
sudo "$sw" logs "pod/$pod" -n default --tail=20
sudo "$sw" events --for="pod/$pod" -n default
sudo "$sw" why "pod/$pod" -n default
```

`logs`는 기본 마지막 50줄/64 KiB를 한 번 읽고 끝나며 `--previous`로 직전 실행 로그를
읽을 수 있어. 컨테이너가 여러 개면 `-c NAME`을 지정해. `why`가 성공해도 앱 정상
판정은 아니야. 상태·이벤트·제한된 로그를 잘 수집했다는 뜻이고 HTTP 응답은 따로 확인해.
자세한 출력 해석과 오류 해결은 동봉 `TROUBLESHOOTING-ko.md` 또는
[온라인 진단 안내](https://github.com/ChoYugyeong/shearwater/blob/v0.1.0-alpha.5/docs/troubleshooting.md)를 봐.

## 6. 재시작과 종료

서버만 잠깐 정지했다 시작하면 기존 앱 컨테이너를 이어받아. 이 동안 API와 Service 프록시는 멈추지만 앱 컨테이너는 유지될 수 있어.

```bash
sudo "$sw" down --node-name=shearwater
sudo "$sw" up --node-name=shearwater
k get pods -o wide
service_ip=$(k get service web -o jsonpath='{.spec.clusterIP}')
curl --noproxy '*' --fail --retry 10 --retry-connrefused --retry-delay 1 "http://$service_ip/"
```

예제를 마치려면:

```bash
k delete -f "$example" --wait=true --timeout=60s
sudo "$sw" down --all --node-name=shearwater
```

`down --all`은 이 Shearwater가 관리하는 앱과 전용 네트워크도 내려. 다른 앱을 추가했다면 그것도 중단돼. SQLite·인증서·볼륨·로그를 지우지는 않아. 설치 디렉터리와 데이터를 자동 삭제하거나 OS 부팅 시 자동 시작하도록 설정하지 않아.

실제 WSL 재부팅 후에는 containerd·IPv4 전달을 확인하고 같은 경로와 옵션으로 up을 실행해. 처음부터 다른 데이터 경로를 쓰면 기존 상태를 이어받는 복구가 아니야.

## 7. 막혔을 때

| 증상 | 먼저 확인할 것 |
|---|---|
| 실행 형식 오류 | `uname -m`이 x86_64인지, Windows가 아니라 WSL Bash에서 실행했는지 |
| containerd 연결 실패 | 소켓 경로, containerd 실행 여부, 같은 옵션을 계속 사용했는지 |
| IPv4 전달·대역 충돌 | doctor 결과와 기존 WSL/컨테이너 네트워크. 기존 장치나 규칙을 임의로 지우지 마 |
| ImagePullBackOff | `k describe pod <이름>`의 메시지와 이미지 저장소 연결 |
| Ready인데 HTTP 실패 | 실제 앱 준비 시간, Service selector, EndpointSlice와 로그 |
| 다른 터미널에서 명령 실패 | sw/example/kubectl_bin 변수, k 함수, PATH를 다시 선언했는지 |

자세한 [호환 범위](https://github.com/ChoYugyeong/shearwater/blob/v0.1.0-alpha.5/docs/compatibility.md), [운영 안내](https://github.com/ChoYugyeong/shearwater/blob/v0.1.0-alpha.5/docs/operations.md), [보안 제보](https://github.com/ChoYugyeong/shearwater/security/advisories/new)를 참고해.

## 8. alpha.4의 새 운영 기능

자동 시작·복구는 `sw service`, 백업·새 경로 복원은 `sw backup/restore`, AI 연결은 `sw mcp`야. 기본 MCP는 조회 전용이고 변경은 계획을 검토한 뒤 로컬에서 정확한 해시로 승인해야 해. 실행 파일 교체만으로 서비스 설치나 Windows 로그인 설정을 만들지는 않아.

동봉된 `AUTOMATIC-START-ko.md`, `BACKUP-RESTORE-ko.md`, `HEALTH-PROBES-ko.md`, `AGENT-MCP-ko.md`에서 예제와 제한을 읽어. `--include-volumes`는 앱을 중지한 상태에서만 사용하고, 온라인 백업만으로 앱 볼륨까지 보호됐다고 판단하지 마.

## 9. alpha.5의 연결 유지

롤링 교체에서는 새 연결 대상에서 이전 Pod를 제외하고 기존 Service 연결을 최대 30초 기다려. 그 다음 원래 Pod 종료 유예를 시작해. 30초보다 긴 연결, Pod 직접 IP, 강제 삭제, 앱/호스트/WSL 재부팅은 기존 연결 유지 보장 대상이 아니야. [원리와 진단·재현](connection-continuity.md), [개발 단계 실측](connection-verification.md), [릴리스 기록](releases.md)을 함께 봐.
