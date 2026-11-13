# 앱이 실제로 준비됐는지 확인하기

컨테이너 프로세스가 살아 있어도 앱은 아직 시작 중이거나 요청을 처리하지 못할 수 있어.
alpha.4은 세 가지 프로브를 실행해. 프로브는 정해진 간격으로 작은 검사를 하는 설정이야.

| 이름 | 성공하면 | 계속 실패하면 |
|---|---|---|
| startupProbe | 시작 완료를 기록하고 나머지 검사를 시작 | 유예 종료 후 재시작 정책에 따라 처리 |
| readinessProbe | Pod를 Service 연결 대상으로 포함 | Service 대상에서 제외, 이 검사만으로는 재시작하지 않음 |
| livenessProbe | 계속 실행 | 유예 종료 후 재시작 정책에 따라 처리 |

startup 검사가 있으면 성공하기 전까지 readiness와 liveness를 실행하지 않아.
느린 초기화를 liveness가 고장으로 오해해서 계속 재시작하는 일을 막는 역할이야.
readiness가 없으면 startup 완료 후 실행 중인 컨테이너를 Ready로 봐.

## alpha.3에서 올리기 전에

alpha.3는 프로브 필드를 저장만 했지만 alpha.4는 **기존 DB에 저장된 프로브도 실행해**.
따라서 실행 파일만 바꾸더라도 이전에 무시되던 liveness 설정이 앱 재시작을 일으킬 수 있어.
`sw get deployments --full`과 `sw get pods --full`로 선언을 읽고, 경로·포트·시간 제한이 실제
앱과 맞는지 별도 환경에서 확인해. 기존 필드를 조용히 삭제하거나 자동으로 완화하지 않아.
gRPC나 사용자 지정 host 등 이 버전에서 지원하지 않는 기존 설정은 고쳐야 해.

반대로 alpha.3로 실행 파일을 되돌리면 프로브 실행과 readiness에 따른 트래픽 차단도 사라져.
이번 구버전 복귀 실험은 프로브가 없는 앱으로 검증했어. DB 스키마가 같다는 것과
구버전에서도 새 상태 검사 정책이 유지된다는 것은 서로 다른 이야기야.

## HTTP 앱의 예시

아래는 Pod 또는 Deployment의 `spec.template.spec` 안에 넣는 컨테이너 설정의 일부야.

```yaml
containers:
  - name: web
    image: example/web:1
    ports:
      - name: http
        containerPort: 8080
    startupProbe:
      tcpSocket:
        port: http
      periodSeconds: 2
      failureThreshold: 30
    readinessProbe:
      httpGet:
        path: /ready
        port: http
      periodSeconds: 2
      failureThreshold: 2
    livenessProbe:
      httpGet:
        path: /live
        port: http
      periodSeconds: 5
      failureThreshold: 3
      terminationGracePeriodSeconds: 10
```

`example/web:1`은 설명용 이미지 이름이야. 실제 앱 이미지와 실제로 제공하는 경로를 사용해.
readiness 경로는 요청을 받을 수 있을 때 성공을, liveness 경로는 앱 자체가 작동할 때 성공을
반환하게 만들어. 외부 DB가 잠깐 끊겼다는 이유로 모든 앱을 재시작하는 검사는 피해야 해.

## 지원하는 검사와 제한

- `httpGet`: Pod IP로 GET을 보내고 HTTP 200 이상 400 미만을 성공으로 판단해.
  HTTP/HTTPS와 요청 헤더를 지원해. HTTPS는 Kubernetes 프로브와 같이 인증서 검증을 생략해.
  리다이렉트는 따라가지 않고 첫 응답 코드만 봐. 다른 호스트로 이동해 검사하지 않아.
- `tcpSocket`: Pod IP와 포트에 TCP 연결이 되면 성공이야. 앱 내용이 올바르다는 보장은 아니야.
- `exec.command`: 컨테이너 안에서 argv 그대로 실행하고 종료 코드 0을 성공으로 봐.
  호스트 셸에서 실행하지 않아. 셸 문법이 필요하면 `['sh','-c','...']`처럼 명시해야 해.
  출력은 저장하지 않아. 검사 명령에서 별도 백그라운드 작업을 만들지 마.
- 한 프로브에는 위 동작 중 하나만 넣어. gRPC 프로브와 `httpGet.host`/`tcpSocket.host`는
  지원하지 않으므로 명시하면 거절해. 가상 호스트는 HTTP `Host` 헤더로 지정해.
- 포트는 1~65535 숫자 또는 같은 컨테이너의 TCP 포트 이름을 사용할 수 있어.

기본값은 timeout 1초, period 10초, 연속 성공 1회, 연속 실패 3회야.
initialDelay는 0~86400초, timeout은 1~3600초, period는 1~86400초, 임계값은 1~1000을 지원해.
startup/liveness의 successThreshold는 1이어야 해. readiness에서만 더 큰 값을 사용할 수 있어.
프로브별 종료 유예는 startup/liveness에만 설정하고 없으면 Pod의 종료 유예를 사용해.

실패 임계값에 도달하면 먼저 TERM을 보내고 유예가 끝나도 살아 있으면 KILL을 보내.
`restartPolicy: Never`는 재시작하지 않아. `OnFailure`는 프로브로 종료한 경우에도 실패로
판단해. 앱이 TERM에 정상 코드 0으로 응답했다고 실패 원인을 잊지 않아.

## sw가 재시작하면

시작 성공과 종료 요청은 Pod 디렉터리에 원자적으로 기록해.
새 sw는 같은 컨테이너의 startup 검사를 처음부터 반복하지 않고 종료 유예도 이어받아.
연속 성공·실패 횟수는 새로 관측해. 오래된 일부 실패만 보고 앱을 바로 죽이지 않기 위해서야.
readiness는 새 관측자가 성공을 확인할 때까지 보수적으로 제외하므로 sw 재시작 직후
일시적으로 Service 연결 대상이 줄어들 수 있어.

## 문제 확인

```bash
sw get pods -n demo
sw events --for=pod/실제이름 -n demo
sw why pod/실제이름 -n demo
sw logs pod/실제이름 -n demo --previous
```

`Unhealthy` 이벤트에 실패한 검사 종류와 짧은 오류가 들어가.
Pod의 `Ready=False`와 컨테이너 `started=false`는 각각 트래픽 준비와 시작 완료 여부야.
`why`의 정상 종료 코드는 진단 수집 성공이고 앱 전체가 정상이라는 인증은 아니야.
프로브가 Service 선택과 롤링 업데이트에 영향을 주므로 새 설정은 별도 QA에서 시험해.

의미의 기준은 [Kubernetes 프로브 설명](https://kubernetes.io/docs/concepts/workloads/pods/probes/)이야.
여기에 적은 지원 동작과 제한이 Shearwater의 실제 범위이고 Kubernetes 전체 호환을 뜻하지 않아.
