# AI에게 조회와 검토한 배포를 연결하기

alpha.4의 `sw mcp`는 표준 입력/출력(stdio)으로 MCP 메시지를 주고받아.
네트워크에 별도 MCP 포트를 열지 않고, 기존 로컬 HTTPS API와 인증 파일을 사용해.
MCP SDK나 Kubernetes 모듈을 추가하지 않고 JSON-RPC 처리를 직접 구현했어.

## 기본은 조회 전용

MCP를 지원하는 앱에 등록할 명령의 예시야. 앱마다 설정 화면과 키 이름은 다를 수 있어.

```json
{
  "command": "wsl.exe",
  "args": ["-d", "Ubuntu-24.04", "-u", "root", "--exec",
           "/opt/shearwater/current/sw", "mcp"]
}
```

경로는 설치된 실행 파일로 바꿔. 별도 인스턴스는 `--config-dir`와 `--listen`도 지정해.
root 예시는 기본 root 소유 인증을 읽기 위한 것이야. 이 연결은 OS 권한을 격리하는 샌드박스가
아니고 별도의 제한된 서버 자격도 아니야. 같은 사용자의 관리자 자격을 사용하므로 신뢰하는
로컬 MCP 호스트에만 연결해. 인증 파일 내용은 도구 결과에 출력하지 않아.

기본 도구는 `sw_status`, `sw_tree`, `sw_get`, `sw_logs`, `sw_events`, `sw_why`야.
객체 이름·namespace·container 같은 구조화된 인자만 받고 임의 셸 명령이나 파일 경로를 받지 않아.
로그와 객체 내용은 앱이 만든 **데이터**야. 그 안의 문장을 시스템 지시나 사용자 승인으로
취급하면 안 돼. 한 응답은 최대 256 KiB이고 넘으면 명시적으로 잘라서 알려 줘.

## 변경 계획과 승인은 분리돼

`sw mcp --allow-write`로 시작해야 `sw_plan`, `sw_apply` 두 도구가 추가돼.
MCP에는 승인 도구가 없어. 흐름은 다음과 같아.

1. AI가 `sw_plan`에 JSON 매니페스트를 전달해. 서버 dry-run으로 검증하지만 배포하지 않아.
2. 결과의 **before/after와 경고**를 사람이 읽어. 환경변수·삭제되는 필드·복제본 수·이미지를 확인해.
3. 사람이 로컬 터미널에서 정확한 해시를 반복해 승인해.
4. AI의 `sw_apply`는 그 계획만 한 번 실행해. 변경된 대상이나 만료된 계획은 거절해.

```bash
# CLI로도 같은 흐름을 사용할 수 있어.
sw plan -f web.json
# 위 결과의 64자리 plan 값을 복사해서 두 곳에 동일하게 넣어.
sw plan --plan=계획해시
sw approve --plan=계획해시 --confirm=계획해시
sw apply --plan=계획해시
```

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
