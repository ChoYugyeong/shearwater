# WSL에서 자동으로 시작하고 복구하기

`sw service`는 alpha.4에서 추가됐어. 공개 alpha.3 실행 파일에는 이 명령이 없어.
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
Linux 서비스만 테스트할 때는 WSL 터미널을 유지해. Windows에서도 자동 시작하려면
이 문서 아래의 Windows 감독 프로그램과 예약 작업을 함께 준비해. `sw service`
명령 자체는 Windows 설정을 바꾸지 않아.

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

## Windows 로그인부터 중계까지 자동으로 시작하기

Windows 보조 스크립트는 `sw` 바이너리와 별도야. alpha.6 릴리스의
`shearwater-v0.1.0-alpha.6-windows-forward.zip`에 스크립트와 이 안내를 동봉해.
Linux 압축에도 같은 `hack/` 보조 파일을 넣었어. 내려받기만 하면 자동 설치되지는 않아.
Windows와 WSL에 Python
3.11 이상이 이미 있고, WSL 2 NAT·eth0 단일 IPv4 환경인 경우를 대상으로 해.
추가 Python 패키지는 필요 없어. 먼저 [수동 중계](windows-localhost.md)로 별도
포트에서 접속을 확인한 뒤 자동 시작을 설정해.

자동 시작에는 서로 다른 세 역할이 있어.

```text
현재 Windows 사용자 로그인
  └─ 예약 작업 → pythonw.exe → Windows 감독 프로그램
       └─ wsl.exe → Linux TCP 중계 → 기존 Service → 앱
                        WSL 부팅
                          └─ systemd → sw supervise → 앱 상태 복구
```

| 구성 요소 | 책임 |
| --- | --- |
| Windows 예약 작업 | 현재 사용자가 로그인하면 감독 프로그램을 한 번 실행해. 관리자 권한이나 저장된 암호를 요구하지 않아. |
| Windows 감독 프로그램 | 중계가 끝나면 WSL과 Linux 중계를 다시 실행하고 현재 WSL IP를 읽어. Windows localhost 주소는 그대로 유지해. |
| Linux systemd 서비스 | WSL이 부팅될 때 같은 sw 바이너리와 데이터 경로로 서버를 시작해. |

감독 프로그램은 실패 후 2·4·8·16·30초 순서로 기다려. 이후에도 최대 30초
간격으로 재시도하고, 60초 이상 유지된 중계가 끝나면 대기 간격을 다시 2초부터
시작해. 실패한 시도의 자식 프로세스와 연결을 정리한 뒤 다음 시도를 실행해.
앱의 HTTP 요청은 재전송하지 않아. 쓰기 요청이 두 번 처리되는 것을 피하려는 거야.

예약 작업에는 `IgnoreNew`, 감독 프로그램에는 운영체제 파일 잠금을 사용해
중복 실행을 막아. 이전 실행의 PID를 믿고 종료하지 않고, 현재 실행만 가진 임의
ID로 종료 요청을 전달해. 잠금 파일은 지우지 마. 프로세스가 끝나면 잠금 자체는
운영체제가 풀어 줘.

### 1. 고정 경로에 파일 준비하기

먼저 Windows PowerShell에서 ZIP과 체크섬을 받아 확인해. 아래는 새 다운로드
폴더만 만들고, 기존 파일은 덮어쓰지 않아. 다운로드한 코드를 바로 실행하지 않아.

```powershell
$version = 'v0.1.0-alpha.6'
$ErrorActionPreference = 'Stop'
$archive = "shearwater-$version-windows-forward.zip"
$download = Join-Path $env:TEMP ('shearwater-alpha6-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $download | Out-Null
$base = "https://github.com/ChoYugyeong/shearwater/releases/download/$version"
Invoke-WebRequest "$base/$archive" -OutFile "$download\$archive"
Invoke-WebRequest "$base/SHA256SUMS" -OutFile "$download\SHA256SUMS"
$lines = @(Get-Content "$download\SHA256SUMS" | Where-Object { $_ -match ('^[0-9a-f]{64}  ' + [regex]::Escape($archive) + '$') })
if ($lines.Count -ne 1) { throw '체크섬 항목을 찾을 수 없어.' }
if ((Get-FileHash "$download\$archive" -Algorithm SHA256).Hash -ine $lines[0].Substring(0,64)) { throw '체크섬 불일치' }
Expand-Archive -LiteralPath "$download\$archive" -DestinationPath $download
$repo = Join-Path $download "shearwater-$version-windows-forward"
```

`$repo`는 이 ZIP을 푼 폴더야. 그 아래 `hack`에 스크립트 3개가 있어. 소스
저장소를 이미 받았다면 `$repo = 'C:\Dev\shearwater'`처럼 그 경로로 지정해도 돼.
이후 실행 패키지는 고정 경로
`C:\Dev\shearwater-runtime\forward\v1`에 복사해. 설치 위치가 다르면 모든 경로를
같이 바꿔. 이미 설치했다면 이 준비 단계를 반복하지 말고 아래 상태 확인으로 가.
버전별 폴더를 사용하는 이유는 실행 중인 파일을 덮어쓰지 않고 교체·복귀할 수
있게 하려는 거야. 검증 폴더나 다운로드 임시 폴더에 실행 파일을 두지 마.

일반 PowerShell에서 실행해.

```powershell
# $repo는 위에서 검증한 ZIP의 압축 해제 폴더 또는 소스 저장소야.
$relayRoot = 'C:\Dev\shearwater-runtime\forward'
$package = Join-Path $relayRoot 'v1'
if (Test-Path -LiteralPath $relayRoot) { throw '기존 설치가 있어. 상태와 설정부터 확인해.' }
New-Item -ItemType Directory -Path $package | Out-Null
New-Item -ItemType Directory -Path (Join-Path $relayRoot 'state') | Out-Null
Copy-Item -LiteralPath "$repo\hack\wsl-forward.py","$repo\hack\wsl-forward-autostart.py" -Destination $package
```

Linux 파일은 WSL 안의 `/opt/shearwater/windows-forward/v1`에 놓아.
아래는 Windows에 복사한 고정 패키지를 Ubuntu Bash에서 읽는 예시야.
기존 폴더가 있으면 덮어쓰지 말고 먼저
내용을 확인해. 루트 권한은 이 전용 경로의 생성과 읽기 권한 설정에 사용해.

```bash
test ! -e /opt/shearwater/windows-forward/v1 || { echo '기존 설치를 먼저 확인해'; exit 1; }
sudo install -d -m 0755 /opt/shearwater/windows-forward/v1
sudo install -m 0644 /mnt/c/Dev/shearwater-runtime/forward/v1/wsl-forward.py \
  /opt/shearwater/windows-forward/v1/wsl-forward.py
python3 --version
```

Linux 중계는 배포판의 기본 사용자로 실행돼. 이 파일과 상위 폴더를 그 사용자가
읽고 통과할 수 있어야 해. sw systemd 서비스만 root로 실행돼.

Windows PowerShell에서 `config.json`을 만들어. PowerShell 5의 기본 UTF-8 저장은
BOM을 넣을 수 있으므로 아래처럼 BOM 없는 UTF-8을 명시해. **IP·포트는 실제
Service 값으로 바꿔.** Windows 포트와 Linux 중계 포트는 다른 프로그램이 쓰고
있지 않아야 해. `18080`이 기존 수동 중계의 포트라면 검증 중에는 `18180`처럼
별도 포트를 사용하고, 확인된 기존 중계만 정상 종료한 뒤 전환해.

```powershell
$relayConfig = [ordered]@{
  schema = 1
  distro = 'Ubuntu-24.04'
  target_ip = '10.96.0.1'
  target_port = 80
  linux_port = 18081
  listen_port = 18080
  linux_script = '/opt/shearwater/windows-forward/v1/wsl-forward.py'
}
[IO.File]::WriteAllText((Join-Path $relayRoot 'config.json'),
  ($relayConfig | ConvertTo-Json), [Text.UTF8Encoding]::new($false))
```

Windows 수신은 `127.0.0.1`뿐이고 Linux 수신은 Windows NAT 게이트웨이 주소에서
오는 연결만 허용해. LAN 공개나 mirrored 네트워크 설정으로 바꾸는 도구는 아니야.
설정에 IP가 들어 있지만 WSL 자체의 IP를 고정하는 설정은 없어. 감독 프로그램이
Linux 중계의 준비 응답에서 현재 IP를 매번 받아.

### 2. Linux 부팅 서비스와 네트워크 확인하기

앞부분의 `sw service render/install` 절차로 **원래 사용하던 옵션**을 가진
`shearwater.service`를 등록해. 이미 서비스로 운영 중이면 중복 설치하지 마.
수동 서버에서 인계할 때도 앱 삭제나 `down --all`을 실행할 필요가 없어.
SQLite 백업과 앱 데이터 백업을 먼저 만들고 인계 후 비교해.

Pod 네트워크에는 Linux IPv4 전달이 필요해. 현재 `sysctl net.ipv4.ip_forward`가
`1`이어도 재부팅 후 유지된다는 뜻은 아니야. 이 작업의 실환경에서는 Shearwater
전용 systemd drop-in으로 서버 시작 전에 준비했어.

```ini
# /etc/systemd/system/shearwater.service.d/10-pod-forwarding.conf
# Pod에서 다른 주소로 가는 패킷을 호스트가 전달하도록 준비한다.
[Service]
ExecStartPre=/usr/sbin/sysctl -w net.ipv4.ip_forward=1
```

필요한 환경에서만 이 전용 파일을 추가하고 `sudo systemctl daemon-reload`를 해.
`command -v sysctl`로 실제 경로를 먼저 확인해. IPv4 전달은 호스트 커널의 설정이야.
기존 부팅 설정이 이미 책임지고 있다면 중복 파일을 만들지 않아도 돼.
Docker/containerd 설정 파일을 고칠 필요는 없어.

### 3. 예약 작업의 내용을 보고 등록하기

다시 Windows PowerShell에서 실행해. 현재 `python`이 실제 Python 3.11 이상인지
확인하고 그 설치 폴더의 `pythonw.exe`를 사용해. `pythonw`는 로그인할 때 불필요한
콘솔 창이 뜨지 않게 실행하는 같은 Python이야.

```powershell
$pythonExe = (python -c 'import sys; print(sys.executable)').Trim()
$pythonw = Join-Path (Split-Path -Parent $pythonExe) 'pythonw.exe'
$taskArgs = @{
  PackageDir = $package
  Config = (Join-Path $relayRoot 'config.json')
  StateDir = (Join-Path $relayRoot 'state')
  Pythonw = $pythonw
}
& "$repo\hack\install-wsl-forward-task.ps1" -Action Render @taskArgs
& "$repo\hack\install-wsl-forward-task.ps1" -Action Install @taskArgs
& "$repo\hack\install-wsl-forward-task.ps1" -Action Start @taskArgs
```

`Render`는 실행 경로·계정·인수를 보여 주고, `Install`은 등록만 해. `Start`가
지금 실행하는 단계야. 같은 이름의 기존 작업은 덮어쓰지 않아. PowerShell 실행
정책 때문에 차단됐다면 조직 정책과 파일 출처를 먼저 확인해. 설치 도구가 실행
정책을 전역으로 바꾸거나 Python을 새로 설치하지는 않아.

작업은 현재 사용자의 대화형 로그인으로 실행되며 암호를 저장하지 않아.
[Windows 작업의 보안 컨텍스트](https://learn.microsoft.com/en-us/windows/win32/taskschd/security-contexts-for-running-tasks)와
[예약 작업 등록 문서](https://learn.microsoft.com/en-us/powershell/module/scheduledtasks/register-scheduledtask?view=windowsserver2025-ps)를
참고할 수 있어. 사용자가 아직 로그인하지 않은 상태에서 동작하는 시스템 서비스는
아니야. 실행 시간 제한은 없고, 배터리로 전환해도 중단하지 않게 등록해.
감독 프로그램 자체가 오류로 종료되면 예약 작업이 1분 간격으로 최대 3회 다시
시작해. WSL 중계만 끝나는 경우에는 앞에서 설명한 감독 프로그램의 재시도를 사용해.

### 4. 동작 확인하기

```powershell
Get-ScheduledTask -TaskName Shearwater-Windows-Forward | Select-Object TaskName,State
Get-ScheduledTaskInfo -TaskName Shearwater-Windows-Forward |
  Select-Object LastRunTime,LastTaskResult
python -I "$package\wsl-forward-autostart.py" status --state-dir "$relayRoot\state"
curl.exe --fail --max-time 3 http://127.0.0.1:18080/
Get-Content "$relayRoot\state\supervisor.log" -Tail 30
```

예약 작업의 결과 `267009`(`0x41301`)은
[작업이 실행 중이라는 뜻](https://learn.microsoft.com/en-us/windows/win32/taskschd/task-scheduler-error-and-success-constants)이야.
`active: true`, `phase: running`, 실제 HTTP 응답을 함께 확인해.
`running`만으로 앱까지 정상이라고 판단할 수는 없어.
[각 상태의 의미와 문제 구분](windows-localhost.md#자동-감독-모드에서-접속이-안-될-때)도 참고해.

`LastTaskResult`는 현재 앱 건강 상태가 아니라 예약 작업의 최근 실행 요청 결과야.
이번 검증에서는 실행 중에 직접 `Start-ScheduledTask`를 다시 호출한 뒤
`0x800710E0`(요청 거부)이 남았지만, 기존 감독 PID와 HTTP 응답은 그대로 유지됐어.
이 코드를 항상 정상으로 무시하면 안 돼. 실제 프로세스·상태·응답을 확인해.
설치 스크립트의 `-Action Start`는 이미 실행 중이면 새 시작 요청을 보내지 않아.

### 일부러 멈추기와 다시 시작하기

감독 프로그램이 실행 중이면 `wsl --terminate`나 `wsl --shutdown` 뒤에도 자동으로
WSL을 다시 켜. 이것은 재부팅 복구를 위한 의도된 동작이야. WSL을 꺼 놓고 싶으면
**먼저 예약 작업을 비활성화하고 감독 프로그램을 정상 종료**해.

```powershell
Disable-ScheduledTask -TaskName Shearwater-Windows-Forward | Out-Null
python -I "$package\wsl-forward-autostart.py" stop --state-dir "$relayRoot\state"
# stopped / active:false가 될 때까지 상태를 확인해.
python -I "$package\wsl-forward-autostart.py" status --state-dir "$relayRoot\state"
```

`stop`은 종료 요청을 보낸 것이지 즉시 종료 완료를 뜻하지 않아. 중계는 진행 중인
연결을 최대 10초 기다리고, WSL 시작 도중이면 준비 응답 제한 시간과 자식 정리
시간도 필요할 수 있어. `Stop-ScheduledTask`로 강제 종료하는 것보다 이 정상 종료
절차를 먼저 사용해. 비활성화만으로 현재 실행이 멈추지는 않아.

이 동작은 Windows 입구를 멈춰. 앱이나 데이터는 삭제하지 않아. Linux sw까지
멈추려면 앞의 `sw service stop`을 별도로 사용해. WSL 전체 종료는 그 안의 다른
앱에도 영향을 주므로, 관련 작업을 확인한 뒤에만 실행해.

다시 사용할 때는 다음 두 명령을 실행해. 낡은 종료 요청 파일은 새 실행의 임의
ID와 달라서 새 프로세스를 멈추지 않아. 상태 폴더를 지우고 다시 만들 필요가 없어.

```powershell
Enable-ScheduledTask -TaskName Shearwater-Windows-Forward | Out-Null
Start-ScheduledTask -TaskName Shearwater-Windows-Forward
```

### 개발 단계의 실제 재부팅 검증 결과와 범위

alpha.6 공개 전 개발 검증에서, 2026-11-15에 Ubuntu-24.04만 실행 중인 환경에서 `wsl.exe --shutdown`으로 WSL VM을
실제로 종료했어. Linux 커널 boot ID가 달라진 것으로 새 부팅을 확인했어.
감독 프로그램이 WSL을 깨웠고 Linux 서비스가 sw를 시작했어. 복구를 기다리는 동안
수동 `wsl` 실행이나 `sw up`, 중계 시작 명령은 사용하지 않았어.

- 종료 명령 시작 후 약 **18.4초**에 기존 Windows 주소의 HTTP 응답이 돌아왔어.
  연속 10회 성공은 약 **20.2초**에 확인했어.
- Windows 감독 PID는 유지됐고 Linux 중계는 새 프로세스로 실행됐어. WSL IP는
  다시 읽었지만 이번 재부팅에서는 같은 값이었어. 다른 IP 반환은 모의 테스트로 확인했어.
- 앱 파일 32개와 인증·설정 파일 8개는 해시와 권한까지 동일했어. Pod UID/IP,
  Service IP, IP 예약 2개와 SQLite 무결성도 확인했어.
- 선언된 객체 9개는 그대로였고, 컨트롤러가 만드는 EndpointSlice 1개는 새 UID로
  재생성됐어. 연결 대상·포트·준비 상태·Service 소유 관계는 같았어.
- 앱 컨테이너의 재시작 수는 한 번 증가했어. WSL VM 자체를 끄면 앱 프로세스도
  종료되므로 이 경우에는 정상적인 복구야. 서버만 교체하는 경우와 구분해야 해.
- 복구 후 약 6분 38초, 초당 약 5개씩 보낸 요청 **1,985개가 모두 성공**했어.
  전체 관측의 실패 7개는 재부팅 중 발생했고, 성공으로 감추거나 재전송하지 않았어.

예약 작업을 다시 시작해도 감독 PID가 늘지 않는 것, 별도 포트의 Linux 중계를
종료하면 자동으로 돌아오는 것, 정상 종료도 확인했어. Windows OS 전체를 다시
시작하거나 로그아웃하지는 않았어. 사용자 로그인 트리거 등록과 그 작업의 실제
실행은 확인했지만, 물리적인 Windows 재로그인까지 시험했다고 말하지는 않아.
[WSL 종료 명령의 범위](https://learn.microsoft.com/en-us/windows/wsl/basic-commands)도
확인해. 재부팅 중 연결은 끊기며, 자동 복구는 무중단이나 일정한 복구 시간의
보장이 아니야.

alpha.6의 공개 소스로 다시 빌드한 릴리스 파일의 운영 교체·재부팅 결과는
[alpha.6 릴리스](https://github.com/ChoYugyeong/shearwater/releases/tag/v0.1.0-alpha.6)에
별도로 기록해. 위 개발 기록을 릴리스 재검증 결과로 대신하지 않아.
