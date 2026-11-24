# AI에게 Shearwater 운영 절차 알려 주기

이 문서는 **alpha.9부터 제공하는 기능**을 설명해. [설치 안내](install.md)로 받은
실행 파일에서 `sw version`이 `0.1.0-alpha.15`인지 확인해. `chat --approved-plan`과
`--task=diagnose`, 작업별 스킬이 Linux 릴리스 묶음에 함께 들어 있어.
새 클라이언트를 실행한다고 운영 서버가 교체되지는 않아.

압축을 푼 폴더의 `skills/shearwater-ops/SKILL.md`가 시작점이고, 그 옆 `references/`에
배포·업데이트·진단 절차가 있어. 연결한 AI에게 이 시작 파일의 절대 경로를 알려 줘.
이 파일을 읽는 것과 MCP를 연결하는 것은 별개야. 실행 도구는 [MCP 연결 안내](agent-mcp.md)로
설정하고, 스킬은 그 도구로 어떤 순서와 근거를 지켜야 하는지 알려 주는 역할이야.

## 스킬은 무엇이고 왜 필요한가

MCP는 AI가 실제 프로그램을 호출하는 연결이고, 스킬은 **어느 순서로 어떤 도구를 쓰고
어떤 결과를 확인할지 알려 주는 작업 안내서**야. 스킬만 읽어서는 앱이 배포되지 않아.
반대로 도구만 주면 AI가 앱 이름을 Pod 이름으로 착각하거나 계획 저장을 배포 성공으로
설명할 수 있어. 코드의 범위 검사와 작업 지침을 함께 써야 해.

이 구조는 [BeautifulRust](https://github.com/morganlinton/BeautifulRust)의 작업별 스킬과
필요할 때 읽는 참고 문서 구조에서 아이디어를 얻었어. Rust로 이식하거나 그 프로젝트의
성능 주장을 가져온 것은 아니야. Shearwater에서는 배포·업데이트·진단으로 나눴어.

```text
skills/shearwater-ops/
  SKILL.md                 작업 선택과 공통 원칙
  references/deploy.md      새 앱 계획 → 승인 → 적용 → 결과 확인
  references/update.md      고정한 앱의 이미지·복제본 변경
  references/diagnose.md    짧은 근거 조회 → 원인 → 별도 복구 계획
```

한 질문에는 `SKILL.md`와 해당 작업의 참고 문서만 읽어. 전체 설계서·로그를 매번 보내는
방식보다 필요한 지침을 찾기 쉽지만, 지침 자체도 토큰을 사용해. 이 구조가 언제나 더
빠르거나 저렴하다는 보장은 없어. 실제 도구 응답과 최종 상태로 효과를 확인해야 해.

## Claude·Codex·딥시크에서 사용하기

먼저 [MCP 연결](agent-mcp.md)을 완료해. 사용하는 앱이 프로젝트 파일을 읽을 수 있다면
다음처럼 요청할 수 있어.

> `skills/shearwater-ops/SKILL.md`를 읽고 배포 지침을 따라 줘. 연결된 테스트 환경의
> `demo` 이름 공간에 `web`을 `nginx:stable`, TCP 80, 복제본 1로 배포할 계획을 만들어 줘.
> 먼저 계획을 보여 주고 승인 전에는 적용하지 마.

파일 읽기가 없는 MCP 연결에는 호스트의 지침 기능으로 `SKILL.md`와 작업 문서 본문을
전달해. 앱마다 등록 방법이 다르므로 파일을 둔 것만으로 자동 로딩됐다고 생각하지 마.
AI가 실제로 본 지침과 도구 목록을 확인해야 해. 이 저장소는 사용자 전역 설정이나 앱의
스킬 폴더에 자동 설치하지 않아. MCP도 로컬 파일을 임의로 읽어 보내지 않아.

딥시크 공식 명령줄과 MCP의 실제 검증은 로그인된 앱의 공식 실행 경로로 수행했어.
이것이 기존 그래픽 앱의 모든 연결 설정을 수정·검사했다는 뜻은 아니야.

## Qwen 한 질문 대화에서 승인된 작업 이어가기

일반 `--prompt` 대화는 실제 터미널의 `/review`, `/approve` 입력을 받을 수 없어.
이전에는 사용할 수 없는 적용 도구를 보고 같은 거부를 반복할 수 있었어. 개발판은
일반 한 질문 대화에서 그 도구를 숨기고, **이미 로컬에서 승인한 계획 하나**를 선택해
실행을 이어가는 옵션을 제공해. AI가 새로운 승인을 만들지는 않아.

아래 경로·포트·해시는 예시야. 연결 안내에서 준비한 일반 사용자 인증 경로와 실제 값을
사용해. 관리자 서버를 실행하는 root로 AI 대화를 실행하지 않아.

```bash
SW="$HOME/.local/share/shearwater/v0.1.0-alpha.15/sw"
CONFIG=/absolute/path/to/user/config
LISTEN=127.0.0.1:6443
PLAN=EXACT_64_CHARACTER_PLAN_HASH

# 1. 계획 원문을 읽어 대상, 이전 값, 바뀔 값, 경고를 확인한다. 승인하지는 않는다.
"$SW" plan --config-dir="$CONFIG" --listen="$LISTEN" --plan="$PLAN"

# 2. 확인한 그 계획 하나를 로컬에서 승인한다.
"$SW" approve --config-dir="$CONFIG" --listen="$LISTEN" \
  --plan="$PLAN" --confirm="$PLAN"

# 3. AI에게 그 계획 한 번의 적용과 원래 목표 확인만 맡긴다.
"$SW" chat --config-dir="$CONFIG" --listen="$LISTEN" --model=qwen2.5:latest \
  --allow-write --approved-plan="$PLAN" \
  --prompt="승인된 계획을 한 번 적용하고 원래 목표를 확인해 줘. HTTP 미확인은 구분해 줘."

# 4. 아직 준비 중이면 같은 계획을 나중에 다시 검사한다. 다시 적용하지 않는다.
"$SW" verify --config-dir="$CONFIG" --listen="$LISTEN" --plan="$PLAN"
```

직접 `--plan-dir`을 바꿨다면 모든 단계에 같은 값을 붙여. `sw_verify_plan`은 MCP 도구
이름이고, 터미널 명령은 `sw verify --plan=...`야. 둘을 혼동하지 않아.

Windows Ollama를 쓴다면 새 `hack/chat-windows.py`에 `--allow-write --approved-plan HASH`를
추가해. 기존의 `--sw`, `--config-dir`, `--listen`, `--model`, `--prompt`도 필요해.
승인 단계는 같은 WSL 환경의 Linux CLI에서 수행해. 도우미는 대화 이력을 저장하지 않아.

선택된 실행 연결에는 `sw_apply`, `sw_receipt`, `sw_verify_plan` 세 도구만 보여.
각 도구의 계획 해시는 하나로 고정되고, 다른 해시·추가 인자·같은 대화의 두 번째 적용은
거부해. 별도 프로세스에서 재실행해도 기존 실행 기록이 중복 적용을 막아.
만료·파일 변조·다른 서버·승인 없음도 거부하며, 실제 적용 직전 동시 변경을 다시 검사해.
모호한 실패를 만났다면 실행 기록을 먼저 살펴봐. 실행 횟수 제한은 자동 복구 기능이 아니야.

`--target`은 변경 **계획을 만드는 앱**을 고정하고, `--approved-plan`은 검토된 **실행 계획**을
고정해. 두 옵션을 함께 쓰지 않아. 이 장치는 같은 관리자 인증을 가진 다른 프로그램까지
제한하는 서버 권한 체계가 아니야.

## 업데이트와 장애 복구

실전 시험에서 지침을 추가해도 Qwen이 namespace를 빠뜨리고 Pod 이름을 만들어 내는
문제가 반복됐어. 그래서 진단도 프로그램이 앱 대상을 보관하는 경로를 추가했어.

```bash
"$SW" mcp --config-dir="$CONFIG" --listen="$LISTEN" \
  --target=deploy/web -n demo --task=diagnose
```

Qwen은 `mcp` 대신 `chat --model=qwen2.5:latest --prompt="이 앱의 장애 근거를 확인해 줘"`로
시작하고 같은 대상 옵션을 붙여. Windows 도우미도 `--task diagnose`를 전달해.
모델에는 인자 없는 `sw_target_diagnose` 하나만 보여. 프로그램이 Deployment와 ReplicaSet의
소유 UID를 따라 실제 Pod를 찾아서 이름 추측을 없애. 같은 라벨이나 이름만으로 묶지 않아.

응답에는 현재 세대, 준비된 Pod 수, 실패 Pod의 이미지·대기 이유·오류가 들어 있어.
같은 저장소 시점의 목록만 사용하고, 대상 UID/세대가 바뀌면 `stale`로 알린 뒤 다른 앱의
상세 근거를 섞지 않아. 로그·환경 변수·주석은 보내지 않아. 이 제한은 권한 격리가 아니야.

실패를 먼저 최대 4 Pod·각 3 컨테이너까지 보여 주고 생략 수를 명시해. 긴 오류는
앞뒤를 보존하고 `truncated=true`로 알려 주며, `answer` JSON도 8 KiB 안으로 제한해.
원문 전체나 로그가 필요하면 해당 Pod를 명시한 일반 진단을 별도로 해야 해.
`readyPods>0`은 실제 웹 접속 보장이 아니고, 항상 HTTP 미확인을 구분해.

이미지 변경은 [집중 모드](focused-tools.md)의 `--task=image`로 시작해. AI가 앱 이름이나
UID를 매 호출마다 옮겨 쓰지 않고 이미지 값만 제안하게 해. 그 결과는 아직 적용하지 않은
계획이야. 원문을 검토하고 승인한 뒤 별도 실행 연결에서 적용해.

업데이트가 멈추면 Deployment의 요약을 보고 그 앱에 속한 실패한 Pod를 찾아. Pod 이름은
보통 Deployment 이름에 해시가 붙으므로 직접 추측하지 않아. 잘못된 이미지라면 컨테이너가
시작되지 않아 앱 로그가 없을 수 있어. 이때는 대기 이유와 이미지 받기 오류가 더 직접적인 근거야.

이전 Pod가 준비되어 서비스 중이어도 새 버전의 배포는 실패할 수 있어. `Ready`와 실제 HTTP
응답도 달라. 또한 Kubernetes의 `progressDeadlineSeconds`를 저장한다고 해서 Shearwater가
자동 시간초과를 처리하는 것은 아니야. [호환표](compatibility.md)의 한계를 함께 봐.

복구는 현재 대상에 대한 **새 계획**이야. 확인된 정상 이미지로 돌아가는 계획을 다시 검토하고
승인해. 정상 Pod를 지우거나 실패 계획을 재적용하는 것은 이미지 오류를 해결하지 못해.

## 어떤 검사가 이 절차를 지키는가

- `cmd/sw/chat_resume_test.go`: 승인·신원·만료·변조, 다른 해시, 추가 호출, 재시도 제한.
- `hack/chat-resume-check.py`: 실제 임시 API와 모델 대역으로 승인 전 차단부터 실행 기록,
  원래 목표 검사, 새 프로세스의 중복 적용 차단까지 확인.
- `hack/chat-check.py`: 기존 대화형 터미널 검토·승인 흐름.
- `hack/chat-windows-test.py`: Windows 인자 전달과 잘못된 옵션 조합 거부.

이 검사들은 모델의 지능을 측정하지 않아. 실제 Qwen·딥시크 실험은 별도 컨테이너 환경에서
계획·승인·배포·업데이트·이미지 장애·복구를 실행하고 도구 원문과 HTTP를 대조해야 해.
모델이 맡은 단계와 운영자가 승인·HTTP 검사한 단계도 따로 기록해야 공정한 결과가 돼.
