# 설치부터 AI 연결까지: 먼저 한 앱만 연결하기

이 문서는 **v0.1.0-alpha.13**에서 `sw setup`, 기존 Service 자동 탐색,
MCP 설정 내보내기를 사용하는 순서야. `setup`은 alpha.11부터 있어.
새 버전은 별도 폴더에 설치해. 실행 중인 서버 교체는 [백업·업데이트 순서](install.md)를
따로 따라야 하며, 아래 설치기는 기존 운영 서버를 덮어쓰지 않아.

## 처음에 무엇을 준비하는가

Shearwater는 컨테이너를 실행하고, 원하는 앱 상태를 유지하고, AI에게 작은 운영 도구를
제공해. 모델 자체나 로그인 계정을 포함하지 않아. 다음 세 연결은 서로 달라.

| 연결 | 역할 | 한 번 준비하는 사람 |
| --- | --- | --- |
| Linux → containerd | 실제 컨테이너 실행 | 서버 관리자 |
| 일반 사용자 → sw API | 앱 상태 조회와 승인한 변경 실행 | 관리자가 `sw setup`으로 위임 |
| Claude/Qwen/DeepSeek → sw MCP 또는 chat | 자연어 요청을 앱 운영 도구로 전달 | 일반 사용자 |

모델이 관리자 비밀번호를 받거나 root로 실행될 필요는 없어. 아래 `sw setup`으로 만드는
인증은 **운영자용 전체 로컬 관리자 권한**이야. alpha.12부터 운영자가 이 인증을 보관하고
[앱별 위임 안내](agent-operations.md)의 `sw task create`로 제한 토큰을 따로 만들 수 있어.
AI에게는 그 제한 연결만 전달해. 기존 `sw connect`에 관리자 인증을 전달하는 방식은
앱 고정·계획 승인을 제공하지만 서버가 강제하는 앱별 권한 제한은 아니야.

## 1. 실행 파일 받기

릴리스에 동봉한 `install.py`와 체크섬을 받아 설치기 자체를 먼저 검사해.
Ubuntu Bash에서 Python 3.11 이상의 표준 라이브러리로 실행해.

```bash
download_dir=$(mktemp -d)
cd "$download_dir"
version=v0.1.0-alpha.13
release="https://github.com/ChoYugyeong/shearwater/releases/download/$version"
curl --fail --location --proto '=https' --tlsv1.2 "$release/install.py" -o install.py
curl --fail --location --proto '=https' --tlsv1.2 "$release/SHA256SUMS" -o SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
# install.py: OK를 확인하고, 설치기 내용을 검토한 뒤 진행해.
python3 install.py --version "$version"
python3 install.py --version "$version" --apply
```

첫 Python 명령은 설치 위치를 보여 주기만 해. 둘째 Python 명령은 정확한 버전의 압축과 SHA256SUMS를 받아
전체 압축 해시를 확인하고, 새 버전 폴더에 풀고, 내부 `sw.sha256`도 확인해.
이미 있는 설치 폴더는 덮어쓰지 않아. 압축 안의 상위 경로 이동·심볼릭 링크·장치 파일도
받지 않아. 다운로드 흔적은 출력한 임시 폴더에 남겨. SHA-256은 무결성 검사이고,
릴리스 게시자와 독립된 서명은 아니야.

설치기는 WSL·containerd·모델을 설치하거나, 실행 중인 서버·PATH·AI 설정을 바꾸지 않아.
Linux x86_64용이야. 설치한 폴더에는 `INSTALL-ko.md`와 이 안내인 `ONBOARDING-ko.md`가 있어.
이후 명령에 쓸 실행 파일을 지정해. 별도 설치 위치를 골랐다면 그 위치로 바꿔:

```bash
sw="$HOME/.local/share/shearwater/v0.1.0-alpha.13/sw"
"$sw" version
"$sw" setup -h
```

이후 `$sw`는 그 **Linux 절대 경로**야. 새 터미널에서는 이 변수를 다시 지정해.
버전이 `0.1.0-alpha.13`인지 확인해. 직접 빌드하려면 [소스 빌드 안내](quickstart.md)를 사용해.

## 2. 서버 준비와 인증을 한 번에

일반 사용자 터미널에서 이름을 먼저 확인해. `root`이면 AI를 사용할 일반 사용자로 들어가.

```bash
id -un
sudo "$sw" setup --user="$(id -un)" --node-name=shearwater
```

아직 아무것도 바꾸지 않아. `checks`에서 빠진 containerd·runc·네트워크 도구나
호스트 설정을 알려 줘. 준비물은 [설치 안내](install.md)의 WSL/Ubuntu 절차로 준비해.
기존 Docker가 설치한 containerd나 공유 호스트 설정을 무작정 다시 설치하지 마.

새 전용 환경의 준비물이 맞고 출력한 경로를 확인했다면:

```bash
sudo "$sw" setup --user="$(id -un)" --node-name=shearwater --apply
```

동작은 다음과 같아.

1. 해당 인증과 주소로 서버가 응답하면 그 서버를 계속 사용해.
2. 서버가 없으면 `sw up`의 포트·경로·인스턴스 소유권 검사를 거쳐 시작해.
3. 선택한 일반 사용자의 `~/.local/state/shearwater/client`에 CA와 토큰을 준비해.
4. 실제 일반 사용자 권한으로 인증 요청이 되는지 확인해.

그 뒤 그 사용자는 `sw status`, `sw tree`, `sw get`·`logs`·`events`·`why`, `sw observe`·`verify`, `sw task`를
옵션 없이 쓸 수 있어. 이 명령들은 `--config-dir`과 `--listen`을 둘 다 생략하면 저장된 연결을 써.

토큰은 화면이나 명령줄 인자에 출력하지 않아. 인증 파일은 사용자만 읽는 0600 권한,
새 폴더는 0700 권한으로 만들어. 서버 개인 키·DB·앱 파일은 복사하지 않아.
같은 명령의 재실행은 동일한 인증만 재사용해. 다른 인증이 있으면 덮어쓰지 않고
새 `--client-dir`를 쓰라고 알려 줘. 서버 시작 뒤 인증 준비만 실패했다면 서버가 남아
있을 수 있어. 오류에 그 사실을 표시하며 앱을 삭제하는 자동 되돌리기는 하지 않아.

기존 인스턴스의 경로·주소가 기본값과 다르면 `--config-dir`, `--listen`, `--data-dir`,
`--run-dir`, `--log-dir`, `--containerd-socket`, `--node-name`을 실제 값으로 지정해.
**`setup`은 실행 중인 서버의 바이너리를 업데이트하거나 자동 부팅 서비스를 설치하지 않아.**
부팅 설정은 [자동 시작 안내](automatic-start.md)를 따로 봐.

## 3. 기존 앱 또는 새 앱 하나 선택하기

이제부터 `sudo` 없이 같은 일반 사용자로 실행해. 기본 인증을 준비했다면:

```bash
# 이미 실행 중인 앱: 현재 이미지를 다시 적지 않아도 돼.
"$sw" connect --name=web -n default

# 아직 없는 새 앱: 실행할 이미지는 명시해야 해.
"$sw" connect --name=trial --image=nginx:1.28-alpine --port=80
```

기존 앱의 Deployment와 일치하는 Service가 하나면 자동으로 선택해. 앱과 Service의
이름이 달라도 되고 `app: web`, 여러 라벨, 이름 붙은 targetPort도 사용할 수 있어.
Service가 여러 개이거나 TCP 포트가 여러 개라면 사람이 하나를 명시해.

```bash
"$sw" connect --name=web --service=public-web --port=8080
```

`--port`는 **Service를 방문하는 포트**야. Service가 `8080 → http → 컨테이너 80`으로
연결되어 있다면 8080을 골라. 새 앱을 만들 때만 Service와 컨테이너 포트를 같은 값으로
만들며 기본값은 80이야.

기본 작업 파일은 `~/.local/state/shearwater/workflows/default-web.json`이야.
이 파일에는 토큰 대신 인증 폴더 위치, 서버 신원 해시, 앱·Service UID, 선택 경로와
계획 기록을 보관해. `connect`는 서버의 앱·라벨·Service를 고치지 않아. 기존 작업 파일도
덮어쓰지 않으므로 다른 연결은 새 `--workflow=/절대/경로/web-new.json`으로 만들어.
기본 위치 외에 인증을 위임했다면 `--config-dir`와 `--listen`을 명시해.

연결 직후 AI에게 먼저 **“이 앱이 실제로 응답하는지 확인해 줘”**라고 해.
`connected`는 연결 정보를 기록했다는 뜻이고, `healthy`가 실제 준비 상태와 내부 HTTP를
통과했다는 뜻이야. 새 앱이라면 **“배포 계획을 만들고 승인 전에 멈춰 줘”**라고 해.

## 4. 사용하는 AI에 연결하기

아래 예시는 `default/web`의 기본 작업 파일을 사용해. 출력된 실제 경로와 맞춰.

```bash
work="$HOME/.local/state/shearwater/workflows/default-web.json"
```

### Qwen: 이미 설치된 Ollama 모델 사용

Linux Ollama라면:

```bash
"$sw" chat --workflow="$work" --allow-write --model=qwen2.5:latest
```

Windows Ollama라면 릴리스/소스의 `hack/chat-windows.py`를 PowerShell에서 사용해.
`--sw`, `--workflow`에는 Linux 경로를 넣어. 지원 인자는 `--help`에서 확인해.
이 도우미는 모델을 설치하거나 Windows 방화벽을 열지 않아.

### Claude Code: 이 연결만 담은 설정 파일 사용

Linux Claude Code에서는 아래처럼 **새 파일에** MCP 설정을 내보내.
`noclobber`는 같은 이름의 기존 파일을 덮어쓰지 않게 하는 Bash 옵션이야.

```bash
( set -o noclobber; "$sw" workflow mcp-config --file="$work" > "$work.mcp.json" )
claude --strict-mcp-config --mcp-config "$work.mcp.json"
```

Windows Claude Code에서는 Ubuntu에서 `--platform=windows`를 붙여 출력한 JSON을
새 Windows 설정 파일에 저장한 뒤 같은 Claude 옵션으로 열어. 이 출력은 정확한
WSL 배포판·일반 사용자·Linux 실행 파일을 자동으로 채워 줘.

```bash
"$sw" workflow mcp-config --file="$work" --platform=windows
```

Claude 자체 로그인은 Claude가 제공하는 로그인 절차를 사용해. sw에 Claude 비밀번호나
API 키를 넣을 필요는 없어. 위 옵션은 설치된 Claude Code 2.1.325 명령줄에서 확인했어.

### DeepSeek와 다른 MCP 앱

같은 `mcpServers.shearwater`의 `command`와 `args`를 해당 앱의 MCP 설정에 등록해.
Windows 앱이면 위 Windows용 출력을 사용해. 설정 화면이 다를 수 있으므로 기존
MCP 설정 전체를 교체하지 말고 Shearwater 항목만 추가해.
DeepSeek의 공식 명령줄 연결은 실제 모델로 검증하지만, 그래픽 앱의 버튼이나 설정
화면까지 자동으로 조작했다는 뜻은 아니야. [MCP 상세 안내](agent-mcp.md)를 함께 봐.

MCP 설정에는 토큰이 들어가지 않아. 실제 도구는 일반 사용자의 인증 파일을 읽어.
모델에게는 `sw_workflow` 하나만 보이고 이름·UID·포트를 인자로 다시 적게 하지 않아.
외부 AI의 쉘 도구까지 함께 허용하면 그 AI가 별도 CLI를 실행할 수 있으므로,
시험에서는 이 MCP 하나만 연결하고 일반 명령 실행 도구는 끄는 것을 권해.

## 5. 배포 → 업데이트 → 장애 복구

AI에게 아래 순서로 요청해. 계획을 만드는 것과 서버에 적용하는 것은 다른 단계야.

1. “연결된 앱을 배포할 계획을 만들어 줘. 적용 전에 멈춰.”
2. 사람이 `sw workflow review --file="$work"`로 실제 변경을 읽어.
3. 괜찮으면 `sw workflow approve --file="$work" --confirm=출력된_정확한_해시`로 승인해.
4. “방금 계획을 승인했어. 적용하고 실제 응답까지 확인해 줘.”
5. 새 이미지를 선택하고 “다른 설정과 데이터를 유지하며 업데이트 계획을 만들어 줘.”
6. 문제가 있으면 “실제 실패 근거를 읽고 마지막 정상 이미지로 복구할 계획까지 만들어 줘.”

새 이미지의 긴 digest는 [빌드 결과 파일](agent-workflow.md#업데이트와-장애-복구)로 고정할 수 있어.
이미지 변경과 복구에는 각각 새 계획의 승인이 필요해. 모델에게 승인 도구는 제공하지 않아.
복구 기준은 이 작업 파일에서 HTTP까지 확인한 이미지야. 확인 기록이 없으면 추측하지 않아.
이미지 복귀가 앱이 바꾼 DB 내용을 되돌리는 것은 아니므로 DB 변경에는 별도 복구 계획이 필요해.

## 잘 안 될 때

| 메시지/상태 | 의미 | 다음 행동 |
| --- | --- | --- |
| `found 0 matching Services` | 앱 라벨과 일치하는 Service가 없음 | 앱과 Service 설정을 읽어 보고 올바른 대상을 선택해 |
| `found 2 matching Services` | 여러 접속 경로 중 하나를 골라야 함 | `--service=이름` 지정 |
| `one selected TCP port` | 여러 포트 또는 선택한 포트가 없음 | 실제 Service TCP 포트를 `--port`로 지정 |
| `ambiguous Service` | 같은 Service가 다른 앱 Pod도 선택함 | 다른 앱을 섞어 성공 판정하지 않도록 연결을 중단한 것. 설정을 검토해 |
| `STALE ... route changed` | 연결 뒤 Service UID/IP/selector/선택 포트가 바뀜 | 변경 의도를 확인하고 새 작업 파일로 다시 연결해 |
| `existing client differs` | 해당 폴더에 다른 인증이 있음 | 원래 파일을 보존하고 새 `--client-dir` 사용 |
| `not_met`, `http_failed` | 아직 준비 안 됐거나 실제 응답 실패 | `diagnose`의 근거 확인. verify는 최대 약 18초 기다리고 쓰기는 하지 않아 |
| `awaiting_approval` | 계획만 저장됨 | 사람이 원문을 확인하고 정확한 계획을 승인해 |

현재 자동 연결은 Shearwater가 관리하는 **Deployment의 단일 컨테이너 HTTP 앱** 대상이야.
이미 Docker Compose에서 실행 중인 컨테이너를 소유권째 가져오는 기능은 아니야.
TCP 비HTTP 앱·여러 컨테이너·selector 없는 외부 Service에는 기존 집중 도구와 별도 검증을 써.
공개 도메인, TLS, 사용자 로그인, 다중 노드, 부팅 전 복구까지 이번 HTTP 검사에 포함하지 않아.

구현과 실제 시험 결과는 [설치·기존 앱 연결 검증](onboarding-verification.md)에 정리해.
