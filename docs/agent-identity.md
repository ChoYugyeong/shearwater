# 에이전트 신원 한 번에 만들기 (`sw agent`)

`sw agent`는 AI 에이전트가 한 네임스페이스 안에서 Shearwater에 붙기 위해 필요한 모든 파일을 한 번의 명령으로 만든다. 내부적으로는 `sw user`와 같은 관리자 API를 쓰며, 에이전트 전용 라벨(`shearwater.io/agent=NAME`)을 붙인다.

```sh
sw agent add ops-bot --namespace team-a
sw agent list
sw agent remove ops-bot
```

## 만들어지는 것

`sw agent add NAME --namespace NS`는 기본적으로 `./NAME-agent`(권한 `0700`, 내부 파일 `0600`)를 만들고 다음 파일을 쓴다. 비어 있지 않은 기존 디렉터리는 `--force` 없이는 덮어쓰지 않는다.

| 경로 | 용도 |
| --- | --- |
| `kubeconfig` | `NS` 네임스페이스에 제한된 kubeconfig(기본 네임스페이스 `NS`). 원격 접속 시 `--server` 또는 컨트롤 플레인의 광고 주소(`--advertise-address`)를 쓴다 |
| `sw/ca.crt`, `sw/token` | `sw mcp --config-dir <DIR>/sw --namespace NS`와 `sw` CLI가 읽는 에이전트 연결 폴더. **관리자 토큰(`admin.token`)은 절대 들어가지 않는다** |
| `mcp/claude.json` | Claude Code `.mcp.json` 형식 |
| `mcp/codex.toml` | Codex CLI `[mcp_servers.shearwater]` 형식 |
| `mcp/gemini.json` | Gemini CLI `settings.json`의 `mcpServers` 형식 |
| `mcp/qwen.json` | Qwen CLI `mcpServers` 형식 |
| `skills/shearwater-ops/` | 바이너리에 내장된 운영 스킬 사본 |
| `AGENT.md` | 에이전트가 읽는 한 쪽 안내(이름, 네임스페이스, RBAC 권한, 계획 해시 승인 규칙, 신뢰할 수 없는 증거 원칙, 스킬 위치) |

생성된 디렉터리에는 에이전트의 Bearer 토큰(`kubeconfig`, `sw/token`)이 들어 있다. 권한 `0700`을 유지하고 버전 관리에 올리지 않는다.

## 주요 옵션

```sh
sw agent add NAME --namespace NS \
  [--role edit|view|admin] \
  [--client claude|codex|gemini|qwen|all] \
  [--out DIR] \
  [--server URL] \
  [--config-dir DIR] \
  [--listen ADDR] \
  [--force]
```

- `--role`: 기본값 `edit`. 읽기 전용 에이전트는 `view`, 네임스페이스 내 역할·바인딩 관리까지 필요하면 `admin`을 쓴다.
- `--client`: 기본값 `all`. 쉼표로 여러 클라이언트(`--client claude,codex`)를 고를 수도 있다. 선택한 클라이언트 설정 파일만 생성하고, 출력 끝에 바로 붙여 실행할 수 있는 한 줄 명령을 표시한다.
- 네임스페이스가 없으면 새로 만들고 `pod-security.kubernetes.io/enforce=baseline`과 `shearwater.io/agent=NAME` 라벨을 붙인다. RoleBinding(`sw-user.NAME`)에도 `shearwater.io/agent=NAME` 라벨을 붙인다.

## 네임스페이스 제한 토큰으로 동작하는 `sw mcp` 도구

`sw agent add`가 만드는 연결 폴더(`sw/`)는 `admin.token` 대신 `token` 파일에 에이전트 토큰을 두고, 생성된 MCP 설정과 연결 명령에 `--namespace NS`를 함께 넣는다. `sw mcp --namespace NS`는 도구 호출에서 `namespace` 인자가 생략될 때 기본값으로 `NS`를 사용한다. `sw` CLI와 `sw mcp`는 `token` 파일이 있으면 그 토큰만 사용하며, 권한이 부족해도 관리자 토큰으로 폴백하지 않는다.

| 도구 | 네임스페이스 제한 토큰 동작 | 비고 |
| --- | --- | --- |
| `sw_capabilities` | 동작 | 서버 호출 없음 |
| `sw_status`, `sw_tree` | 동작 (`NS` 기본값 또는 `namespace: NS`) | `NS` 범위 목록만 읽는다. `/api/v1/nodes`가 403이면 실패하지 않고 `nodes: not visible to this identity`를 표시한다. 네임스페이스 없이 클러스터 전체를 조회하거나 다른 네임스페이스를 지정하면 HTTP 403 (`UNAUTHORIZED`) |
| `sw_observe`, `sw_verify` | 동작 (`NS` 기본값 또는 `namespace: NS`) | `NS` 범위의 Deployment·ReplicaSet·Pod·Service·EndpointSlice만 읽는다. 노드가 보이지 않으면 `notes`에 `nodes: not visible to this identity`를 남긴다. 다른 네임스페이스는 HTTP 403 |
| `sw_verify_plan` | 동작 (영수증의 모든 목표가 `NS` 안일 때) | 영수증 목표가 한 네임스페이스에 모여 있으면 그 네임스페이스만 읽는다. 여러 네임스페이스에 걸치거나 다른 네임스페이스 목표는 HTTP 403 |
| `sw_changes` | 동작 (`NS` 기본값 또는 `namespace: NS`) | 서버가 호출자 RBAC에 따라 볼 수 있는 변경만 필터링해 돌려준다 |
| `sw_get` | 동작 (`NS` 기본값 또는 `namespace: NS`) | 다른 네임스페이스나 클러스터 자원(`nodes`, `namespaces`)은 HTTP 403 (`UNAUTHORIZED`) |
| `sw_logs` | 동작 (`NS` 기본값 또는 `namespace: NS`) | `view` 이상에서 `pods`, `pods/log` 조회 허용 |
| `sw_events` | 동작 (`NS` 기본값 또는 `namespace: NS`) | `view` 이상에서 `events` 조회 허용 |
| `sw_why` | 동작 (`NS` 기본값 또는 `namespace: NS`) | Pod, Event, 로그 조회 모두 네임스페이스 범위 |
| `sw_receipt` | 동작 | 로컬 계획 디렉터리의 영수증 조회 |
| `sw_plan`, `sw_app_plan`, `sw_apply` | `--allow-write` + `edit`/`admin`에서 동작 (`NS`) | 쓰기 직후 자동 검증(`verifyAppliedPlan`)도 모든 목표가 `NS` 안에 있으면 성공한다. `view` 역할이나 다른 네임스페이스·`Namespace` 종류는 HTTP 403. 로컬 `sw approve`로 정확한 계획 해시 승인 필요 |
| 집중 도구 (`sw_target_*`) | 동작 (`sw mcp --target=deploy/NAME -n NS`) | 선택한 네임스페이스의 Deployment·ReplicaSet·Pod만 조회 |

> 관리자 권한이 계속 필요한 작업: 네임스페이스 없는 클러스터 전체 조회(`sw status`, `sw tree`, `sw observe`), 노드 목록·상세(`nodes`), 네임스페이스 목록·생성(`namespaces`), 다른 네임스페이스 접근, 여러 네임스페이스에 걸친 계획·검증, 그리고 `sw user`·`sw agent`·`sw node`·서버 수명주기 관리.

## 목록과 제거

```sh
sw agent list
sw agent remove ops-bot
```

- `sw agent list`는 `sw agent add`로 만든 에이전트만 이름, 네임스페이스, 역할, 생성 시각(`RFC3339`)과 함께 출력한다. `sw user add`로 만든 일반 사용자는 나오지 않는다.
- `sw agent remove NAME`은 에이전트 토큰을 폐기하고 라벨이 붙은 RoleBinding을 삭제한다. **네임스페이스와 그 안의 앱은 그대로 남기며**, 출력에서도 이를 알린다.

관련 문서: [접근 제어와 테넌트 워크로드](access-control.md)
