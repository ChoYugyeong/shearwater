# AI에게 한 앱과 필요한 도구만 맡기기

이 기능은 **alpha.8부터 제공해.** [설치 안내](install.md)에서 alpha.12 실행 파일을 받고
`sw version`으로 확인해. 클라이언트만 새 버전으로 실행해 기존 API에 연결할 수도 있어.
새 실행 파일을 받는 것만으로 운영 서버가 자동 업데이트되지는 않아.

## 왜 필요한가

AI에게 도구를 많이 주면 상황마다 맞는 도구와 인자를 골라야 해. 앱이 `bench`라는
이름 공간에 있는데 조회할 때 `default`를 넣으면 엉뚱한 곳을 읽을 수 있어.
사용자가 고른 앱을 프로그램이 기억하면 이 선택을 매 호출마다 반복할 필요가 없어.

집중 모드는 **사람이 먼저 앱과 작업을 선택하고, AI가 그 범위 안에서 값과 설명을 제안**하는
방식이야. 예를 들어 `web`의 이미지만 바꾸는 작업에서는 모델이 이미지 이름만 넘겨.
대상의 이름 공간·이름·UID·설정 세대와 컨테이너 이름은 연결 프로그램이 보관해.

| 작업 | AI가 보는 도구 | AI가 넣는 인자 |
| --- | --- | --- |
| 준비 상태 확인 `inspect` | `sw_target_status` | 없음 `{}` |
| 이미지 변경 계획 `image` | 위 도구 + `sw_target_image` | `image` 하나 |
| 복제본 수 계획 `scale` | 상태 도구 + `sw_target_scale` | `replicas` 하나, 0~1000 |

도구 설명만 숨기는 기능이 아니야. 목록에서 제외한 `sw_get`, `sw_plan`, `sw_apply` 등을
직접 호출해도 거부해. 상태 도구에 다른 `namespace`나 `resource`를 끼워 넣어도 거부해.
현재 지원 대상은 **기존 Deployment 한 개**야. 새 앱 만들기, Pod 상세 로그, Service,
소스 이미지 빌드는 [일반 연결](agent-interface.md)의 다른 흐름을 사용해.

**alpha.9 추가:** `--task=diagnose`는 고정한 Deployment의 실제 소유 Pod를 찾아
`sw_target_diagnose` 하나로 오류 근거를 읽어. 쓰기 허용 없이 사용할 수 있고,
alpha.12 실행 파일이 필요해. 자세한 사용법은 [작업별 운영 안내](agent-skills.md)를 봐.

이 기능은 이 클라이언트 연결의 실수 방지 장치야. 서버 RBAC나 다른 OS 프로그램까지
제한하는 권한 격리는 아니야. 같은 관리자 인증을 가진 별도 CLI는 다른 리소스에도 접근할 수 있어.

## 처음 사용하기

먼저 [설치 안내](install.md)로 alpha.12 실행 파일을 준비하고, [MCP 인증 안내](agent-mcp.md)에
따라 일반 사용자 소유 인증 경로를 준비해. 아래 `SW`와 `CONFIG`는 실제 **Linux 절대 경로**로
바꿔. 소스 폴더의 실행 파일과 운영 서버 실행 파일을 혼동하지 마.

```bash
SW=/absolute/path/to/new/sw
CONFIG=/absolute/path/to/user/config
```

대상을 아직 모르면 일반 CLI의 `observe`로 이름 공간과 이름을 먼저 확인해.

```bash
"$SW" observe --config-dir="$CONFIG"
```

그 다음 앱을 하나 선택해. 아래 `bench`와 `web`은 예시 이름이야.

### Claude·Codex·DeepSeek 앱의 MCP 연결

기존 MCP 등록의 실행 인자에 집중 모드 옵션을 추가해. 앱 자체의 등록 방법은
[MCP 연결 안내](agent-mcp.md)를 따르고, 등록 후 연결을 새로 시작해.

```bash
"$SW" mcp --config-dir="$CONFIG" --target=deploy/web -n bench --task=inspect
```

이 연결에는 상태 도구 하나만 보여야 해. “이 앱의 준비 상태와 확인하지 않은 부분을 알려줘”라고
말해서 `sw_target_status`가 실제 호출되는지 확인해. 이 모드에는 `sw_capabilities`나
`sw_observe`가 없으므로 일반 모드용 연결 확인 문장을 그대로 사용하지 않아.

이미지 계획을 만들 때는 연결의 작업 종류를 바꾸고 새로 시작해.

```bash
"$SW" mcp --config-dir="$CONFIG" --allow-write \
  --target=deploy/web -n bench --task=image --target-container=app
```

“app의 이미지를 nginx:stable로 바꾸는 계획을 만들어 줘”라고 요청할 수 있어.
컨테이너가 여러 개면 `--target-container`가 필수야. 하나면 그 컨테이너를 선택해.
`--allow-write`는 이 모드에서 계획 파일 저장을 허용해. 모델에 승인·적용 도구를 주지는 않아.

복제본 수를 바꾸는 연결은 `--task=scale`로 만들고 `--target-container`를 빼.
외부 MCP에서 만든 계획은 같은 인증·계획 폴더를 사용하는 로컬 CLI에서 검토·승인·적용해.

```bash
"$SW" plan --config-dir="$CONFIG" --plan=PLAN_HASH
"$SW" approve --config-dir="$CONFIG" --plan=PLAN_HASH --confirm=PLAN_HASH
"$SW" apply --config-dir="$CONFIG" --plan=PLAN_HASH
"$SW" verify --config-dir="$CONFIG" --plan=PLAN_HASH
```

`PLAN_HASH`는 방금 받은 정확한 해시로 바꿔. 따로 `--listen`이나 `--plan-dir`을 썼다면
네 명령에도 같은 값을 사용해. 계획은 저장 당시의 객체 버전과 연결에 묶이고, 만료되거나
대상이 바뀌면 적용을 거부해. 계획 파일을 고쳐 재사용하지 말고 새 계획을 검토해.

### 내장 Qwen 대화

Linux에서 Ollama가 실행 중이면 다음처럼 시작해. 모델은 이미 설치된 이름을 사용해.

```bash
"$SW" chat --model=qwen2.5:latest --config-dir="$CONFIG" \
  --target=deploy/web -n bench --task=inspect

"$SW" chat --model=qwen2.5:latest --config-dir="$CONFIG" --allow-write \
  --target=deploy/web -n bench --task=image --target-container=app
```

이미지 계획 응답을 받은 뒤 실제 터미널에 `/review`를 입력하면 프로그램이 계획 원문을
보여 줘. 확인한 다음 `/approve`를 입력하면 그 계획을 한 번 적용하고 실행 기록과 준비 상태
검증을 출력해. 적용은 CLI가 직접 수행하므로 AI에게 적용 도구를 추가로 열지 않아.
이 집중 대화에서는 해당 세션에서 만든 계획만 검토할 수 있어.

`/reset`은 대화와 승인 선택을 지워도 처음 고른 앱·원래 UID·세대를 바꾸지 않아.
자기 계획을 적용해 세대가 바뀐 뒤에도 원래 목표는 그대로야. 배포 결과는 `verify --plan`의
실행 기록으로 확인하고, 다른 설정을 변경하려면 `/quit` 후 새 세션을 시작해.
이 차이를 지키면 “나중에 다른 앱이 정상”인 것을 “원래 배포 성공”으로 오해하지 않아.

Qwen이 Windows에 있으면 기존 연결 도우미에도 같은 옵션을 줄 수 있어.

```powershell
python .\hack\chat-windows.py `
  --sw /absolute/linux/path/sw --config-dir /absolute/linux/user/config `
  --model qwen2.5:latest --target deploy/web -n bench --task inspect `
  --prompt "고정한 앱의 준비 상태와 확인하지 않은 부분을 알려줘"
```

Windows 도우미는 한 질문의 도구 호출과 답변을 전달해. 그 창의 대화형 승인은 지원하지 않아.
계획을 만들려면 `--task image --target-container app --allow-write`를 사용하고 Linux CLI에서
검토·승인해. 이 도우미는 모델이나 포트·인증을 새로 설치하지 않아.

alpha.9 도우미는 검토·승인 뒤 `--target`을 빼고 `--approved-plan HASH`로 새
실행 연결을 시작할 수 있어. 선택한 계획만 적용·검증하며 스스로 승인하지 않아.
[전체 절차](agent-skills.md)에서 검토·승인·실행의 역할을 먼저 확인해.

## 무엇을 고정하고 무엇을 새로 읽나

시작 때 선택한 Deployment 한 개를 조회해 UID와 generation을 한 번 저장해.
UID는 객체의 신분증이고 generation은 원하는 설정의 세대야. 매 도구 호출에서는 같은
이름의 **현재 객체**를 다시 조회하고 처음 저장한 값과 비교해.

| 상황 | 상태/동작 |
| --- | --- |
| 원래 UID·세대이고 컨트롤러가 모든 준비 조건을 보고함 | `met` |
| 현재 세대를 아직 관측하지 않았거나 대상이 없음 | `unknown` |
| 준비 중·삭제 중·복제본 0 등 | `not_met` |
| 같은 이름이지만 UID 또는 세대가 달라짐 | `stale`; 변경 계획도 거부 |

이전에 저장한 배포 목표를 확인하려면 시작 시 `--target-uid=ORIGINAL_UID`와
`--target-generation=ORIGINAL_GENERATION`을 **함께** 줘. 현재 값으로 덮어쓰지 않아.
두 옵션 없이 없는 앱을 지정하면 고정할 신원이 없으므로 시작에 실패해.

계획을 만들 때 상태 갱신으로 resourceVersion만 바뀐 것은 최신 값으로 읽어 와도 돼.
그때도 UID·generation은 고정돼 있고, 읽기와 계획 검증 사이의 변경도 다시 검사해.
기존 환경 변수·볼륨·준비 검사·보조 컨테이너·메타데이터는 보존하고 요청한 필드만 바꿔.

### 답변에 사용할 값은 `answer` 한 곳에 둬

원래 목표는 프로그램 안에서 비교 기준으로 보관해. 응답에는 **지금 읽은 객체의 ID만**
넣어. 예전 ID와 현재 ID를 나란히 반복하면 모델이 예전 값을 답변으로 옮길 수 있기 때문이야.
다음은 같은 이름의 앱이 새 객체로 교체된 경우의 예시야.

```json
{
  "format": "shearwater.answer/v1",
  "resource": "deploy/web",
  "namespace": "bench",
  "answer": {
    "state": "stale",
    "uid": "CURRENT_OBJECT_UID",
    "generation": 1,
    "http": "not_checked"
  },
  "reason": "target_identity_or_generation_changed"
}
```

`reason`은 판정 이유를 설명하는 코드이고 예시의 값은 상황에 따라 달라져. 환경 변수·임의
주석·로그 본문은 보내지 않아. 대상이 없으면 `answer.uid`는 빈 문자열, 세대는 0이야.
`http=not_checked`는 API 상태만 읽었다는 뜻이므로 실제 웹 접속 성공을 의미하지 않아.

이미지·복제본 계획도 같은 바깥 구조를 사용해. 안쪽 `answer`에는
`{"plan":"64자리 해시","applied":false}`가 들어가. 해시 하나만 답하면 계획인지 적용
결과인지 알기 어려우므로 두 필드를 함께 반환해. 만료 시각과 변경 원문은 로컬 `plan`
명령이나 `/review`에서 확인해. 응답이 짧아져도 만료·사람 승인·동시 변경 검사는 유지돼.

외부 MCP 사용자는 `answer` **전체 JSON**을 최종 결과로 전달해야 해. 도구의 설명과
구조를 명확하게 해도 외부 AI가 잘못 옮길 가능성까지 없애지는 못해. 중요한 판단은 실제
도구 결과와 대조해. 이 구조는 alpha.8 집중 모드의 계약이며 일반 모드의 응답은 유지해.
이전 개발판의 최상위 `uid`를 읽는 연동은 `answer.uid`로 바꿔.

### 내장 대화는 프로그램이 확인한 JSON을 직접 보여 줘

집중 모드의 `sw chat`은 모델이 다시 쓴 문장 대신 이번 질문에서 성공한 도구의 `answer`를
`[Shearwater 확인 결과]` 아래 출력해. 모델이 과거 ID를 적거나 계획 해시만 답해도 사용자에게
보이는 결과는 실제 도구가 만든 JSON이야. 다음 질문의 대화 이력에도 그 JSON을 남겨.
도구를 호출하지 않고 모델만 답하면 확인 실패를 알리고, 이전 질문의 결과를 재사용하지 않아.
뒤의 도구가 오류를 내거나 숨긴 도구를 요청해도 앞의 성공 결과로 덮지 않아.

이 기능은 모델의 판단력이 좋아졌다는 보장이 아니라 **검증된 값을 다시 옮겨 쓰는 단계를
프로그램이 맡는 것**이야. 모델 재시도나 자동 승인 기능을 추가하지 않아. 새 질문·`/reset`·
오류에서 출력용 답변을 지워도 고정한 목표와 계획의 승인 조건은 그대로 유지해.
자유 설명을 출력하는 일반 대화 모드와 구분해 사용해.

## 코드와 검사 읽기

1. `cmd/sw/focus.go`: 입력 검사 → 한 번 대상 고정 → 실제 도구 목록 → 대상만 조회 →
   기존 계획 함수 호출. 상태 판정은 `internal/agent`의 공통 규칙을 재사용해.
2. `cmd/sw/mcp.go`: 선택된 목록에 없는 호출과 스키마 밖 인자를 실행 전에 거부해.
   목록과 실행 허용 범위에 같은 함수를 써서 서로 어긋나지 않게 해.
3. `cmd/sw/chat.go`: 같은 도구 목록으로 모델과 대화하고, 사람의 터미널 승인만 기존
   계획 실행 함수로 연결해. 모델에게 원시 셸이나 승인 함수를 노출하지 않아.
4. `cmd/sw/app_plan.go`, `plan.go`: 기존 필드 보존, 동시 변경 검사, 해시·만료·승인·한 번
   적용·실행 기록 검증을 담당해. 집중 모드가 별도 적용 경로를 만들지 않아.
5. `focus_test.go`, `focus_chat_test.go`: 다른 대상 인자·숨긴 도구·재생성·세대 변경·조회
   직후 경합·상태만 갱신·초기화·모의 모델 도구 요청을 검사해.
6. `focus_answer_test.go`: 두 제공자의 응답 형식으로 과거 ID 오답과 해시만 있는 답변을
   재현해. 실제 API에서 읽은 JSON 출력, 잘못된 문장의 이력 제외, 도구 없는 다음 질문과
   뒤늦은 오류에서 앞의 결과를 재사용하지 않는지 확인해.

```bash
. hack/env.sh
bash hack/t.sh
python3 hack/focus-check.py /absolute/path/to/new/sw
python3 hack/chat-check.py /absolute/path/to/new/sw
python3 hack/mcp-check.py /absolute/path/to/new/sw
python3 hack/chat-windows-test.py
```

`focus-check.py`는 일반 사용자로 별도 API/SQLite와 실제 stdio MCP·가상 터미널을 실행해.
모의 모델의 계획을 사람이 검토·승인하는 흐름까지 검사하고 자신의 자식 프로세스를 종료해.
실제 AI의 정확도·속도와 컨테이너 운영 성능은 이 검사만으로 주장할 수 없어.
모델 비교는 같은 질문·대상·실패 기준을 유지한 별도 반복 실험으로 측정해야 해.
