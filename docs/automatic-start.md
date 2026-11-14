# WSL에서 자동으로 시작하고 복구하기

이 문서는 alpha.4의 기능이야. 공개 alpha.3 실행 파일에는 `sw service`가 없어.
systemd는 Linux 프로그램을 시작하고 종료 상태를 지켜보는 서비스 관리자야.
Shearwater를 등록하면 WSL 배포판이 시작될 때 sw가 뜨고, sw 프로세스가 비정상 종료되면
다시 실행해. 살아 있는 앱 컨테이너는 기존 shim에서 인계하므로 sw만 죽었다고 앱을 재배포하지 않아.

## 시작 전에 확인할 것

- WSL 안에서 `systemctl status`가 동작해야 해. systemd가 없다면
  [Microsoft의 WSL 안내](https://learn.microsoft.com/en-us/windows/wsl/systemd/)를 먼저 따라 해.
- containerd와 필요한 네트워크 도구, IPv4 전달이 준비되어 있어야 해. `sw doctor`로 확인해.
- SQLite와 실행 파일은 Linux 파일시스템의 고정 경로에 둬. 빌드 임시 경로를 서비스에 등록하지 마.
- 지금 실행 중인 인스턴스의 `--data-dir`, `--config-dir`, `--run-dir`, `--log-dir`,
  `--containerd-socket`, `--node-name`, `--listen`을 기록해. 다른 옵션은 다른 인스턴스를 뜻할 수 있어.

systemd가 sw를 시작하는 것과 **Windows 로그인 때 WSL 자체를 켜는 것은 별개**야.
이 명령은 Windows 예약 작업이나 로그인 설정을 만들지 않아. WSL이 꺼져 있으면 서비스도 돌지 않아.
또한 systemd 서비스만으로 WSL 배포판이 계속 켜져 있는 것은 아니야.
마지막 WSL 세션을 닫으면 배포판이 종료될 수 있다는 것이
[Microsoft가 설명하는 WSL 동작](https://learn.microsoft.com/en-us/windows/wsl/systemd/)이야.
테스트할 때는 WSL 터미널을 유지하고, 상시 운영은 별도로 검토한 Windows 시작·유지 절차나
항상 실행 중인 Linux 호스트를 준비해야 해. 이번 기능은 WSL 자체의 수명 주기를 바꾸지 않아.

## 내용을 먼저 보고 설치하기

아래는 Ubuntu Bash 예시야. 실제 설치한 실행 파일과 기존 옵션으로 바꿔.

```bash
sw=/opt/shearwater/current/sw
opts=(--listen=127.0.0.1:6443 --data-dir=/var/lib/shearwater
      --config-dir=/etc/shearwater --run-dir=/run/shearwater
      --log-dir=/var/lib/shearwater/log --node-name=my-node
      --containerd-socket=/run/containerd/containerd.sock)
sudo "$sw" service render --binary="$sw" "${opts[@]}"
sudo "$sw" service install --binary="$sw" "${opts[@]}"
```

`render`는 서비스 파일 내용을 출력할 뿐 설치하지 않아. `install`은 새
`/etc/systemd/system/shearwater.service`를 쓰고 부팅 시 실행을 등록해.
기존 동명 파일이 있으면 덮어쓰지 않고 실패해. Docker/containerd 설정은 변경하지 않아.
기본 containerd 소켓을 사용할 때만 `containerd.service` 뒤에 실행되도록 의존성을 넣어.
사용자 지정 소켓이면 그 런타임의 자동 시작은 운영자가 별도로 준비해야 해.

서비스의 기본 파일 생성 마스크는 `0022`야. 컨테이너에 제공하는 일반 파일을 앱 사용자도
읽을 수 있게 하는 설정이고, 관리자 토큰·개인키·DB 디렉터리는 sw가 별도로 `0600`/`0700`을 지정해.
런타임까지 직접 띄우는 사용자 지정 wrapper에 `0077`을 일괄 적용하면 이미지 루트 디렉터리가
일반 앱 사용자에게 닫힐 수 있어. 이때 nginx 같은 앱이 실행되어도 HTTP 403이 날 수 있어.

설치만으로 기존 서버를 끄거나 새 서버를 바로 시작하지 않아. 수동 `sw up` 서버를
인계하려면 백업과 옵션을 확인한 다음 아래 순서로 **서버만** 교체해.

```bash
sudo "$sw" down "${opts[@]}"
sudo "$sw" service start
sudo "$sw" status
sudo "$sw" doctor "${opts[@]}"
sudo journalctl -u shearwater.service -n 50 --no-pager
```

서비스가 관리하는 서버를 멈출 때는 `sw service stop`을 써. 서비스 설치 전의
`sw up`과 서비스 실행을 동시에 관리하려고 하지 마. `down --all`은 앱까지 정리하는
다른 동작이므로 단순 서비스 인계나 버전 교체에 사용하지 않아.

## 문제가 생겼을 때

```bash
sudo "$sw" service status
sudo systemctl show shearwater.service -p MainPID -p NRestarts
sudo journalctl -u shearwater.service -n 100 --no-pager
```

`Restart=on-failure`라 정상 정지와 오류 종료를 구분해. 오류 종료 후 3초 뒤 재시도하고,
60초 동안 10번 시작에 실패하면 계속 반복하지 않아. 옵션·경로·런타임 오류를 고친 뒤
`systemctl reset-failed shearwater.service`와 `sw service start`로 다시 시작해.
`sw supervise`는 systemd가 실제 서버 PID를 추적하도록 foreground에서 실행하는 내부 진입점이야.

자동 시작을 해제해도 데이터와 서비스 파일은 보존돼.

```bash
sudo "$sw" service stop
sudo "$sw" service disable
```

서비스 이름을 바꿨다면 모든 명령에 같은 `--name=shearwater-이름`을 붙여.
서비스 파일과 Windows/WSL 설정을 삭제하는 자동 명령은 제공하지 않아.

## 검증 기준

1. 서버 PID를 강제 종료했을 때 새 PID로 자동 복구하는지 확인해.
2. 그동안 기존 앱의 컨테이너 ID·PID·재시작 수와 HTTP 응답을 비교해.
3. 별도 QA 배포판을 실제 종료한 뒤 다시 켜서 수동 `sw up` 없이 복구되는지 확인해.
4. 재부팅은 앱 프로세스도 종료하므로 이 경우의 재시작 수 증가는 정상일 수 있어.
5. 같은 Pod UID/IP·Service IP·인증·앱 데이터가 유지되는지도 함께 확인해.

alpha.4까지는 Service TCP 프록시가 sw 안에 있어 서버 재시작 때 연결도 닫혔어.
alpha.5는 커널에 Service 전달 규칙을 남겨, 같은 호스트에서 sw만 재시작할 때
살아 있는 앱으로 향하는 연결을 유지해. API·상태 검사·자동 복구는 sw가 내려간 동안 멈춰.
호스트/WSL 재부팅이나 앱 종료는 별개야. 자동 시작 설정만으로 모든 요청의 무중단을
보장하지 않으므로 [연결 유지 범위와 검증 방법](connection-continuity.md)을 함께 확인해.
