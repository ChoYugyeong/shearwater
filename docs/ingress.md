# Ingress (`networking.k8s.io/v1`)

Shearwater는 별도의 Ingress 컨트롤러 파드 없이 기존 공개 진입점(`sw edge`, `internal/edge`)에서 Kubernetes `Ingress`와 `IngressClass` 자원을 직접 처리한다.

## 기본 IngressClass

서버를 시작할 때 `IngressClass` `shearwater`(컨트롤러 `shearwater.io/ingress`)가 없으면 자동으로 만들고 기본 클래스 주석(`ingressclass.kubernetes.io/is-default-class: "true"`)을 붙인다.

Shearwater는 `spec.ingressClassName`이 비어 있거나 `shearwater`인 Ingress만 라우팅한다. 다른 클래스 이름을 지정한 Ingress는 무시한다.

## 경로 일치 규칙

하나의 호스트 안에서 여러 경로 규칙을 둘 수 있으며 Kubernetes Ingress 규칙을 따른다.

- **`Exact`**: URL 경로와 대소문자를 구분해 정확히 일치해야 한다(`/foo`는 `/foo`만 일치, `/foo/`는 불일치).
- **`Prefix`**: `/`로 구분된 경로 요소 단위로 일치한다.
  - `/`는 모든 경로와 일치한다.
  - `/foo`는 `/foo`, `/foo/`, `/foo/bar`와 일치하고 `/foobar`와는 일치하지 않는다.
  - `/foo/`는 `/foo/`, `/foo/bar`와 일치하고 `/foo`와는 일치하지 않는다.
- **`ImplementationSpecific`**: `Prefix`와 동일하게 처리한다.
- **우선순위**:
  1. 호스트가 일치하는 규칙이 호스트 없는(`host` 비움) 규칙보다 우선한다.
  2. 일치하는 경로 중 가장 긴 경로가 우선한다.
  3. 경로 길이가 같으면 `Exact`가 `Prefix`보다 우선한다.
  4. 일치하는 규칙이 없으면 `spec.defaultBackend`로 전달한다.
- 백엔드 포트는 `service.port.number` 또는 `service.port.name` 모두 지원한다.
- 기존 Service 주석 방식(`shearwater.io/public-host`, `shearwater.io/public-port`)도 그대로 함께 동작한다.

## TLS 인증서

공개 진입점에 HTTPS가 켜져 있을 때(`sw edge enable --agree-tos ...`):

- `spec.tls[].secretName`을 지정하면 같은 네임스페이스의 `kubernetes.io/tls` Secret(`tls.crt`, `tls.key`) 인증서를 사용한다. Secret을 교체하면 재시작 없이 반영된다.
- `secretName`이 없거나 `spec.tls`를 생략한 호스트는 기존처럼 ACME(HTTP-01)로 인증서를 자동 발급·갱신한다.

## 호스트 소유권 (다중 네임스페이스 격리)

- 다른 네임스페이스의 Ingress나 Service 주석(`shearwater.io/public-host`)이 이미 사용 중인 호스트 이름을 다른 네임스페이스에서 생성·수정하려 하면 admission 단계에서 **409 Conflict**로 거절한다.
- 같은 네임스페이스 안에서는 여러 Ingress가 같은 호스트 이름을 나눠 쓸 수 있다(예: `/api`와 `/web`을 별도 Ingress로 분리).
- 라우팅 표에서도 충돌 시 먼저 생성된 객체가 우선한다.

## 상태와 `kubectl` 출력

- 공개 진입점이 실행 중이면 각 Ingress의 `status.loadBalancer.ingress[].ip`에 진입점이 수신 중인 주소를 기록한다.
- `kubectl get ingress`는 `NAME CLASS HOSTS ADDRESS PORTS AGE` 열을 출력한다.
- `kubectl create ingress`(Protobuf 요청 본문)를 지원한다.

## 제한

- 와일드카드 호스트(`*.example.com`)와 `resource` 백엔드는 지원하지 않는다.
- URL 재작성(rewrite), 속도 제한, 커스텀 nginx 주석 등은 지원하지 않는다.
