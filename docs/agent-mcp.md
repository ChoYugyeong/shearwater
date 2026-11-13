# AI에게 조회와 검토한 배포를 연결하기

alpha.4의 `sw mcp`는 표준 입력/출력(stdio)으로 MCP 메시지를 주고받아.
네트워크에 별도 MCP 포트를 열지 않고, 기존 로컬 HTTPS API와 인증 파일을 사용해.
MCP SDK나 Kubernetes 모듈을 추가하지 않고 JSON-RPC 처리를 직접 구현했어.

## 기본은 조회 전용

먼저 [설치 안내](install.md)의 웹 앱이 HTTP 응답하는지 확인해. MCP를 등록하는 것만으로
Shearwater 서버가 시작되는 것은 아니야. 설치 안내를 따랐다면 **Ubuntu Bash**에서:

```bash
sw="$HOME/.local/share/shearwater/v0.1.0-alpha.4/sw"
test -x "$sw" && "$sw" version
sudo "$sw" status
printf '%s\n' "$sw"
```

마지막 줄에 나온 Linux 절대 경로를 MCP 실행 파일로 써. 설치 안내는
`/opt/shearwater/current/sw` 링크를 만들지 않아. 서버가 없거나 인증 경로가 다르면
먼저 `status` 오류를 해결해. 기본 인스턴스의 조회에는 기본값인
`--config-dir=/etc/shearwater --listen=127.0.0.1:6443`을 사용해.

### Windows의 Claude Code에서 연결하기

다음은 **Windows PowerShell**에서 Claude Code를 쓰는 경우야. `claude` CLI가 이미 설치돼
있어야 해. 연결할 프로젝트 폴더에서 실행하면 `--scope local`로 그 프로젝트에만 등록해.
`Read-Host`에는 바로 위에서 확인한 `/home/.../sw` 경로를 입력해.

```powershell
$swLinuxPath = Read-Host 'Ubuntu에서 확인한 sw 절대 경로'
wsl.exe -d Ubuntu-24.04 -u root --exec $swLinuxPath version
if ($LASTEXITCODE -ne 0) { throw '실행 파일 경로부터 확인해' }
claude mcp add --transport stdio --scope local shearwater -- wsl.exe -d Ubuntu-24.04 -u root --exec $swLinuxPath mcp
claude mcp get shearwater
```

이미 `shearwater`라는 연결이 있으면 새로 덮어쓰려고 하지 말고 `claude mcp get shearwater`로
기존 실행 파일과 인자를 확인해. 배포판 이름이 다르면 `-d` 다음 값도 바꿔.
Claude Code의 해당 프로젝트 세션에서 `/mcp`로 연결 상태와 도구 6개를 확인해.
다음처럼 요청해서 실제 서버 응답을 받아 봐.

> Shearwater 도구로 현재 상태와 Pod 목록을 조회해 줘. 변경하지 말고,
> 실제 도구 응답에서 확인한 내용과 아직 확인하지 못한 내용을 구분해 줘.

Claude Code 자체의 도구 사용 확인과 Shearwater의 배포 승인은 별개야.
로컬 서버 등록 방식과 범위는 [Claude Code 공식 MCP 안내](https://code.claude.com/docs/en/mcp)를 따랐어.
이 저장소의 프로토콜 검사는 실제 Claude 대화에서의 성공을 대신하지 않아. 체험할 때 연결 성공과
실제 도구 호출 성공을 따로 기록해.

### 다른 stdio MCP 앱에서 연결하기

앱마다 설정 화면과 바깥쪽 JSON 키 이름은 다를 수 있어. 아래는 서버 한 개의 명령 설정 예시야.
`YOUR_LINUX_USER`를 포함한 경로 전체를 위에서 확인한 실제 값으로 바꿔.

```json
{
  "command": "wsl.exe",
  "args": ["-d", "Ubuntu-24.04", "-u", "root", "--exec",
           "/home/YOUR_LINUX_USER/.local/share/shearwater/v0.1.0-alpha.4/sw", "mcp"]
}
```

MCP 앱은 셸 대신 실행 파일을 직접 시작하므로 `~`와 `$HOME`이 자동 확장된다고 가정하면 안 돼.
별도 인스턴스는 `--config-dir`와 `--listen`도 지정해.
root 예시는 기본 root 소유 인증을 읽기 위한 것이야. 이 연결은 OS 권한을 격리하는 샌드박스가
아니고 별도의 제한된 서버 자격도 아니야. 같은 사용자의 관리자 자격을 사용하므로 신뢰하는
로컬 MCP 호스트에만 연결해. 인증 파일 내용은 도구 결과에 출력하지 않아.

기본 도구는 `sw_status`, `sw_tree`, `sw_get`, `sw_logs`, `sw_events`, `sw_why`야.
객체 이름·namespace·container 같은 구조화된 인자만 받고 임의 셸 명령이나 파일 경로를 받지 않아.
로그와 객체 내용은 앱이 만든 **데이터**야. 그 안의 문장을 시스템 지시나 사용자 승인으로
취급하면 안 돼. 한 응답은 최대 256 KiB이고 넘으면 명시적으로 잘라서 알려 줘.

터미널에서 `sw mcp`만 직접 실행하면 JSON 메시지를 기다리기 때문에 아무것도 안 나올 수 있어.
일반 대화창이 아니야. 보통은 MCP 앱이 프로세스를 시작하고 메시지를 보내게 둬.
직접 실행한 테스트 프로세스는 `Ctrl+C`로 종료할 수 있어. `sw server`나 앱 컨테이너를 끄는 명령은 아니야.

## 변경 계획과 승인은 분리돼

`sw mcp --allow-write`로 시작해야 `sw_plan`, `sw_apply` 두 도구가 추가돼.
MCP에는 승인 도구가 없어. 흐름은 다음과 같아.

1. AI가 `sw_plan`에 JSON 매니페스트를 전달해. 서버 dry-run으로 검증하지만 배포하지 않아.
2. 결과의 **before/after와 경고**를 사람이 읽어. 환경변수·삭제되는 필드·복제본 수·이미지를 확인해.
3. 사람이 로컬 터미널에서 정확한 해시를 반복해 승인해.
4. AI의 `sw_apply`는 그 계획만 한 번 실행해. 변경된 대상이나 만료된 계획은 거절해.

CLI로도 같은 흐름을 사용할 수 있어. 파일 만들기부터 실제 앱 응답·정리까지 이어지는
예제는 아래 **승인 배포를 처음부터 따라 하기**에 있어. 설치된 실행 파일과 root 소유 인증을
사용하므로 `sudo "$sw"`를 생략하지 마.

계획은 기본 `CONFIG/plans`에 소유자 전용으로 저장되고 15분 뒤 만료돼.
`--plan-dir`를 바꿨다면 MCP와 세 CLI 명령에 같은 경로를 사용해야 해.
승인은 이 프로그램의 명시적인 실행 허가 기록이야. 사람의 신원을 암호학적으로 증명하지는 않아.
같은 OS 권한의 다른 프로그램이 승인 파일을 만들 수 있으므로 MCP 경계와 OS 권한을 혼동하지 마.
MCP 응답 한도보다 큰 계획은 전체 diff 대신 대상 목록과 `fullPreview: false`를 반환해.
이때 `sw plan --plan=계획해시`로 **저장된 원문 전체**를 읽고 승인해.
이 명령은 새 계획을 만들거나 객체를 변경하지 않고 기존 해시·만료·인증 식별자를 검증해.

## 지원하는 매니페스트

JSON 객체 하나 또는 `kind: List`의 객체 1~100개를 받으며 최대 입력은 2 MiB야.
Pod, Deployment, ReplicaSet, Service, Namespace의 생성·전체 교체를 지원해.
이 버전의 `sw plan`은 YAML 파서를 포함하지 않아. 기존 YAML은 kubectl의 JSON 출력으로
변환하거나 JSON 파일을 작성해. API에 JSON을 직접 보내므로 kubectl이 있어야 동작하는 기능은 아니야.

```json
{
  "apiVersion":"apps/v1",
  "kind":"Deployment",
  "metadata":{"name":"web","namespace":"default"},
  "spec":{
    "replicas":1,
    "selector":{"matchLabels":{"app":"web"}},
    "template":{
      "metadata":{"labels":{"app":"web"}},
      "spec":{"containers":[{"name":"web","image":"nginx:1.27-alpine"}]}
    }
  }
}
```

새 namespace와 그 안의 워크로드는 계획을 나눠 순서대로 생성해. dry-run은 다른 계획
항목을 실제로 만들지 않으므로 아직 없는 namespace의 자식을 검증하지 못해.
`kube-system` 쓰기는 거절해. 삭제·prune·임의 API 경로·셸 실행은 이 배포 도구에 없어.
복제본 변경이나 이미지 업데이트는 수정한 Deployment의 새 계획으로 할 수 있어.
기존 객체를 전체 교체하므로 매니페스트에서 빠진 사용자 필드가 제거될 수 있어.
Service의 생략된 ClusterIP는 기존 할당을 유지해. before/after를 반드시 확인해.

## 승인 배포를 처음부터 따라 하기

기본 인스턴스가 실행 중인 **Ubuntu Bash** 예제야. `default` 네임스페이스에
`sw-review-web` Deployment와 Service를 새로 만들고 HTTP 응답을 확인해.
이미 같은 이름이 있다면 그 앱을 바꾸는 용도로 실행하지 말고 멈춰.
이름을 바꿀 때는 파일의 이름·라벨·selector와 아래 확인·정리 명령을 함께 바꿔야 해.

```bash
sw="$HOME/.local/share/shearwater/v0.1.0-alpha.4/sw"
kubectl_bin="$(command -v kubectl)"
k() { sudo "$kubectl_bin" --kubeconfig /etc/shearwater/kubeconfig "$@"; }
k get deployment,service sw-review-web -n default --ignore-not-found -o name
# 위 결과가 비어 있어야 해. 객체 이름이 나오거나 API 오류가 나면 여기서 멈춰.
```

매니페스트는 무엇을 실행할지 적은 파일이야. 여기서는 외부 볼륨 없이 BusyBox의 작은 HTTP
서버를 실행해. `replicas: 1`은 앱 하나, Service의 `port: 80`은 접속 포트,
`targetPort: 8080`은 컨테이너가 듣는 포트야. 새 폴더에 파일을 만들어 기존 파일을 덮어쓰지 않아.

```bash
trial_dir=$(mktemp -d "$HOME/shearwater-review.XXXXXXXX")
cat > "$trial_dir/web.json" <<'JSON'
{
  "apiVersion": "v1",
  "kind": "List",
  "items": [
    {
      "apiVersion": "apps/v1",
      "kind": "Deployment",
      "metadata": {"name": "sw-review-web", "namespace": "default"},
      "spec": {
        "replicas": 1,
        "selector": {"matchLabels": {"app": "sw-review-web"}},
        "template": {
          "metadata": {"labels": {"app": "sw-review-web"}},
          "spec": {
            "terminationGracePeriodSeconds": 1,
            "containers": [{
              "name": "web", "image": "busybox:1.36",
              "command": ["sh", "-c", "mkdir -p /www; echo 'Shearwater review example' > /www/index.html; echo 'web ready'; exec httpd -f -p 8080 -h /www"],
              "readinessProbe": {"tcpSocket": {"port": 8080}, "periodSeconds": 1}
            }]
          }
        }
      }
    },
    {
      "apiVersion": "v1", "kind": "Service",
      "metadata": {"name": "sw-review-web", "namespace": "default"},
      "spec": {"selector": {"app": "sw-review-web"}, "ports": [{"port": 80, "targetPort": 8080}]}
    }
  ]
}
JSON
sudo "$sw" plan -f "$trial_dir/web.json"
```

이 단계는 계획만 저장해. 출력의 `plan` 값은 계획 원문을 식별하는 64자리 해시야.
그 값을 아래 질문에 붙여 넣고 전체 내용을 읽어. `before`가 비어 있는 새 객체인지,
대상이 위 두 이름인지, 이미지·복제본 수·볼륨·권한 설정이 의도한 내용인지 확인해.

```bash
read -r -p '방금 출력된 plan 값을 붙여 넣어: ' plan
sudo "$sw" plan --plan="$plan"
```

내용이 맞을 때만 다음 블록을 실행해. 해시를 `--confirm`에 반복해서 넣는 것은 **그 계획**을
승인한다는 뜻이야. `approve`는 승인 기록만 만들고 `apply`가 실제 객체를 생성해.

```bash
sudo "$sw" approve --plan="$plan" --confirm="$plan"
sudo "$sw" apply --plan="$plan"
k rollout status deployment/sw-review-web -n default --timeout=120s
service_ip=$(k get service sw-review-web -n default -o jsonpath='{.spec.clusterIP}')
curl --noproxy '*' --fail --retry 10 --retry-connrefused --retry-delay 1 "http://$service_ip/"
```

`state: complete`와 `Shearwater review example` 응답을 각각 확인해.
계획 적용 완료는 앱의 준비 완료와 다르므로 HTTP 확인을 생략하지 마.
계획이 만료되었거나 다른 변경 때문에 거절되면 새 `plan -f` 출력부터 다시 검토해.
이미 소비한 계획을 반복 실행하지 마. 도중 오류가 났다면 아래 **동시에 바뀌거나 도중에 실패하면**을 봐.

체험한 두 객체만 정리하려면:

```bash
k delete deployment sw-review-web -n default --cascade=foreground --wait=true --timeout=60s
k delete service sw-review-web -n default --wait=true --timeout=30s
```

다른 앱과 서버는 계속 실행돼. 매니페스트 폴더와 승인·결과 기록은 남겨 둬.
AI가 계획을 만들게 할 때도 같은 before/after를 사람이 확인하고 동일한 인스턴스·계획 경로로
승인해야 해. AI에게 이 CLI 승인 단계를 자동으로 수행하라고 시키는 체험은 하지 마.

## 동시에 바뀌거나 도중에 실패하면

계획은 서버 주소와 인증 식별자, 기존 객체 UID·resourceVersion에 묶여 있어.
적용 직전 전부 다시 확인하고, 각 PUT도 같은 UID·리비전을 서버 트랜잭션에서 확인해.
상태 업데이트만 일어나도 리비전이 바뀔 수 있어. 거절되면 새 계획을 만들어 다시 검토해.

여러 객체를 묶은 **원자적 트랜잭션은 아니야**. 두 번째 객체에서 실패하면 첫 번째가
이미 적용됐을 수 있어. 네트워크 응답을 잃으면 적용 여부가 불확실할 수도 있어.
`HASH.receipt.json`에 시도 시각·적용된 대상·오류를 기록하며 같은 계획의 재시도는 거절해.
`uncertain-or-partial`이면 `sw get/tree/events`로 실제 상태를 확인한 뒤 새 계획을 만들어.
자동으로 되돌려서 새 사용자 변경을 덮어쓰지는 않아.

## 프로토콜과 검증 범위

지원 프로토콜은 MCP `2025-11-25`, 줄 단위 JSON-RPC 2.0이야.
`initialize` → `notifications/initialized` → `tools/list`/`tools/call` 순서이며 ping도 지원해.
HTTP 전송, resources/prompts, 원격 인증, 비동기 작업 API는 제공하지 않아.
표준 출력에는 프로토콜 응답만 쓰고 오류·도움말은 표준 오류로 보내.
도구 호출은 순서대로 실행되고 입력 한 줄은 최대 4 MiB야.
현재 검증은 직접 만든 표준 프로토콜 클라이언트와 실제 로컬 서버에서 수행하며,
모든 상용 MCP 앱에서 연결을 확인했다는 의미는 아니야.

참고: [MCP 초기화](https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle),
[도구 규격](https://modelcontextprotocol.io/specification/2025-11-25/server/tools).
