# Windows localhost 접속만 멈출 때

이 문서는 **WSL 2의 NAT 네트워크**에서 Linux 앱은 정상인데 Windows의
`http://127.0.0.1:포트`만 가끔 시간 초과가 나는 경우를 다뤄.
Shearwater의 Pod·Service 검사와 Windows→WSL 연결 검사는 서로 다른 검사야.
하나가 통과했다고 다른 하나까지 정상이라고 결론 내리면 안 돼.

## 어떤 구간이 멈췄는지 구분하기

보통 접속 경로는 이렇게 이어져.

```text
Windows 브라우저 → Windows wslrelay → Hyper-V 소켓 → Linux localhost 중계
                → Shearwater Service → Pod의 앱
```

Hyper-V 소켓은 Windows와 WSL 가상 머신 사이의 통신 통로야. WSL 2.6.3의
localhost 중계는 새 TCP 연결마다 이 통로를 만든다.
[해당 버전의 Windows 중계 소스](https://github.com/microsoft/WSL/blob/2.6.3/src/windows/wslrelay/localhost.cpp)를
확인할 수 있어. 메모리 압력이나 연속 메모리 할당 실패 때문에 통로를 만들지
못하면, 앱 프로세스가 살아 있어도 Windows 요청은 기다리다가 실패할 수 있어.

먼저 실제 주소에 맞춰 **읽기만 하는 검사**를 해. 예제 IP를 그대로 믿지 말고
`sw get pods`, `sw get services`에서 확인한 값을 써.

```powershell
# Windows에서 기존 앱 입구 확인
curl.exe --fail --max-time 3 http://127.0.0.1:18080/

# WSL 내부에서 Service와 기존 Linux 중계 확인
wsl.exe -d Ubuntu-24.04 --exec curl --fail --max-time 3 http://10.96.0.1:80/
wsl.exe -d Ubuntu-24.04 --exec curl --fail --max-time 3 http://127.0.0.1:18080/
```

WSL 명령을 시작하는 것 자체도 실패한다면, 그 결과는 앱의 응답 실패와 구분해.
이미 열린 WSL 터미널에서 확인하면 새 세션 생성 실패와 앱 상태를 구분하기 쉬워.
필요한 경우 그 터미널에서 `free -h`, `cat /proc/buddyinfo`,
`sudo dmesg`를 읽어. `vmbus_alloc_ring`, `hvs_probe`, `page allocation failure:
order:7`, `UtilAcceptVsock`가 함께 있는지 살펴봐. `order:7`은 페이지 128개를
뜻하고, 4 KiB 페이지 환경에서는 연속된 512 KiB 할당이야. 총 여유 메모리만으로
연속 공간의 부족 여부를 판단할 수는 없어.

Windows 이벤트 뷰어의 **Microsoft-Windows-Hyper-V-Compute-Admin**에도 같은
구간의 높은 메모리 압력 경고가 있는지 확인해. 이 경고 하나만으로 HTTP 요청
하나의 원인을 확정할 수는 없어. WSL의 `dmesg -T` 시각도 가상 머신의 시계
보정 때문에 실제 Windows 시각과 어긋날 수 있어. 새 검사에서는 요청 시작
시각, 실패 단계, Windows 이벤트 시각, Linux 시계 표본을 함께 기록해.

관련 증상은 [WSL 메모리 할당 실패 보고](https://github.com/microsoft/WSL/issues/40795)와
[끝나지 않는 세션 중계 보고](https://github.com/microsoft/WSL/issues/41242)에도 있어.
이슈 보고와 내 환경의 원인이 반드시 같다는 뜻은 아니야.

## 선택적 우회: 앱 데이터는 일반 TCP로 전달하기

`hack/wsl-forward.py`는 기존 Service를 Windows localhost에 연결하는 보조
스크립트야. `sw` 서버나 앱 이미지를 교체하지 않아. 연결 경로는 다음과 같아.

```text
Windows 127.0.0.1 → Windows Python 중계 → WSL eth0 IPv4의 TCP 포트
                 → Linux Python 중계 → 기존 Service → 기존 Pod
```

WSL 실행은 Linux 중계를 시작할 때 한 번만 해. 자동 감독 모드는 중계가 끝났을 때만
다시 시작해. HTTP 연결마다 WSL 프로세스나
Hyper-V 소켓을 새로 만들지 않아. 단, VM 자체가 멈추거나 메모리가 고갈되면
이 경로도 멈출 수 있어. WSL 시작 실패와 OS의 메모리 문제를 고치는 커널 패치는 아니야.

필요한 것은 Windows와 WSL 양쪽의 Python 3.11 이상, WSL의 `ip` 명령,
실행 중인 Service야. 관리자 권한이나 새로운 Python 패키지는 필요 없어.
현재 도구는 NAT·eth0의 단일 IPv4 환경용이며 mirrored 네트워크용이 아니야.
Windows는 `127.0.0.1`에만 바인딩하고 Linux는 Windows NAT 게이트웨이 주소에서
온 연결만 받아. 앱을 LAN에 공개하는 도구로 사용하면 안 돼.

아래는 저장소가 `C:\Dev\shearwater`에 있는 예제야. 다른 위치라면 Windows
스크립트 경로와 `/mnt/c/...` 경로를 **둘 다** 바꿔. 처음에는 기존 입구와 다른
포트 `18180`으로 확인해. `18081`도 다른 프로그램이 사용하지 않아야 해.

```powershell
$run = Join-Path $env:TEMP ('sw-forward-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $run | Out-Null
python C:\Dev\shearwater\hack\wsl-forward.py windows `
  --distro Ubuntu-24.04 `
  --linux-script /mnt/c/Dev/shearwater/hack/wsl-forward.py `
  --target-ip 10.96.0.1 --target-port 80 `
  --linux-port 18081 --listen-port 18180 `
  --ready-file "$run\ready.json" --stop-file "$run\stop"
```

다른 PowerShell 창에서 `curl.exe --fail http://127.0.0.1:18180/`로 확인해.
`windows-ready`가 출력되기 전에는 시작 완료가 아니야. 준비 파일에는 Windows
PID, Linux PID와 주소가 들어 있지만 인증 토큰이나 앱 내용은 들어 있지 않아.
사용한 `$run` 경로를 기록해 두고 그 경로에 `stop` 파일을 만들면 종료해.

```powershell
# 위 명령에서 출력/기록한 실제 폴더를 넣어.
New-Item -ItemType File -Path 'C:\실제\실행폴더\stop'
```

종료할 때 새 연결 수신을 멈추고 각 중계에서 기존 연결을 최대 10초 기다려. 그 뒤에는
남은 연결을 닫아. 데이터 전송 중에 중계를 종료하면 전송도 끝날 수 있어.
기존 포트로 전환할 때는 그 포트를 소유한 **확인된 앱 중계만** 정지하고 새
중계의 `--listen-port`를 바꿔 실행해. 포트 충돌이 나면 다른 프로세스를 죽이지
말고 포트 소유자를 확인해. `--wait-listen=30`은 계획된 포트 인계 때 최대
30초 동안 바인딩만 기다리는 옵션이야. HTTP 요청을 재시도하는 옵션은 아니야.

위의 **단독 실행 명령**은 자동 시작 작업을 설치하지 않아. WSL 또는 Windows를
재시작해 Linux 중계가 끝났다면 새 실행 폴더로 다시 실행해야 해. 다시 시작할 때
현재 WSL IP를 읽어.

계속 사용할 때는 [자동 시작 안내](automatic-start.md#windows-로그인부터-중계까지-자동으로-시작하기)의
`wsl-forward-autostart.py`와 예약 작업을 사용해. Windows 로그인 때 감독 프로그램이
시작되고, WSL 종료를 발견하면 잠시 기다렸다가 WSL과 Linux 중계를 다시 켜.
Linux 안의 sw는 별도로 등록한 `shearwater.service`가 시작해. 자동 시작을 쓰려면
Windows·Linux 파일을 임시 검증 폴더가 아닌 고정 경로에 놓아야 해.

## 자동 감독 모드에서 접속이 안 될 때

아래는 자동 시작 문서의 설치 경로를 사용한 예제야. 직접 실행하던 임시 폴더의
준비 파일과 자동 감독 모드의 `status.json`을 혼동하지 마.

```powershell
python -I C:\Dev\shearwater-runtime\forward\v1\wsl-forward-autostart.py status `
  --state-dir C:\Dev\shearwater-runtime\forward\state
Get-ScheduledTask -TaskName Shearwater-Windows-Forward | Select-Object TaskName,State
Get-Content C:\Dev\shearwater-runtime\forward\state\supervisor.log -Tail 30
curl.exe --fail --max-time 3 http://127.0.0.1:18080/
```

| 상태 | 뜻과 다음 확인 |
| --- | --- |
| `active: false` | 감독 프로그램이 상태 폴더의 잠금을 갖고 있지 않아. 예약 작업의 실행 경로와 최근 결과를 확인해. JSON에 오래된 `running`이 남아 있을 수도 있어. |
| `starting` | WSL 실행과 Linux 중계의 준비 응답을 기다리는 중이야. 준비 응답의 제한 시간은 30초야. |
| `backoff` | 시작이나 연결 유지에 실패해 다음 시도를 기다려. `lastError`와 로그의 `wsl-stderr`를 먼저 봐. |
| `running` | Windows 수신 포트가 열렸다는 뜻이야. 앱의 정상 응답까지 뜻하지 않으므로 curl과 `sw doctor`도 확인해. |
| `stopped` | 해당 감독 실행이 종료됐어. 다시 쓰려면 예약 작업을 시작해. |

`retrySeconds`는 `backoff` 상태에서 의미가 있어. 로그에는 Linux에서 기록한 시각을
`sourceUnix`, Windows에서 받은 시각을 `unix`로 구분해. 로그 한 파일은 약 1 MiB,
현재 파일과 이전 3개를 합쳐 약 4 MiB로 제한해. HTTP 본문과 인증 토큰을 기록하지 않아.

포트 충돌 오류가 있으면 아래에서 **소유자만 확인**해. 감독 프로그램은 다른
프로그램을 종료하거나 포트를 빼앗지 않아. 기존 수동 중계가 남아 있다면 그
중계의 원래 종료 절차를 사용한 뒤 예약 작업을 시작해.

```powershell
Get-NetTCPConnection -LocalPort 18080 -State Listen |
  Select-Object LocalAddress,LocalPort,OwningProcess
```

WSL 내부의 Service는 정상인데 Windows 수신만 안 된다면, 설정의 배포판 이름과
Linux 스크립트 경로, Python 설치 위치를 확인해. `running`인데 앱이 응답하지
않는다면 Linux에서 `systemctl status shearwater.service`와 `sw doctor`를 확인해.
WSL을 일부러 꺼 놓을 때는 [감독 프로그램부터 멈춰야 해](automatic-start.md#일부러-멈추기와-다시-시작하기).

## 검증과 한계

```powershell
python -m unittest discover -s hack -p test_wsl_forward.py -v
python -m unittest discover -s hack -p test_wsl_forward_autostart.py -v
```

Linux에서도 같은 테스트를 실행할 수 있어. 큰 양방향 전송, 한쪽만 전송 종료한
뒤의 응답, 연결 수 제한, 다른 발신지 차단, 포트 충돌, 종료 시 진행 중인 응답
보존, 쓰기 요청의 중복 전송 방지, 응답 없는 중간 경로를 시험해. **메모리 고갈을 일부러 일으키는 검사는
아니야.** 중계 지연 주입에서는 옛 경로의 5회 시간 초과와 우회 경로의 응답을
구분해서 검사해. 이 인위적인 5회와 과거 운영 장애 5회를 합산하면 안 돼.

2026-11-15 실제 Windows/WSL 비교에서는 기존 경로에 HTTP 연결 20개를 유지할 때
VSOCK 연결이 20개 늘었고, 새 경로에서는 0개 늘었어. 앱 응답 해시는 동일했어.
이는 새 앱 경로가 연결마다 VSOCK을 생성하지 않는다는 측정이야. 당시 운영
장애의 메모리 상태를 그대로 재현했다거나 모든 종류의 시간 초과가 사라졌다는
보장은 아니야. 실패한 HTTP 요청을 자동 재전송하거나 성공으로 바꾸지도 않아.

같은 환경의 별도 포트에서 2분 동안 5,315개 HTTP 요청(새 경로 4,725개, 기존
대조 경로 590개)이 오류 없이 끝났어. 실제 주소로 전환한 뒤에는 5분간 새 TCP와
keep-alive를 합쳐 초당 2건씩 보내 596/596이 성공했어. 이때 Windows 여유 메모리의
최솟값은 약 1.35 GiB였어. 전환 과정 자체는 리스너 인계 0.175초, 검사 요청 2건
연결 종료를 기록했으므로 무중단 인계라고 부르지 않아. 더 큰 전환 후 부하는
2 GiB 여유 메모리 기준을 충족하지 않아 시작하지 않았어. 새 중계 테스트 13개는
Windows·Linux 모두 통과했고, 전체 Python 회귀는 Linux 53개 통과,
Windows 52개 통과·기존 symlink 권한 검사 1개 건너뜀이야.

이후 자동 시작 검증에서는 감독 모드 테스트 10개를 추가했어. 전체 Python 회귀는
Linux 63개 통과, Windows 62개 통과·같은 권한 검사 1개 건너뜀이야. 실제 WSL VM을
종료한 뒤 수동 시작 명령 없이 기존 Windows 주소가 약 18.4초 만에 돌아왔고,
안정 복구 후 약 6분 38초 동안 1,985개 요청이 모두 성공했어. 전체 전환·재부팅
관측에서는 3,333개 성공과 재부팅 구간의 실패 7개를 모두 기록했어. 별도 복구 시점
탐지 요청도 실패 7개 후 연속 10개 성공이었으며, 두 측정의 요청을 섞어 세지 않았어.
시험은 약 초당 5회였으므로 최대 처리량이나 모든 환경의 복구 시간 보장은 아니야.

공유 호스트에서 추가 부하 검사를 할 때는 메모리 여유를 먼저 확인하고,
낮아지면 검사를 멈춰. 앱을 지우거나 WSL 전체를 종료하는 조치로 시작하지 마.
