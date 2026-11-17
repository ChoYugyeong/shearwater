# AI가 상태를 읽고 결과까지 확인하는 방법

**alpha.8부터 제공하는 AI 운영 인터페이스 안내야.** [설치 안내](install.md)로 받은
실행 파일에서 `sw version`이 `0.1.0-alpha.10`인지 먼저 확인해. 클라이언트만 새 버전으로
실행해 기존 API를 조회할 수도 있어. 운영 업데이트는 설치 안내의 백업·교체 절차를 따라.

## 어떤 가치를 만들려는 걸까

사용자가 "앱을 고쳐 줘"라고 말하면 AI는 먼저 사실을 알아야 해. 모든 명세와 로그를
한 번에 넘기기보다, 필요한 정보를 쉽게 찾고 변경 결과를 확인할 수 있게 만드는 게 목표야.

```text
지원 범위 → 짧은 관찰 → 필요한 상세 근거 → 작은 의도로 계획
                                              ↓
                                      사람이 원문 검토·승인
                                              ↓
                                한 번 적용 → 실행 기록 → 원래 목표 검증
```

Claude·Codex·DeepSeek 앱은 MCP로, 내장 Qwen은 `sw chat`으로 같은 운영 함수를 호출해.
모델은 선택·설명을 맡고, 프로그램은 인자 검사·승인·동시 변경 검사·결과 판정을 맡아.

**이미 작업할 앱을 골랐다면 [집중 모드](focused-tools.md)부터 써.** 연결할 때 대상과
작업 종류를 고정하고, 조회에는 도구 1개, 이미지/복제본 계획에는 2개만 보여 줘.
모델이 namespace나 UID를 매번 옮겨 적지 않아도 돼. 아래 전체 도구 흐름은 앱을 찾거나
집중 모드가 지원하지 않는 상세 진단이 필요할 때 사용해.

앱 몇 개를 바로 운영하려면 Compose가 잘 맞는 경우가 많아. DNS·Ingress·PVC·RBAC·Helm·
다중 노드가 필요하면 Kubernetes/K3s를 검토해야 해. Shearwater는 [지원 범위](compatibility.md)가
좁고, 오케스트레이터 학습과 AI의 관찰·계획·승인·결과 추적 실험에 적합한지를 시험하고 있어.
단일 실행 파일·SQLite·MCP만으로 차별성을 주장하지 않아. K3s도 단일 바이너리와 SQLite를
사용하고 Docker에도 AI 도구가 있어. [K3s](https://docs.k3s.io/), [Docker AI](https://docs.docker.com/ai-overview/).
다른 도구보다 빠르거나 초보자에게 쉽다는 동일 조건 비교는 아직 하지 않았어.

## 1. 적은 정보부터 읽기

아래 `$sw`는 **alpha.10 실행 파일의 절대 경로**야. 별도 인증 경로라면 모든 명령에 같은
`--config-dir`와 `--listen`을 붙여. 기본 관리자 인증으로 조회하는 CLI는 설치 안내처럼
sudo가 필요할 수 있어. 모델 대화인 `sw chat` 자체는 일반 사용자로 실행해야 해.

```bash
"$sw" capabilities
"$sw" observe
"$sw" observe -n default
"$sw" observe --resource=deploy/web
```

`observe`는 Deployment·Pod·Service의 이름, UID, 객체 버전, 세대(generation), 짧은 상태와
숫자를 JSON으로 내보내. 환경 변수·임의 주석·로그 본문은 기본 응답에 넣지 않아.
문제가 있는 Pod의 자세한 이유는 `sw why pod/NAME`, 원문은 `sw get TYPE/NAME --full`로 봐.

처음에는 namespace를 지정하지 않은 `observe`로 전체를 찾아. namespace는 앱을 나누는
이름 공간이야. 특정 대상만 읽을 때는 기본값이 `default`이므로, 다른 이름 공간의 앱에는
`--resource=deploy/web -n MY_NAMESPACE`처럼 실제 이름도 지정해. 빈 목록은 그 범위에서
찾지 못했다는 뜻이지, 다른 이름 공간의 앱까지 없어졌다는 뜻은 아니야.

- **UID**: 객체의 고유 신분증. 지운 뒤 같은 이름으로 만들어도 다른 UID를 받아.
- **resourceVersion**: 서버가 관리하는 객체 버전. 내가 읽은 뒤 다른 변경이 있었는지 검사해.
- **generation**: 원하는 설정의 세대. 컨트롤러가 현재 세대까지 처리했는지 비교할 때 써.

예를 들어 버전 A의 앱을 배포한 직후 다른 사람이 B로 바꿨다면, B가 정상이라고 해서
A의 배포 확인을 성공 처리하면 안 돼. UID·세대를 고정하는 이유가 이거야.

기본 페이지는 20개, 최대 100개야. `total`과 `states`는 선택한 범위 전체의 집계라서 다음
페이지에 장애가 있다고 숨기지 않아. `next`가 있으면 그 안의 offset·revision·범위를 다음
호출에 사용해. 도중 상태가 바뀌면 `CONFLICT`로 알려 주므로 처음부터 읽어.
여러 목록의 revision이 같아야 서로 다른 시점의 상태를 섞지 않기 때문이야.

## 2. 전체 명세 대신 의도로 계획 만들기

새 앱에는 **이미 준비된 컨테이너 이미지**와 앱이 듣는 TCP 포트가 필요해.

```bash
"$sw" app-plan --action=create --name=web --image=nginx:alpine --port=80
```

Deployment와 내부 TCP Service의 계획만 저장하고 아직 실행하지 않아. 기본 복제본은 1개,
준비 검사는 TCP 연결이야. 소스 이미지 빌드·Service 이름 DNS·Windows 공개 주소·도메인·
HTTPS를 만들어 주지는 않아. 같은 이름이 이미 있으면 새 앱으로 가져오지 않고 거부해.

기존 앱은 `observe`에서 읽은 UID와 `resourceVersion`으로 변경해.

```bash
"$sw" app-plan --action=image --name=web --image=nginx:stable \
  --uid=OBSERVED_UID --resource-version=OBSERVED_RESOURCE_VERSION
"$sw" app-plan --action=scale --name=web --replicas=2 \
  --uid=OBSERVED_UID --resource-version=OBSERVED_RESOURCE_VERSION
```

대문자 자리표시자는 실제 값이야. AI 연결에서는 에이전트가 조회 값으로 인자를 채워.
컨테이너가 여러 개면 이미지 변경에 `--container=NAME`도 지정해. 환경 변수·볼륨·다른
컨테이너는 보존해. 관찰 이후 객체가 바뀌었다면 새로 읽고 변경 내용을 다시 검토해야 해.

## 3. 승인·실행·준비·실제 접속은 다르다

계획 응답의 `plan`은 정확한 계획 해시야. [기존 승인 절차](agent-mcp.md)를 사용해.

```bash
"$sw" plan --plan=PLAN_HASH
"$sw" approve --plan=PLAN_HASH --confirm=PLAN_HASH
"$sw" apply --plan=PLAN_HASH
"$sw" verify --plan=PLAN_HASH
```

`apply`는 쓰기 성공 응답에서 대상의 UID·세대를 실행 기록에 저장하고 그 목표로 준비 상태를
한 번 확인해. 아직 준비 중이면 같은 해시로 `verify --plan`을 다시 호출해.
**최신 관찰에서 다른 버전의 UID·세대를 가져와 원래 목표를 바꾸면 안 돼.**

| 결과 | 의미 |
| --- | --- |
| 실행 `complete` | 계획한 API 쓰기에 성공 응답을 받음 |
| 검증 `met` | 그 쓰기 응답에 묶인 원래 대상의 API 준비 조건을 만족 |
| `not_met` | 준비되지 않음, 삭제 중, 복제본 0, 종료된 Pod 등 |
| `unknown` | 근거 부족, 대상 없음, 부분 실행, 또는 목표가 없는 예전 기록 |
| `stale` | 같은 이름이어도 대상이 교체됐거나 다른 세대가 됨 |
| `reachability=not_checked` | HTTP 연결·본문·외부 접속은 확인하지 않음 |

여러 객체 변경은 한 트랜잭션이 아니야. `uncertain-or-partial`이면 같은 apply를 다시 하지
말고 실행 기록과 실제 상태부터 확인해. MCP의 `sw_receipt`는 실행 기록을, `sw_verify_plan`은
원래 적용 목표를 검사해. 준비 상태는 API가 보고한 시점의 사실이고 지속 정상의 보증은 아니야.
실제 접속이 목표라면 해당 실행 환경에서 HTTP 요청과 응답 본문을 별도로 확인해야 해.

`verify --resource=... --uid=... --generation=...`도 있지만 배포 확인에는 해시를 우선해.
CLI 검증은 목표가 충족되지 않으면 종료 코드가 0이 아니야. MCP 조회 자체가 성공해도
반환된 목표의 `state`는 `unknown`일 수 있어.

## 4. Qwen 내장 대화

### Linux에서 Ollama가 실행 중일 때

```bash
"$sw" chat --model=YOUR_INSTALLED_QWEN_MODEL --config-dir=/absolute/user/config
```

모델을 자동으로 설치하지 않아. 이미 설치된 정확한 이름을 사용해. 대화는 root 실행을
거부해. 사용자 소유 인증은 현재 관리자 권한을 위임받는 것이므로 [MCP 권한 설명](agent-mcp.md)을
읽고 신뢰하는 로컬 사용자에게만 준비해. 모델은 질문과 선택한 도구 결과를 받으므로
로그·객체에 민감한 값이 있으면 그 값도 모델로 전달될 수 있어.

기본은 조회 전용이야. 대화형 모드의 `--allow-write`는 계획과 승인된 적용을 추가해.
`/review`는 가장 최근에 만든 계획의 원문과 정확한 해시를 프로그램이 출력해.
다른 계획을 고르려면 `/review HASH`를 사용해. 읽은 뒤 실제 터미널에서 `/approve`를
입력하면 방금 검토한 계획만 승인해 한 번 적용하므로 해시를 반복해서 복사할 필요가 없어.
새 대화 요청을 보내면 검토 선택을 지우고 다시 검토하게 해. AI에게 승인 도구는 없고
모델 답변에 승인 문장이 있어도 승인이 되지 않아. 같은 OS 권한의 다른 프로그램을 막는
보안 격리 기능은 아니야.

`/reset`은 대화 메모리와 이 대화의 승인 목록을 초기화해. 앱·계획 파일은 삭제하지 않아.
`/quit`은 대화를 끝내. 모델 응답 8회·요청 본문 24 KiB 제한을 넘으면 명시적으로 멈춰.
이는 토큰 수나 모델 성능 수치가 아니야. 대화는 파일이나 서버 SQLite에 저장하지 않아.

### Qwen은 Windows, sw는 WSL에 있을 때

Windows Python 3로 연결 도우미를 실행해. 포트를 새로 열거나 WSL 설정을 바꾸지 않아.
릴리스 페이지의 `chat-windows.py`와 `SHA256SUMS`를 받아 체크섬을 확인했다면 아래
`python .\hack\chat-windows.py`를 `python .\chat-windows.py`로 바꿔 실행해.
Linux 압축에도 `hack/chat-windows.py`가 들어 있어. 새 모델을 내려받는 도구는 아니야.

```powershell
python .\hack\chat-windows.py `
  --sw /absolute/linux/path/sw `
  --config-dir /absolute/linux/user/config `
  --model qwen2.5:latest `
  --prompt "앱 상태를 관찰하고 확인한 사실과 모르는 부분을 알려줘"
```

경로는 실제 값으로 바꿔. **한 질문 안의 여러 도구 호출과 답변**을 지원하고, 질문 사이의
이력과 그 창에서의 승인은 제공하지 않아. `--allow-write`로 계획을 만들 수 있지만
검토·승인은 Linux CLI에서 해야 해. 모델·로그 문장은 JSON 데이터로 감싸서 가짜 제어
메시지가 되지 않게 해. 도우미는 임의 명령이나 모델이 정한 URL을 실행하지 않아.

**alpha.9 추가 기능:** 일반 한 질문 대화에서는 실행할 수 없는 적용 도구를 숨겨.
로컬에서 검토·승인한 계획은 `--allow-write --approved-plan=HASH`로 별도 대화를 시작해
그 계획 하나만 적용·검증할 수 있어. 이전 클라이언트는 alpha.9로 교체해야 해.
[작업별 스킬과 승인된 실행 안내](agent-skills.md)에 사용법과 확인 범위를 정리했어.

## 5. 기존 딥시크 앱의 MCP

딥시크 로그인 앱에 Shearwater MCP를 연결하면 별도 API 키가 필요하지 않아.
기존 앱의 인증 파일에서 키를 꺼내 쓰지 않아. 공식 `@deepseek-ai/dsh-mcp-client`의 stdio
command/args를 [연결 예시](agent-mcp.md)처럼 지정해. 등록 UI는 앱 버전에 따라 달라질 수 있어.
Electron 관리 프로필을 CLI에서 열 수 없다는 오류가 나오면 직접 덮어쓰거나 초기화하지 말고
앱 설정을 사용해. [공식 MCP client 문서](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/mcp/mcp-client/README.md).

연결 후 해당 앱에서 `sw_capabilities`와 `sw_observe`를 실제 호출해야 연결 검증이야.
별도 headless 독립 검토 성공과 구분해. 개발 코드에 별도 API 키를 환경변수로 받는
`--provider=deepseek` 어댑터도 있지만 실제 API 호출 시험은 별도이고, 기존 앱 MCP가 기본 안내야.

## 연구에서 가져온 원칙

| 원문 | 설계에 적용한 추론 |
| --- | --- |
| [SWE-agent, 2024](https://arxiv.org/abs/2405.15793) | AI에 맞는 간결한 도구와 피드백 |
| [Lost in the Middle, 2024](https://aclanthology.org/2024.tacl-1.9/) | 필요한 근거를 찾기 쉽게 제시 |
| [ReAct, 2023](https://arxiv.org/abs/2210.03629) | 행동 후 환경을 다시 관찰 |
| [τ-bench, 2024](https://arxiv.org/abs/2406.12045) | 실제 최종 상태와 반복 성공을 평가 |
| [Miller·Cohen, 2001](https://www.annualreviews.org/content/journals/10.1146/annurev.neuro.24.1.167) | 목표 유지라는 설계 비유 |
| [Rao·Ballard, 1999](https://homes.cs.washington.edu/~rao/Rao-Ballard-NN-1999.pdf) | 예상 상태와 관찰의 차이를 보여 준다는 비유 |
| [Lamport, 1978](https://lamport.azurewebsites.net/pubs/time-clocks.pdf) | 시각 외에 명시적인 버전·이벤트 연결 |
| [Herlihy·Wing, 1990](https://cs.brown.edu/people/mph/HerlihyW90/p463-herlihy.pdf) | 동시 변경에서 연산의 전후 조건 정의 |

뇌과학 논문은 LLM의 성능을 입증한 것이 아니야. Shearwater가 인간 뇌나 논문의 알고리즘을
재현했다는 뜻도 아니야. 이번 변경의 효과는 같은 모델·앱·환경에서 도구 횟수, 정보량,
사람의 개입, 반복 과제 성공률, 거짓 성공을 측정해 판단해야 해. 연결 시험 한 번으로
초보자 사용성 검증이 끝나지는 않아.

## 코드를 읽는 순서

1. `internal/agent/observe.go`: 네트워크 호출 없이 관찰과 목표를 판정하는 함수.
2. `cmd/sw/observe.go`: 일관된 API 조회, 적용 기록의 원래 목표 검증.
3. `cmd/sw/app_plan.go`: 작은 의도를 기존 계획으로 변환, 필드 보존·동시 변경 검사.
4. `cmd/sw/plan.go`: 정확한 계획·승인·한 번 적용·실행 기록. 모든 입구가 재사용.
5. `cmd/sw/mcp.go`: 도구 스키마와 공통 분배, 구조화 응답과 크기 제한.
6. `cmd/sw/chat.go`: 모델 요청/도구 응답 루프. 모델 출력과 사람의 승인을 분리.
7. `hack/chat-windows.py`: Windows Ollama와 WSL 사이의 선택적 전달 도우미.

`bash hack/t.sh ./...`로 Go 검사를, `python3 hack/mcp-check.py /absolute/path/sw`로 별도
API에서 실제 stdio·계획·승인·검증을 확인해. 후자는 컨테이너·네트워크 부하 시험이 아니야.
`python3 hack/chat-check.py /absolute/path/sw`는 Linux의 실제 가상 터미널과 모의 모델로
`/review`·`/approve` 흐름을 검사해. `python hack/chat-windows-test.py`는 Windows에서도
실행할 수 있고, 가짜 제어 문장·크기 제한·HTTP 리다이렉트 거부를 검사해.
Linux 로컬 Ollama가 있다면 `SW_QWEN_SMOKE=MODEL`로 실제 모델 조회 시험을 추가할 수 있어.
