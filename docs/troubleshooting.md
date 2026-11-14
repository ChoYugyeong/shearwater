# `sw get / logs / events / why`로 문제 찾기

이 문서는 **`0.1.0-alpha.5`**의 진단 명령을 설명해.
alpha.2 실행 파일에는 이 네 명령이 없어. [설치 안내](install.md)로 alpha.5를 받거나
[개발 빌드 방법](../README.md)으로 만든 `dist/sw`를 사용해.
진단용 실행 파일을 받거나 빌드했다고 실행 중인 서버가 자동 교체되지는 않아.
네 명령은 기존 로컬 서버의 HTTPS API를 **조회만** 해. Pod를 재시작하거나 설정·DB를 수정하지 않아.

## 1. 어떤 순서로 볼까?

처음에는 전체 상태 → 문제 Pod → 그 Pod의 이유 순서가 좋아. 아래는 WSL의 Bash에서
[설치 안내](install.md)의 사용자 폴더에 설치한 경우야. 소스에서 빌드했다면
`sw` 변수를 그 `dist/sw`의 절대 경로로 바꿔. `web`은 예시이므로 실제 이름으로 바꿔.

```bash
sw="$HOME/.local/share/shearwater/v0.1.0-alpha.5/sw"
sudo "$sw" status
sudo "$sw" get pods -A
sudo "$sw" get pods -n default -l app=web
# 목록에서 실제 Pod 이름을 찾은 다음 실행해.
sudo "$sw" why pod/web-실제이름 -n default
```

`status`는 전체 상태, `get`은 객체 목록·상세, `why`는 Pod 하나의 상태·이벤트·로그를 묶어 보여 줘.
소유 관계를 보고 싶으면 `sw tree --problems`를 함께 써.
서버에 접속하지 못한다면 Pod 원인을 읽을 수 없으므로 `sw doctor`부터 확인해.

용어를 먼저 정리하면 다음과 같아.

| 말 | 뜻 |
|---|---|
| namespace / `-n` | 객체를 묶는 이름 공간. 생략하면 `default` |
| Pod | 함께 실행하는 컨테이너들의 단위. 앱 하나가 컨테이너 하나인 경우도 많아 |
| container / `-c` | Pod 안에서 로그를 읽을 컨테이너 이름 |
| UID | 객체를 생성할 때 붙는 고유 ID. 같은 이름으로 지우고 다시 만들면 달라져 |
| resourceVersion / `rv` | 서버에 저장된 변경 번호. 시각이나 컨테이너 재시작 횟수가 아니야 |
| restartCount | 같은 Pod·컨테이너의 실행 시도가 얼마나 다시 시작됐는지 나타내는 수 |
| Event | 노드·컨트롤러가 남긴 시작·실패 등의 관찰 기록. 앱 자체 로그와는 달라 |

기본 인증 경로 `/etc/shearwater`는 root 소유라 예제에 `sudo`가 있어. 읽기 명령이
root 권한으로 객체를 바꾸는 것은 아니고, 기존 CA와 관리자 토큰을 읽기 위해 필요한 거야.
다른 경로로 서버를 시작했다면 네 명령 모두에 같은 연결 정보를 지정해.

```bash
sudo "$sw" get pods --listen=127.0.0.1:16443 --config-dir=/내/인스턴스/etc -n demo
```

`--listen`은 `https://` 없이 로컬 `호스트:포트` 형식이야. 인증서 검증을 끄거나
원격 주소를 허용하는 옵션은 없어. `--timeout=10s`가 명령 전체의 기본 제한 시간이야.
여러 API를 읽는 `why`도 요청마다 10초를 새로 받지 않아. 0초 초과, 최대 5분까지 지정할 수 있어.

## 2. `get`: 무슨 객체가 있는지, 어떤 상태인지

```bash
sudo "$sw" get pods -n demo
sudo "$sw" get pods -A
sudo "$sw" get pods -n demo -l app=web
sudo "$sw" get pod/web-실제이름 -n demo
sudo "$sw" get deployments -n demo
sudo "$sw" get services -n demo
sudo "$sw" get nodes
sudo "$sw" get pod/web-실제이름 -n demo --full
```

기본 출력은 객체마다 한 줄이야. 네임스페이스, 종류/이름, 준비 수·재시작 수 등의
요약, 저장 리비전을 보여 줘. 객체 하나를 지정하면 UID도 보여 줘.
환경변수·전체 매니페스트를 기본 요약에 펼치지는 않아.

`--full` 또는 `-o json`은 API의 전체 JSON을 출력해. 둘은 `get`에서 같은 의미야.
여기에는 사용자가 저장한 환경변수 값·주석·저장만 하는 필드도 들어 있으므로,
공개 이슈에 붙일 때는 필요한 부분을 골라. 큰 정수도 반올림하지 않고 보존해.
전체 JSON에 필드가 있다고 해서 런타임이 모두 실행한다는 뜻은 아니야.
[지원 범위 표](compatibility.md)의 “저장만”을 함께 봐.

| 객체 | 사용할 수 있는 이름 |
|---|---|
| Pod | `pods`, `pod`, `po` |
| Deployment | `deployments`, `deployment`, `deploy` |
| ReplicaSet | `replicasets`, `replicaset`, `rs` |
| Service | `services`, `service`, `svc` |
| Node | `nodes`, `node`, `no` |
| Namespace | `namespaces`, `namespace`, `ns` |
| Event | `events`, `event`, `ev` |
| EndpointSlice | `endpointslices`, `endpointslice` |

형식은 `TYPE` 또는 `TYPE/NAME`이야. `get pod web`처럼 두 인자를 쓰거나
`get pods,services`처럼 종류를 합치는 문법은 지원하지 않아.
`-A`는 전체 네임스페이스 **목록**을 읽을 때 쓰고, 객체 하나에는 `-n`을 써.
라벨 선택 `-l`도 목록에만 적용돼. `get` 기본 출력은 namespace/name 순으로 정렬돼.
목록이 비어 있으면 `No resources found.`와 종료 코드 0을 반환해.

## 3. `logs`: 앱이 쓴 내용을 읽기

```bash
sudo "$sw" logs pod/web-실제이름 -n demo
sudo "$sw" logs pod/web-실제이름 -n demo -c web --tail=100
sudo "$sw" logs pod/web-실제이름 -n demo -c web --previous
sudo "$sw" logs pod/web-실제이름 -n demo --tail=200 --limit-bytes=131072
```

기본값은 **마지막 50줄, 최대 65,536바이트(64 KiB)**야. 줄 수와 바이트 제한을 함께
적용해. 긴 한 줄 때문에 바이트 제한에 걸리면 줄 중간에서 끝날 수 있어.
`--tail`은 0~10,000, `--limit-bytes`는 1~1,048,576을 허용해. `--tail=0`은 빈 출력이야.

컨테이너가 하나면 이름을 자동으로 고르고, 둘 이상이면 `-c NAME`을 반드시 지정해야 해.
`--previous`는 **직전 실행 시도**의 로그야. 모든 과거 로그를 합치거나 직전 Pod의
로그를 찾는 옵션이 아니야. 재시작한 적이 없거나 이전 파일이 없으면 오류가 나와.
앱이 아직 한 번도 실행되지 않았다면 “시작 대기 중”인 이유를 오류에 표시해.
실행된 앱의 기록이 비어 있으면 정상적으로 빈 출력을 반환할 수도 있어.

이 명령은 한 번 읽고 끝나. `-f`/`--follow`, 타임스탬프, 시간으로 자르기는 지원하지 않아.
계속 따라가야 한다면 기존 `kubectl logs -f`를 사용해.
stdout에는 앱의 원래 바이트만 써서 파일로 저장하거나 다른 도구에 넘길 수 있어.
앱이 출력한 제어 문자도 원문 그대로라는 뜻이야. 터미널에서 인용된 형태로 보고 싶으면
`why`의 로그 부분을 사용해.

로그를 읽기 전후의 Pod UID·컨테이너 ID·재시작 수를 대조해. 중간에 대상이 바뀌면
`CONFLICT retry=safe`로 실패하고 섞인 로그를 일부 출력하지 않아. 이때 다시 실행하면 돼.

## 4. `events`: 노드와 컨트롤러가 무엇을 관찰했는지

```bash
sudo "$sw" events -n demo
sudo "$sw" events -A --limit=50
sudo "$sw" events -n demo --for=pod/web-실제이름
sudo "$sw" events -n demo --for=deploy/web -o json
```

기본적으로 최근에 **갱신된** Event 20개를 먼저 보여 줘. 반복되는 같은 이벤트는
새 줄을 무한히 쌓는 대신 `count`와 마지막 발생 시각을 갱신할 수 있어.
그래서 `count=8`은 해당 기록에 모인 횟수이지 Pod의 재시작 수가 아니야.
실제 이벤트가 없는 객체도 있으므로 빈 결과가 곧 정상 동작의 증거는 아니야.

`--for TYPE/NAME`은 지금 존재하는 객체를 먼저 읽고 **그 UID**에 속한 이벤트만 골라.
같은 이름으로 재생성된 예전 Pod의 오류를 현재 Pod의 오류라고 붙이지 않아.
대상이 삭제돼 없으면 `NOT_FOUND`, 읽는 중 재생성되면 `CONFLICT`를 반환해.
Pod 이벤트는 해당 Pod의 수명에 묶이고 정리될 수 있어. 영구 감사 기록은 아니야.

```bash
# 이전 출력에서 관찰한 events rev=123 다음에 갱신된 기록만 보기
sudo "$sw" events -n demo --since=123 --limit=100 -o json
```

`--since`는 **같은 서버에서 본 resourceVersion 숫자**를 받아. `10m`이나 날짜는 안 돼.
JSON의 `resourceVersion`은 목록을 읽은 리비전, `items`는 고른 기록,
`omitted`는 출력 개수 제한 때문에 빠진 기록 수야. 제한은 최대 1,000개야.
`omitted`가 0보다 크면 `--limit`을 늘리거나 범위를 좁혀 확인한 다음 커서를 옮겨.
빠진 기록이 있는데 새 리비전을 곧바로 `--since`로 쓰면 그 기록은 건너뛸 수 있어.
별도 DB로 재설치한 서버에 예전 커서를 재사용하지 마.

## 5. `why`: 상태, 이벤트, 로그를 묶어서 원인 좁히기

```bash
sudo "$sw" why pod/web-실제이름 -n demo
sudo "$sw" why pod/web-실제이름 -n demo -c web --tail=100
sudo "$sw" why pod/web-실제이름 -n demo -o json
```

출력은 대상 UID·리비전 → 현재 상태 → 관찰된 문제(`finding`) → 다음 확인(`next`) →
컨테이너의 현재/직전 종료 코드 → 이벤트 → 현재/직전 로그 순서야.
`why`가 임의로 명령을 실행하거나 앱을 수정하지는 않아.

`why`와 `logs`는 현재 `pod/NAME`만 지원해. Deployment에는 Pod가 여러 개일 수 있으므로
`get pods -l app=web` 또는 `tree`로 실제 Pod를 골라.
`why`는 기본적으로 최대 8개 컨테이너의 로그를 읽고, 더 많으면 `-c`로 좁히도록 안내해.
`-c`는 수집할 **로그**를 고르는 옵션이고 Pod 전체의 컨테이너 상태·문제는 계속 보여 줘.
각 현재/직전 로그에 `--tail`과 `--limit-bytes`가 따로 적용돼.

| 관찰된 이유 | 뜻과 다음 확인 |
|---|---|
| `ErrImagePull`, `ImagePullBackOff` | 이미지를 받지 못했어. 이미지 이름·태그와 레지스트리 연결 오류를 봐. 비공개 레지스트리 인증은 아직 미지원이야 |
| `CrashLoopBackOff` | 실행된 앱이 반복해서 종료돼. 현재/직전 종료 코드와 로그, 앱 명령·설정을 함께 봐 |
| `CreatePodSandboxError` | 컨테이너 전 준비에 실패했어. 실제로 잘못된 `hostPath.type`도 이 이유가 나와. 메시지의 볼륨 경로·종류 또는 네트워크 오류를 구분해 |
| `CreateContainerConfigError` | 컨테이너 설정을 준비하지 못했어. 메시지의 마운트·볼륨·설정 값을 확인해 |
| `RunContainerError` | 런타임이 컨테이너를 시작하지 못했어. 이미지 안의 실행 파일, `command`/`args`, 마운트를 확인해. 앱 로그가 없을 수도 있어 |
| `ContainerTerminated` | 실행이 끝났어. 종료 0도 `restartPolicy`에 따라 다시 실행될 수 있어. 0이 아닌 코드는 실패 결과이지 구체적인 원인의 증명은 아니야 |
| `Restarted` | 지금 실행 중이지만 재시작 이력이 있어. 직전 종료와 로그를 봐 |
| `Unscheduled`, `NoContainerStatus` | 노드 배치나 상태 보고가 아직 없어. `status`, 이벤트, `doctor`를 확인해 |
| `Terminating` | 삭제 요청 뒤 유예 기간·finalizer 처리를 기다리고 있어 |
| `NoFailureReported` | 현재 컨테이너 상태에 실패가 보고되지 않았어. 앱 HTTP/TCP 응답까지 확인했다는 뜻은 아니야 |

예를 들어 `hostPath`에 실제 **파일**을 지정하면서 `type: Directory`라고 선언하면
종류가 맞지 않아 실행되지 않아. 디렉터리로 바꾸거나 선언을 실제 의도에 맞춰야 해.
`DirectoryOrCreate`는 없는 디렉터리를 만들 수 있지만 기존 파일을 디렉터리로 바꾸지는 않아.
진단 명령은 그 경로를 만들거나 삭제하지 않아.

종료 코드 137만으로 OOM이라고 단정하지 않아. WSL이 내려가 태스크를 잃은 경우에는
실제 종료 코드를 모르므로 상태에 `reason: Unknown`과 그 설명이 남을 수 있어.
그때의 표기용 코드 255를 앱이 실제로 반환한 코드로 해석하면 안 돼.

로그가 없거나 아직 시작되지 않았으면 `unavailable`로 이유를 표시해. 빈 문자열을
정상 증거처럼 바꾸지 않아. 로그 부분의 각 줄은 따옴표로 감싸 제어 문자를 이스케이프해.
바이트 한도와 같은 크기의 조각에는 잘렸을 가능성도 표시해. 줄 제한 역시 적용되므로
이 출력은 전체 이력이 아니라 제한된 발췌야.

상태를 읽는 동안 Pod가 변하면 최대 세 번 다시 수집하고, 계속 변하면 `CONFLICT`를 반환해.
UID와 Pod 리비전을 대조하지만 로그·이벤트 파일 전체를 잠그는 것은 아니므로
한 시점에 완전히 고정된 기록이라고 보장하지는 않아.

**`why` 종료 코드 0은 진단 수집에 성공했다는 뜻**이야. 앱이 실패해도 성공적으로
그 이유를 보여 주면 0이야. 자동화에서는 JSON의 상태·findings를 직접 판단해.
공개 alpha.3의 Ready는 실행 중인 컨테이너 기준이야. alpha.4는
**설정된 startup/readiness 프로브 결과도 Ready에 반영**해. [프로브 점검](health-probes.md)을 봐.
실제 응답은 별도로 확인해야 해. Service 연결 문제는 [연결 점검 순서](service-networking.md#연결이-안-될-때-확인할-순서)를 봐.

## 6. 진단 명령 자체가 실패한다면

오류는 stderr로 나가고 종료 코드가 1이 돼. 형식은 `CODE retry=safe|never`야.
`safe`는 같은 **조회**를 다시 시도해도 된다는 표시이고 성공 보장은 아니야.
실패한 읽기의 중간 출력은 stdout으로 보내지 않아.

| 오류 | 다음 확인 |
|---|---|
| `INVALID` | `-h`에서 문법·범위를 확인해. 컨테이너가 여러 개인 `logs`에는 `-c`가 필요해 |
| `NOT_FOUND` | 이름·네임스페이스가 맞는지 `get`으로 확인해 |
| `NOT_RUNNING` | 같은 연결 옵션으로 `status`/`doctor`를 실행해. WSL 재시작 뒤 서버를 다시 올렸는지 봐 |
| `UNAUTHORIZED` | 인증 설정 경로와 대상 서버가 맞는지 확인해. CA 검증을 끄는 방식으로 해결하지 않아 |
| `CONFLICT` | 읽는 동안 객체나 실행 시도가 바뀌었어. 잠시 후 다시 조회해 |
| `TIMEOUT` | 서버 상태를 보고 필요하면 전체 제한 시간 `--timeout`을 늘려 |
| `UNSUPPORTED` | 예를 들어 `--runtime=none` 서버에는 실제 컨테이너 로그가 없어 |
| `RUNTIME_ERROR`, `INTERNAL` | 서버 메시지와 `doctor`의 로그 위치를 함께 확인해 |

WSL이 종료됐으면 `/run`의 실행 기록과 앱 프로세스가 없어질 수 있지만,
`/var/lib/shearwater`의 DB와 `/etc/shearwater`의 인증 설정은 남아 있을 수 있어.
`doctor`로 containerd 등 준비물을 확인한 뒤 **원래 실행 파일과 같은 옵션**으로
`sw up`하는 복구 절차는 [운영 안내](operations.md#정지와-복구)에 있어.
진단하려고 DB·볼륨을 초기화하거나 앱에 `down --all`을 실행할 필요는 없어.

## 7. 개발자가 실제 장애 검사를 재현할 때

단위·HTTP 통합 검사는 일반 사용자로 실행할 수 있어.

```bash
. hack/env.sh
bash hack/t.sh ./...
python3 -m unittest discover -s hack -p 'test_*.py'
bash hack/build.sh
```

`hack/diagnostics-check.sh`는 실제 컨테이너를 만들어 실패시키는 별도 검사야.
**보존할 앱이 없는 격리된 Linux 환경**에서만 root로 실행해.
다른 containerd 소켓을 골라도 네트워크는 자동으로 격리되지 않아. 특히 여러 WSL
배포판이 외부 네트워크 네임스페이스를 공유할 수 있어. 기존 서버와 같은 `sw-br0`,
`sw-svc0`, iptables 이름을 쓰지 않도록 별도 VM 또는 전용 netns를 먼저 준비해야 해.

```bash
# 격리 환경 내부의 root Bash에서 실행하는 예제야.
# 아래 소켓과 kubectl 경로는 그 환경의 실제 경로로 바꿔.
SW_CONTAINERD_SOCKET=/run/shearwater/qa-containerd.sock \
  bash hack/diagnostics-check.sh "$PWD/dist/sw" /absolute/path/to/kubectl
```

소켓 기본값은 `/run/containerd/containerd.sock`, API 포트 기본값은 16456,
테스트 이미지 기본값은 `busybox:1.36`이야. 필요하면 `SW_DIAGNOSTICS_PORT`,
`SW_DIAGNOSTICS_IMAGE`로 바꿀 수 있어. 사전에 `shearwater` 컨테이너·태스크와
전용 네트워크가 비어 있는지 검사하고, 기존 자원을 발견하면 중단해.
이미지 가져오기는 격리 환경의 containerd를 사용하고 패키지는 설치하지 않아.

검사는 이미지 받기 실패, exit 7 반복 종료, 잘못된 hostPath 종류, 없는 실행 파일,
restartPolicy Never의 exit 9, 컨테이너 두 개의 선택·로그 제한을 다뤄.
같은 이름의 Pod를 지우고 다시 만들어 UID별 이벤트 분리도 확인해.
현재/직전 로그, 종료 코드, 안내 문구, JSON과 오류 코드를 실제 API로 대조해.

결과와 각 명령의 stdout/stderr는 새 ext4 디렉터리
`/var/lib/shearwater/check/diagnostics.*/`의 `result.json`, `command-*.json`에 남아.
검사 마지막에는 **그 검사가 시작한 sw의 정확한 옵션**으로 `down --all`해서
검사용 Pod와 전용 네트워크를 내리고 DB·로그·증거는 보존해.
호출자가 준비한 별도 containerd·외부 netns·연결 규칙은 호출자가 정리해야 해.
실행이 강제로 중단됐다면 결과의 `cleanup`과 남은 프로세스를 먼저 확인해.

Go 검사는 모의 런타임/HTTPS 오류·타임아웃·UID 변경·정수 보존을 다루고,
장애 스크립트는 실제 containerd에서의 동작을 다뤄. 둘은 서로 대체하지 않아.
이번 구현의 실제 검증 결과는 [진단 검증 기록](diagnostics-verification.md)에 따로 남겨.
