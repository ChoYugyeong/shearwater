# 기존 Docker Compose 앱을 AI에게 맡기기 — `sw compose serve`

[English](en/compose-backend.md) · **한국어**

> alpha.14부터 제공해. 위임 경로(`sw task`)만 지원해. 실제 Docker에서 확인한 것:
> - Claude·DeepSeek·Qwen이 이 흐름으로 7단계(배포·업데이트·장애 진단·복구)를 끝냈어. Qwen은 2회 중 1회 통과야.
> - docker 그룹의 일반 사용자 운영자와, docker 권한이 없는 별도 AI 계정 조합으로도 돌려 봤어.

Shearwater 엔진으로 옮기지 않고, 이미 Compose로 돌리는 서비스 하나를 같은 위임 흐름으로 AI에게 맡길 수 있어.

- 하나의 Compose 프로젝트 앞에 엔진과 같은 운영 API(`/shearwater/operations`)를 띄워.
- AI가 받는 것은 그대로야: 서비스 하나에 묶인 위임 토큰, 정확한 계획 해시, 운영자 승인, 실행 기록, 복제본별 HTTP 증거.
- AI는 docker 소켓이나 운영자 토큰을 받지 않아.

## 시작하기

운영자 계정에서 실행해. 이 계정은 docker를 쓸 수 있어야 해. root일 필요는 없고 `docker` 그룹이면 돼. `docker compose` 플러그인도 있어야 해.

```bash
sw compose serve --file /srv/shop/compose.yaml --project shop --listen 127.0.0.1:16444
```

- 운영자 인증서와 토큰은 `~/.config/shearwater-compose`에 만들어져. 위치는 `--config-dir`로 바꿀 수 있어.
- 작업 기록은 `~/.local/state/shearwater-compose`에 저장돼. 위치는 `--data-dir`로 바꿀 수 있어.
- compose 파일이 여러 개면 `--file`을 `docker compose -f`와 같은 순서로 반복해.

다른 터미널에서 서비스 하나를 위임해. Compose 모드에서는 프로젝트 이름을 `-n`으로 넘겨. AI는 운영자와 다른 OS 계정에서 돌리는 걸 권장해. 그래서 위임 폴더를 두 계정이 넘겨받을 수 있는 교환 폴더에 만들어:

```bash
sudo install -d -o "$USER" -m 0755 /srv/sw-handoff        # 처음 한 번만
sw task create --config-dir ~/.config/shearwater-compose --listen 127.0.0.1:16444 \
  -n shop --name web --port 80 --allow update,recover --expect-body "Welcome" \
  --out /srv/sw-handoff/web --ttl 2h
sudo chown -R ai-user: /srv/sw-handoff/web                # AI 계정에게 넘김
```

AI 계정은 그 안의 `work.json`으로 `sw mcp --workflow=/srv/sw-handoff/web/work.json --allow-write`를 연결하면 돼.

- **운영자 홈에 만든 폴더는 넘길 수 없어.** `--out ~/agent-web`처럼 운영자 홈에 만들면 AI 계정이 열지 못해(`permission denied`). `work.json`이 폴더의 절대 경로를 기억하므로 다른 곳으로 옮겨도 안 돼. 같은 계정에서 AI를 돌릴 때만 홈에 만들어도 돼.
- **AI 계정은 `docker` 그룹에 넣지 마.** 넘긴 뒤 AI 계정은 위임 폴더만 쓸 수 있고, 운영자 토큰이나 docker 소켓은 쓸 수 없어.

계획 검토와 승인은 엔진 모드와 같아: `sw task review`, `sw task approve --confirm <hash>`, `sw task stage-image`.

## 동작 방식

| 항목 | 방식 |
| --- | --- |
| 앱 신원 | Compose 프로젝트 + 서비스 이름 |
| 세대 | 컨테이너의 `shearwater.io/generation` 라벨 |
| 적용 | 이미지와 Shearwater 라벨만 담은 override 파일을 `--data-dir/compose/`에 써. 그다음 `docker compose up --detach --no-deps --no-build --pull missing <서비스>`를 실행해. 사용자의 compose 파일은 고치지 않아 |
| 이중 적용 방지 | 컨테이너의 계획 해시 라벨과 override 파일로 판단해. 응답이 사라져 다시 `continue`해도 같은 계획을 두 번 적용하지 않아 |
| 상태 읽기 | Docker Engine API를 유닉스 소켓으로 직접 호출해 |
| 준비 판정 | 실행 중이고, healthcheck가 있으면 `healthy`여야 해. 그리고 원하는 복제본 수와 컨테이너 수가 같아야 해 |
| HTTP 증거 | 컨테이너 IP마다 직접 확인해. 서비스에 노출된 호스트 포트가 있으면 그 포트도 확인해 |
| 실패 처리 | 이미지 pull 실패처럼 컨테이너가 그대로인 실패는 계획을 닫아. 그래서 바로 복구 계획을 세울 수 있어. 컨테이너가 일부 바뀐 실패는 같은 계획을 다시 `continue`해서 마치거나, 운영자가 폐기해 |
| 손으로 바꾼 경우 | override 없이 `docker compose up`을 다시 하면 라벨이 사라져. 이 작업은 그 상태를 `stale`로 보고하고 목표로 받아들이지 않아 |

## 엔진 모드와 다른 점

- **업데이트 중 끊김:** Compose는 컨테이너를 새로 만들어서 업데이트 중 짧은 끊김이 생길 수 있어. 무중단 롤링이 필요하면 Shearwater 엔진을 써.
- **지원 범위:** `sw task` 위임 경로만 지원해. 옛 `sw connect` 흐름과 kubectl은 쓸 수 없어.
- **새 서비스:** compose 파일에 정의돼 있고 아직 컨테이너가 없는 서비스는 `deploy`로 처음 띄울 수 있어. compose 파일에 없는 서비스는 만들 수 없어.
- **운영 중 직접 변경:** 운영 중인 서비스를 `sw compose serve`를 거치지 않고 다시 `up`하면 이 작업은 멈춰(stale). 운영자가 새로 위임해야 해.
- **보안 경계:** 위임 토큰은 이 서버의 다른 서비스나 승인 기능을 쓸 수 없어. 하지만 AI가 다른 도구로 docker 소켓이나 운영자 폴더에 접근할 수 있으면, 그 권한까지 막지는 못해. OS 계정을 분리해.
