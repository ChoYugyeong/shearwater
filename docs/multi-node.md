# 여러 서버로 쓰기

Shearwater는 서버 한 대로도 쓰지만, 컨트롤 플레인 한 대에 워커를 붙여 여러 대로도 쓸 수 있어.
앱은 여러 서버에 나뉘어 돌고, 한 대가 멈추면 다른 서버에서 다시 시작돼. 어느 서버에 있든 같은
Service 이름·DNS·Ingress로 닿아.

## 구성

```
            kubectl, sw (관리자·사용자)
                    │ HTTPS 6443
        ┌───────────▼───────────┐
        │ 컨트롤 플레인 (sw up)  │  SQLite, API, 스케줄러, 노드 수명, 공개 진입점
        │ 자기 노드 10.244.0.0/24 │  (자기 노드에서 앱도 돌릴 수 있어)
        └──┬─────────────────┬──┘
   노드 API │ HTTPS 6443       │        VXLAN UDP 8472 (Pod 네트워크, 모든 서버 사이)
        ┌──▼───────┐     ┌───▼──────┐   로그 TCP 10250 (컨트롤 플레인 → 워커)
        │ 워커 w1   │     │ 워커 w2   │
        │10.244.1/24│     │10.244.2/24│  노드 에이전트, 서비스 프록시, DNS, 네트워크 정책
        └──────────┘     └──────────┘
```

- 컨트롤 플레인은 **한 대**야. 상태는 그 서버의 SQLite에 있고, 워커는 DB가 없어. 워커는 컨트롤 플레인의
  노드 API로 상태를 읽고 쓴다(노드 자격 증명, 자기 노드에 배정된 것만 쓸 수 있음).
- 컨트롤 플레인이 멈추면 API·스케줄링·재배치는 멈추지만, 워커에서 이미 돌던 앱과 Service 전달은 계속 돌아.
  컨트롤 플레인 자체의 고가용성(여러 대)은 아직 없어.
- 각 서버의 상주 프로세스는 `sw` 하나야.

## 필요한 것

- 서버마다 Linux, root, containerd(설치 안내는 [설치](install.md)).
- 서버 사이에 열어 둘 포트: 컨트롤 플레인 TCP **6443**(워커·kubectl → 컨트롤 플레인), 모든 서버 사이 UDP
  **8472**(VXLAN), TCP **10250**(컨트롤 플레인 → 워커, `kubectl logs`).
- **VXLAN은 암호화하지 않아.** 사설망(같은 LAN, 클라우드 VPC)이나 VPN(WireGuard 등) 위에서 써.
- 서버끼리 L3로만 닿으면 돼(같은 L2일 필요 없음).

## 시작하기

1. 컨트롤 플레인 서버에서, 다른 서버가 닿는 주소를 알려 주고 시작해. 이 주소는 설정 폴더에 기억돼서 다음
   `sw up`부터는 다시 안 써도 돼.

   ```bash
   sudo sw up --advertise-address 192.168.1.10
   ```

   로컬 도구는 계속 `127.0.0.1:6443`을 쓰고, 다른 서버와 원격 kubectl은 `192.168.1.10:6443`으로 붙어.
   서버 인증서에 이 주소가 들어가.

2. 붙일 명령을 받아:

   ```bash
   sudo sw node token            # 기본 24시간 유효, --ttl 2h 처럼 바꿀 수 있어 (최대 7일)
   ```

   `sudo sw join https://192.168.1.10:6443 --token swj_... --ca-hash sha256:...` 한 줄이 나와.

3. 워커 서버에서 그 줄을 실행해:

   ```bash
   sudo sw join https://192.168.1.10:6443 --token swj_... --ca-hash sha256:...
   ```

   워커는 먼저 컨트롤 플레인의 CA를 받아 `--ca-hash`와 비교해(kubeadm과 같은 방식). 다르면 붙지 않아.
   붙으면 노드 이름(기본은 호스트 이름, `--node-name`), 자기 주소(기본은 컨트롤 플레인으로 가는 경로의
   주소, `--node-ip`), Pod 대역 `/24`, 노드 자격 증명, 노드 인증서를 받아 설정 폴더에 저장하고 `sw up`처럼
   시작해. 그 뒤로는 `sudo sw up` / `sudo sw down`으로 다뤄.

4. 컨트롤 플레인에서 확인:

   ```bash
   kubectl get nodes -o wide
   sudo sw node list
   ```

## 배치

스케줄러는 Ready 노드 중에서 고른다. 거르는 조건은 cordon, taint/toleration, nodeSelector, 필수 nodeAffinity,
요청한 CPU·메모리 합(요청이 없으면 limits), Pod 110개, 볼륨이 있는 노드야. 남은 노드 중에서는 덜 찬 노드,
같은 Deployment의 Pod가 적은 노드, 선호 nodeAffinity가 맞는 노드를 먼저 고른다. 맞는 노드가 없으면
Pod 조건에 쿠버네티스 형식으로 이유가 남아:

```
0/3 nodes are available: 1 node(s) had untolerated taint {gpu: true}, 2 Insufficient cpu.
```

`kubectl cordon NODE`로 새 배치를 막고, `kubectl taint`로 taint를 단다.

## 서버가 멈추면

- 워커가 10초마다 보내는 신호(메모리에만 기록, 디스크 쓰기 없음)가 **40초** 끊기면 노드가 `NotReady`
  (`Ready=Unknown`)가 되고 `node.kubernetes.io/unreachable:NoExecute` taint가 붙어. 그 노드의 Pod는 바로
  Service 엔드포인트에서 빠져.
- toleration이 없는 Pod는 **300초** 뒤 삭제 표시가 되고, Deployment가 다른 노드에 새로 만든다. 시간을
  바꾸려면 Pod에 `node.kubernetes.io/unreachable` toleration과 `tolerationSeconds`를 줘.
- 노드가 돌아오면 taint가 풀리고, 남아 있던 컨테이너는 그 노드 에이전트가 정리해.
- 다시 안 돌아올 서버는 `sudo sw node remove NAME`으로 지워. 노드 객체, 그 노드에 묶인 Pod, 그 노드의 Pod
  주소, 노드 자격 증명이 지워져.
- `local-path` PVC를 쓰는 Pod는 데이터가 있는 노드에만 갈 수 있어. 그 노드가 안 돌아오면 Pending으로 남아.

## 네트워크

- Pod 주소는 `10.244.0.0/16`에서 노드마다 `/24`. 노드 사이는 VXLAN 장치 `sw-vxlan0`으로 전달하고,
  각 노드가 자기 장치의 MAC을 Node 주석(`shearwater.io/vtep-mac`)으로 알린다. 학습하지 않으므로 컨트롤
  플레인이 알려 준 노드하고만 통신해.
- 여러 서버일 때 Pod 인터페이스 MTU는 VXLAN 헤더 50바이트만큼 작아져. 이미 돌던 Pod는 다시 만들어질 때 바뀌어.
- Service ClusterIP는 어느 노드에서나 모든 노드의 Pod로 나뉘어 가. Pod가 보낸 연결은 원래 출발지 주소를
  유지해서 NetworkPolicy가 그대로 적용돼.
- 클러스터 DNS는 노드마다 그 노드의 게이트웨이(`10.244.N.1:53`)에서 답해.
- 공개 진입점(Ingress)은 컨트롤 플레인에서 돌고, 다른 노드의 Pod에도 연결해. 진입점은 백엔드 연결을
  재사용하므로, 순서대로 오는 요청은 한 Pod로 계속 갈 수 있어(동시 요청은 나뉘어).

## 로그

`kubectl logs`는 컨트롤 플레인이 그 Pod가 있는 노드의 10250 포트로 중계해. 노드는 클러스터 CA가 서명한
컨트롤 플레인의 클라이언트 인증서만 받아 준다.

## 확인한 것

2026-11-24 QA 배포판에서 서버 세 대를 흉내 냈어(서버마다 별도 network namespace, 전용 containerd,
cgroup, 사설 underlay 192.168.77.0/24). 기록과 하네스는 `maintenance/20261124-multinode-tenancy/qa/`.
PVC 단계를 넣은 뒤의 세 번(`mnq-7`·`mnq-8`·`mnq-9`)에서 아홉 단계가 모두 통과했어(`mnq-8`·`mnq-9`는 PVC를 워커 `w1`에서 시험):

1. 컨트롤 플레인 시작과 join 토큰
2. 워커 두 대 join, 세 노드 Ready
3. Deployment 6개가 노드마다 2개씩 배치
4. 워커 Pod에서 DNS, Service가 세 노드의 Pod 6개 모두로 감, 다른 노드 Pod IP 직접 연결, 로그 중계
5. Ingress 연결(모르는 호스트는 404), 컨트롤 플레인에서 ClusterIP로 세 노드 모두 연결
6. 테넌트 격리(다른 네임스페이스 403), Pod Security(hostPath 거절), 쿼터 초과 거절
7. NetworkPolicy가 다른 네임스페이스의 접근을 노드를 넘어 막음
8. PVC 데이터가 쓴 노드에 남고(워커 `w1`), 같은 PVC를 쓰는 다음 Pod도 그 노드로 감, PVC 삭제 뒤에만 데이터가 지워짐
9. 워커 네트워크를 끊었을 때(`mnq-6`~`mnq-9`) 40~46초에 NotReady, `tolerationSeconds: 20`인 Pod가 65~68초에 다른 노드에서 Ready, 복구 뒤 taint 해제

같은 커널을 쓰는 namespace 흉내이므로 실제 서로 다른 서버·클라우드 네트워크·방화벽에서는 아직 확인하지
않았어. 측정 시간은 이 환경의 값이야.
