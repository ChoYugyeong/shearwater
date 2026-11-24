# 에이전트에게 제한된 kubeconfig 주기

관리자가 서버에 저장된 연결 정보로 다음 명령을 실행하면 `team-a`에서 쓸 `alice`의 kubeconfig가 생긴다. 조회만 하는 에이전트에는 `view`, 워크로드를 배포하는 에이전트에는 `edit`를 지정한다. `edit`는 같은 네임스페이스의 Secret도 읽고 변경할 수 있다.

```sh
sw user add alice --namespace team-a --role view --out ./alice.kubeconfig
kubectl --kubeconfig=./alice.kubeconfig auth whoami
kubectl --kubeconfig=./alice.kubeconfig auth can-i list pods -n team-a
kubectl --kubeconfig=./alice.kubeconfig auth can-i list pods -n team-b
kubectl --kubeconfig=./alice.kubeconfig auth can-i --list -n team-a
```

에이전트에는 `alice.kubeconfig`만 전달한다. 이 예에서 사용자 이름은 `alice`, 두 권한 질의의 예상 답은 차례로 `yes`, `no`다. kubeconfig의 기본 네임스페이스는 편의를 위한 값이다. 요청마다 서버가 RoleBinding과 ClusterRoleBinding을 평가하므로 `-n team-b`를 직접 지정해도 권한이 생기지 않는다. 허가되지 않은 목록 요청은 HTTP 403과 `User "alice" cannot list resource "pods"`가 포함된 Status를 돌려준다.

### 역할과 토큰 관리

| 역할 | 부여되는 권한 |
| --- | --- |
| `view` | 워크로드·서비스 등 조회. Secret 읽기 제외 |
| `edit` | 워크로드·서비스·Secret 등 변경 |
| `admin` | 네임스페이스의 역할·바인딩 관리, ResourceQuota 조회 |
| `cluster-admin` | 클러스터 전체 자원과 비자원 URL 접근 |

다른 사람에게 규칙을 부여하려면 그 규칙을 이미 가지고 있거나 대상 역할에 대한 `bind` 권한이 있어야 한다. 자신이 갖지 않은 규칙을 역할에 추가하려면 `escalate`가 필요하다. 사용자 생성·삭제와 토큰 발급은 로컬 관리자 토큰으로 수행한다.

```sh
sw user list
sw user token alice
sw user remove alice
```

새 토큰은 발급 응답에서 한 번만 확인할 수 있다. 사용자 제거 시에는 토큰을 폐기하고 사용자 관리 API가 표시한 바인딩을 정리한다. 네임스페이스와 그 안의 데이터는 유지된다.

`sw user token alice --out ./alice.kubeconfig`는 토큰을 교체하고 새 kubeconfig를 쓴다. 교체가 끝나면 이전 토큰의 다음 요청은 HTTP 401을 받는다. `--cluster-role NAME`은 클러스터 전체에 별도 바인딩을 추가한다. 한 네임스페이스만 다루는 에이전트에는 `--namespace`와 `--role`을 사용한다.

### 테넌트 워크로드

새 테넌트 네임스페이스에는 `pod-security.kubernetes.io/enforce=baseline`을 붙인다. Pod뿐 아니라 Deployment·ReplicaSet의 Pod 템플릿도 생성·변경 시 검사한다. 예를 들어 baseline에서는 hostPath 볼륨을 포함한 Deployment 생성 요청이 거절된다. `restricted`는 컨테이너별 권한 상승 금지, non-root 실행 선언, seccomp 프로필과 capability 제한도 검사한다. 이 검사는 API 요청의 명세를 검증한다. 런타임이 저장만 하는 보안 설정의 범위는 [호환성 표](compatibility.md)를 따른다.

관리자는 `kubectl create quota team-limit --hard=pods=10,requests.cpu=4 -n team-a`로 한도를 선언할 수 있다. 해당 네임스페이스의 Pod 생성 요청에는 추적되는 CPU 요청량을 지정한다. 한도 초과 시 HTTP 403의 `exceeded quota` 메시지에 요청량·사용량·한도가 나온다. `kubectl get quota team-limit -n team-a -o yaml`로 집계된 `status.used`와 `status.hard`를 확인할 수 있다.

쿼터가 있는 네임스페이스에서는 ReplicaSet 컨트롤러도 Pod를 만들기 전에 같은 admission을 거친다. 이 Pod도 사용량에 집계되고, `pods`·CPU·메모리 한도를 넘게 되면 Pod를 만들지 않고 ReplicaSet에 `FailedCreate` 이벤트를 남긴다. 그래서 Deployment로도 쿼터를 넘길 수 없다.
