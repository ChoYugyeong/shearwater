# 공개 진입점 — 호스트 이름으로 앱을 인터넷에 열기

Shearwater 서버가 호스트의 80·443 포트를 직접 받아서, 요청의 호스트 이름에 맞는 Service로 넘겨줘.
HTTPS 인증서는 Let's Encrypt 같은 ACME 인증 기관에서 자동으로 받고 갱신해.
별도 프록시 프로그램은 필요 없고, 상주 프로세스는 그대로 `sw server` 하나야.

**검증 범위부터 밝혀 둘게.**

- 자동 시험: 가짜 인증 기관, 실제 TLS 연결, 실제 SQLite로 확인했어.
- 실제 엔진: 격리한 QA 환경에서 nginx Pod를 띄우고 80·443 포트로 14단계를 통과했어(2026-11-19).
- 인증 기관: 같은 14단계를 두 가지로 돌렸어. 하나는 이 저장소의 시험용 인증 기관이고, 다른 하나는 Let's Encrypt가 만든 시험용 서버 Pebble v2.10.3(엄격 모드)이야. 둘 다 실제 HTTP-01 확인을 거쳐 발급했어.
- 아직 안 한 것: 실제 Let's Encrypt 서버에서의 발급, 공인 IP와 실제 DNS가 있는 서버, 서버 재시작 뒤 저장된 인증서로 이어 가기(이건 자동 시험으로만 확인했어).

## 켜는 순서

1. **DNS를 먼저 맞춰.** 쓸 호스트 이름의 A 레코드가 이 서버의 공인 IP를 가리켜야 해. 인증 기관이 80 포트로 접속해 확인하니까 방화벽에서 80·443을 열어 둬.
2. **진입점을 켜.** 인증 기관의 가입자 약관에 동의한다는 뜻으로 `--agree-tos`를 줘야 해.

   ```bash
   sudo sw edge enable --agree-tos --email you@example.com
   ```

3. **서버를 다시 시작해.** 설정은 `/etc/shearwater/edge.json`에 저장되고, 서버가 시작할 때 읽어.

   ```bash
   sudo sw down
   ```

   ```bash
   sudo sw up
   ```

4. **Service에 호스트 이름을 붙여.**

   ```bash
   kubectl annotate service web shearwater.io/public-host=app.example.com
   ```

5. **상태를 확인해.** 인증서 칸이 `valid until 날짜`가 되면 `https://app.example.com`으로 접속돼.

   ```bash
   sudo sw edge status
   ```

처음에는 `--staging`을 붙여서 시험하는 걸 권해. Let's Encrypt 시험용 서버는 발급 한도가 넉넉하고, 대신 브라우저가 신뢰하지 않는 인증서를 줘.
DNS가 틀린 채로 운영 서버에 반복 요청하면 한도에 걸릴 수 있어.

## 설정 옵션

| 옵션 | 기본값 | 뜻 |
| --- | --- | --- |
| `--http` | `:80` | 공개 HTTP 주소. 인증 기관의 확인 요청도 여기로 와 |
| `--https` | `:443` | 공개 HTTPS 주소. `off`면 HTTP만 써 |
| `--agree-tos` | 없음 | 인증 기관 약관 동의. HTTPS를 쓰려면 필수 |
| `--email` | 없음 | 인증 기관이 만료 안내를 보낼 주소(선택) |
| `--staging` | 꺼짐 | Let's Encrypt 시험용 서버 사용 |
| `--acme-directory` | Let's Encrypt | 다른 ACME 인증 기관 주소 |
| `--acme-ca` | 없음 | 사설·시험용 인증 기관에 접속할 때 믿을 루트 인증서 파일 |

`sw edge disable`은 설정을 지우지 않고 끄기만 해. 다시 시작하면 포트를 닫아.

## Service에 붙이는 표시

| 주석 | 뜻 |
| --- | --- |
| `shearwater.io/public-host` | 공개할 호스트 이름. 쉼표로 최대 8개 |
| `shearwater.io/public-port` | Service에 TCP 포트가 여러 개일 때 쓸 포트 번호나 이름 |

- 호스트 이름은 소문자로 바꿔 정확히 일치하는 것만 연결해. IP 주소, 와일드카드(`*.example.com`), 포트가 붙은 값은 받지 않아.
- 두 Service가 같은 호스트 이름을 쓰면 먼저 만든 Service가 유지돼. 뒤의 것은 `sw edge status`에 문제로 표시돼.
- Service를 고치면 바로 반영돼. 서버를 다시 시작할 필요는 없어.

## 동작 규칙

- **HTTPS를 켠 호스트는 평문으로 내보내지 않아.** 인증서가 준비되면 HTTP 요청은 HTTPS로 넘기고(308), 준비 전에는 503으로 답해.
- **공개한 호스트만 인증서를 받아.** 모르는 이름으로 온 TLS 접속은 연결 단계에서 거절하고, 그 이름으로 발급을 시도하지도 않아.
- **TLS 이름과 Host 헤더가 다르면 거절해(421).** 한 앱의 인증서로 다른 앱에 닿을 수 없어.
- **갱신은 유효 기간의 마지막 3분의 1에 해.** 90일짜리면 만료 30일 전부터야.
- **발급이 실패하면 호스트별로 기다렸다 다시 해.** 10분에서 시작해 두 배씩, 최대 12시간이야. 이유는 `sw edge status`에 나와.
- 앱에는 `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host`를 붙여 전달하고 원래 Host 헤더는 그대로 둬.

인증서와 계정 키는 `/var/lib/shearwater/edge/` 아래에 관리자만 읽게 저장해. `sw backup`으로 함께 백업되는지는 아직 확인하지 않았어.

## 지금 없는 것

- 와일드카드 인증서와 DNS-01 확인. HTTP-01만 지원해.
- 경로별 연결(`/api`는 다른 Service로). 호스트 이름 단위로만 연결해.
- Kubernetes Ingress 자원. 지금은 Service 주석으로만 선언해.
- 접속 제한, 요청 속도 제한, HSTS 자동 추가, 접속 로그.
- 진입점만 따로 재시작하기. 서버를 재시작하면 공개 포트의 연결은 끊겨. 내부 Service 연결을 유지하는 기능과는 별개야.

## 코드 위치

- `internal/acme/acme.go`: 표준 라이브러리만 쓴 ACME(RFC 8555) 클라이언트.
- `internal/edge/routes.go`: Service 주석에서 연결표를 만드는 규칙.
- `internal/edge/certs.go`: 인증서 저장·갱신·재시도.
- `internal/edge/server.go`: HTTP·HTTPS 수신과 전달.
- `cmd/sw/edge.go`: `sw edge` 명령과 서버 연결.
