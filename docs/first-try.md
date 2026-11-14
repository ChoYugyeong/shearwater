# 처음 써 보고 판단하기

Shearwater alpha.6가 내 작업에 도움이 되는지 직접 확인하는 안내야.
한 대의 Linux 환경에서 작은 앱을 띄우고, 실패 이유를 읽고, 변경 계획을 승인해 배포하는
흐름을 체험해. **실제 서비스나 중요한 데이터를 옮기는 절차는 아니야.**

이 안내는 alpha.6 실행 파일 기준이야. 압축의 `INSTALL-ko.md`와 저장소의 [설치 안내](install.md)를 따라 해. 정확한 기능 범위는
[호환표](compatibility.md)를 함께 봐. 끝나면 [체험 기록](first-try-feedback.md)을 남겨 줘.

## 시작할 수 있는 환경인지 확인하기

- 검증 대상은 WSL 2 / Ubuntu 24.04 / amd64야. 다른 Linux에서도 동작한다고 검증한 것은 아니야.
- containerd, runc, 네트워크 도구와 관리자 권한이 필요해. 실행 파일 하나를 받는 것과
  전체 준비물이 하나라는 것은 달라. Go나 소스 빌드는 필요 없어.
- 기존 Kubernetes·Docker·다른 Shearwater 앱과 함께 쓰는 PC라면 기존 설정을 지우거나
  런타임을 덮어쓰면서 진행하지 마. 별도 WSL 배포판도 네트워크를 공유할 수 있어.
- root 권한을 사용하는 개인 실험용이야. 여러 사람의 신뢰하지 않는 앱을 격리하는 도구로
  평가하지 마. 설정한 일부 Kubernetes 필드가 실제로 실행되지 않는 범위는 호환표에 있어.

준비물이 없으면 [설치 안내 1절](install.md#1-wsl과-준비물-확인)에서 먼저 확인해.
기존 환경과 충돌해 멈춘 것도 중요한 체험 결과야. 무리해서 통과시키지 않아도 돼.
시간은 **환경 준비**와 **다운로드 이후 체험**을 나눠 적어 줘. 아직 설치 시간 목표를 달성했다고
주장하지 않아.

## 체험 A: 웹 앱 한 개 띄우기

[설치 안내 2~5절](install.md#2-다운로드체크섬-확인설치)을 순서대로 따라 해.
Ubuntu Bash와 Windows PowerShell을 구분하고, 안내에 있는 실제 실행 파일 경로를 사용해.

이 세 가지가 모두 보이면 A 성공이야.

1. 설치된 실행 파일의 `"$sw" version`에 `0.1.0-alpha.6`가 나온다.
2. `kubectl` 조회에서 `web` Deployment의 Pod가 Ready다.
3. **Ubuntu 안에서** Service IP에 `curl`하면 `Welcome to nginx!`가 나오고 로그에 요청이 남는다.

Ready 표시 하나만으로 성공이라고 기록하지 마. 앱의 HTTP 응답까지 확인해.
Service IP는 내부 주소라 Windows 브라우저에서 바로 여는 흐름은 제공하지 않아.
웹에서 공개 접속할 주소나 도메인·HTTPS를 자동으로 만드는 기능도 이번 체험 범위에 없어.

## 체험 B: 일부러 실패한 작은 앱 진단하기

A에서 켠 서버는 유지해. 다음 예제는 `default/sw-try-crash`라는 **새 Pod 하나**만 만들어.
이 Pod는 메시지를 출력하고 종료 코드 7로 끝나도록 정해져 있어. 기존 `web` 앱은 수정하지 않아.

**Ubuntu Bash**에서 필요한 경로를 다시 선언해. 설치 위치나 kubeconfig가 다르면 실제 값으로 바꿔.

```bash
sw="$HOME/.local/share/shearwater/v0.1.0-alpha.6/sw"
export PATH="$HOME/.local/share/shearwater-tools/kubectl-v1.36.6:$PATH"
kubectl_bin="$(command -v kubectl)"
k() {
  [ -n "$kubectl_bin" ] || { echo 'kubectl을 못 찾았어. 설치 안내 3절부터 확인해.' >&2; return 1; }
  sudo "$kubectl_bin" --kubeconfig /etc/shearwater/kubeconfig "$@"
}
k get pod sw-try-crash -n default --ignore-not-found -o name
# 이름이 나오거나 API 오류가 나면 멈춰. 기존 객체를 덮어쓰는 실험이 아니야.
```

PATH 줄은 설치 안내에서 받은 kubectl을 새 터미널에서도 찾도록 해.
`kubectl_bin`이 비어 있거나 sudo 실행 파일 오류가 나면 [kubectl 준비](install.md#3-kubectl-준비)를
확인하고 다음 생성 단계로 넘어가지 마.

충돌이 없을 때 새 파일을 만들고 그 Pod만 생성해.

```bash
crash_dir=$(mktemp -d "$HOME/shearwater-crash.XXXXXXXX")
cat > "$crash_dir/crash.json" <<'JSON'
{
  "apiVersion": "v1", "kind": "Pod",
  "metadata": {"name": "sw-try-crash", "namespace": "default"},
  "spec": {
    "restartPolicy": "Never",
    "terminationGracePeriodSeconds": 1,
    "containers": [{
      "name": "demo", "image": "busybox:1.36",
      "command": ["sh", "-c", "echo 'demo: intentional exit 7'; exit 7"]
    }]
  }
}
JSON
k create -f "$crash_dir/crash.json"
k wait --for=jsonpath='{.status.phase}'=Failed pod/sw-try-crash -n default --timeout=90s
sudo "$sw" get pod/sw-try-crash -n default
sudo "$sw" logs pod/sw-try-crash -n default
sudo "$sw" events --for=pod/sw-try-crash -n default
sudo "$sw" why pod/sw-try-crash -n default
```

`Failed`는 이 예제에서 기대한 결과야. 로그에서 `demo: intentional exit 7`, 진단에서 종료 코드
7과 다시 확인할 근거를 찾으면 B 성공이야. 이벤트는 상태 변화 기록이며 모든 앱 문제의 원인을
직접 설명하지는 않아. `why` 명령의 종료 코드 0도 앱 정상이라는 뜻은 아니야.

`ImagePullBackOff`이면 예제의 종료 코드 진단까지 도달하지 못한 거야. 실패한 단계와
이미지 다운로드 오류를 기록해. 기다리는 시간을 임의로 성공으로 바꾸지 마.

확인 후 테스트 Pod만 정리해.

```bash
k delete pod sw-try-crash -n default --wait=true --timeout=30s
```

## 체험 C: AI 조회와 승인 배포 연결하기 — 선택

MCP 앱을 쓰는 사람은 [AI 연결 안내](agent-mcp.md)의 조회 전용 설정부터 따라 해.
Claude Code를 Windows에서 쓰는 경우의 명령과 실제 설치 경로를 확인하는 단계가 있어.

1. MCP 연결 표시가 정상인지 확인한다.
2. AI에게 Shearwater 도구로 상태와 Pod 목록을 읽게 한다. 실제 도구 호출이 있었는지 확인한다.
3. AI 설명을 `sw status/get`의 실제 출력과 비교한다. 추측과 관측을 섞는지 적는다.
4. [승인 배포 예제](agent-mcp.md#승인-배포를-처음부터-따라-하기)로 계획 내용을 읽고,
   직접 승인한 뒤 실제 Service HTTP 응답을 확인한다.

AI 연결을 못 했더라도 CLI 승인 배포는 별도로 체험할 수 있어.
CLI만 성공했다면 **MCP 연결 성공**으로 기록하지 마. AI가 계획을 만들고 적용한 것과
사람이 CLI에서 계획·승인·적용한 것도 구분해.

AI에게 승인 명령까지 자동으로 실행시키지 마. 계획의 대상·이미지·복제본·볼륨·삭제되는 필드를
읽고 사람이 결정하는 경험을 평가하는 거야. MCP는 운영체제 관리자 권한을 제한하는 보안
경계가 아니야. 같은 관리자 권한을 가진 다른 도구까지 통제해 주는 것은 아니야.

## 끝낼 때

체험 B와 C에서 만든 객체는 각 안내의 정확한 이름으로 정리해.
A의 `web` 예제까지 끝낼 때는 [설치 안내 6절](install.md#6-재시작과-종료)을 따라 해.
다른 앱을 함께 운영한다면 `down --all`을 사용하지 마. 다른 앱도 중단되는 명령이야.
배포 파일·계획 기록·로그는 결과를 확인할 수 있도록 남겨도 돼. 다른 사람에게는 아래 양식의
필요한 부분만 보내고 토큰·kubeconfig·개인 앱 로그 전체를 보내지 마.

## 체험을 진행하는 사람에게

처음에는 설명을 덧붙이지 말고 이 문서와 릴리스 링크만 전달해. 막히면 어디에서 멈췄는지
먼저 기록하고, 도움을 준 뒤에는 **도움 후 성공**으로 표시해. 실패하거나 중단한 체험도 남겨.
개발자의 재현은 `개발자 점검`, AI의 재현은 `에이전트 점검`으로 표시해.
둘 다 외부 사용자 수에 포함하지 마.

첫 참가자 3~5명의 목적은 인기나 시장 점유율을 추정하는 것이 아니야. 반복해서 막히는 곳과
실제로 쓸 이유를 찾는 작은 탐색이야. 가능하면 처음 며칠 뒤 다시 물어봐.

- 설명 없이 첫 HTTP 응답까지 도달했는가?
- 실패 진단과 계획 검토가 다음 행동을 결정하는 데 도움이 됐는가?
- 기존 방식과 비교해 다시 쓸 이유가 있었는가?
- 일주일 뒤 실제로 다시 썼는가? 아직 일주일이 안 지났다면 미확인으로 둔다.

결과는 [체험 기록 양식](first-try-feedback.md)에 적어. 성공률과 소요 시간은 관찰한
참가자·환경·도움을 받은 정도와 함께 보고하고, 실행하지 않은 검사를 통과로 표시하지 마.
