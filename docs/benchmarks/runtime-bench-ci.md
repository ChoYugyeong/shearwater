# CI 런타임 비교: minikube · K3s · Shearwater · Docker Compose

[alpha.9 비교](runtime-comparison-alpha9.md)는 2026-11-17에 이 PC에서 한 번 잰 기록이야. 측정 스크립트가 이 PC의 시험 전용 경로에 묶여 있어서 다른 장비에서 다시 돌릴 수 없었어. 이 문서는 같은 측정을 저장소 안의 스크립트로 옮기고 GitHub Actions에서 돌리는 방법을 설명해.

- 워크플로: [`.github/workflows/runtime-bench.yml`](../../.github/workflows/runtime-bench.yml)
- 측정 스크립트: [`hack/bench_runtime.py`](../../hack/bench_runtime.py), 부하 생성기: [`hack/benchload`](../../hack/benchload/main.go)
- 집계 검사: [`hack/test_bench_runtime.py`](../../hack/test_bench_runtime.py) (일반 CI의 Python 검사에서 같이 돌아)

## 2026-11-24 결과

GitHub Actions [실행 45421129331](https://github.com/ChoYugyeong/shearwater/actions/runs/45421129331)의 기록이야. main 커밋 `b0ffe10`에서 빌드한 `sw`(버전 문자열은 0.1.0-alpha.14)를 쟀어. 사례는 도구 4개 × 앱 1개/10개 × 3회 반복으로 24개이고 모두 끝까지 돌았어. 각 칸은 **3회의 중앙값 [최솟값–최댓값]** 이고 업데이트 오류만 3회 합계야.

**이 표는 제품 순위가 아니야.** 네 도구는 기능 범위가 달라. 아래 수치는 이 앱, 이 러너, 이 설정에서의 관측이야. 숫자의 뜻은 [alpha.9 문서의 설명](runtime-comparison-alpha9.md#숫자를-읽는-방법)과 같고, 이 문서 아래쪽에 alpha.9와 다른 조건을 적었어.

[사례별 값 CSV](runtime-bench-ci-2026-11-24.csv) · 원본 요청 기록과 자원 표본은 그 실행의 artifact에 있어(90일 보관).

### 앱 10개

| 도구 | 전체 프로세스 PSS, MiB | cgroup 전체, MiB | 부하 CPU, 코어 1개=100% | 배포 준비, 초 | 평상시 p95, ms | 업데이트 오류/요청 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| minikube | 1175.44 [1154.25–1182.98] | 3534.24 [3518.31–3699.51] | 14.13 [12.60–14.21] | 3.05 [2.03–6.38] | 0.36 [0.33–0.36] | 234/27,000 |
| K3s | 1133.54 [1132.64–1171.50] | 1964.55 [1964.45–2000.54] | 10.63 [9.76–10.82] | 4.73 [4.67–6.36] | 0.34 [0.31–0.35] | 216/27,000 |
| Shearwater | 178.78 [174.39–180.20] | 205.84 [201.32–206.29] | 5.46 [5.35–5.50] | 0.96 [0.95–0.96] | 0.27 [0.27–0.27] | 0/27,000 |
| Docker Compose | 248.20 [245.56–250.72] | 232.30 [230.82–233.89] | 16.37 [15.90–16.73] | 7.08 [6.58–7.10] | 0.40 [0.40–0.41] | 1,051/27,000 |

### 앱 1개

| 도구 | 전체 프로세스 PSS, MiB | cgroup 전체, MiB | 부하 CPU, 코어 1개=100% | 배포 준비, 초 | 평상시 p95, ms | 업데이트 오류/요청 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| minikube | 1069.48 [1064.82–1073.07] | 3397.61 [3395.60–3601.86] | 9.07 [8.86–10.54] | 1.76 [1.74–1.76] | 0.31 [0.31–0.38] | 761/27,000 |
| K3s | 1040.36 [1025.23–1074.32] | 1853.26 [1839.03–1886.10] | 5.76 [5.42–5.77] | 1.43 [1.38–2.58] | 0.29 [0.28–0.29] | 783/27,000 |
| Shearwater | 127.44 [121.81–127.64] | 139.48 [133.73–139.53] | 1.88 [1.82–2.01] | 0.70 [0.70–0.70] | 0.25 [0.24–0.28] | 0/27,000 |
| Docker Compose | 193.12 [190.91–196.05] | 161.60 [157.01–162.40] | 7.36 [7.35–7.56] | 5.78 [5.78–5.79] | 0.33 [0.32–0.35] | 169/27,000 |

평상시 요청 오류는 네 도구 모두 0건이었어. 메모리 부족 강제 종료와 메모리 상한 도달도 0이었어.

### 읽을 때 조심할 점

- **minikube의 cgroup 값이 큰 이유**: 노드 컨테이너 안의 Kubernetes 전체와 dockerd, 노드 컨테이너의 파일 캐시가 모두 들어가. PSS는 K3s와 비슷한데 cgroup 값이 1.5GiB쯤 더 큰 건 주로 파일 캐시야.
- **업데이트 오류의 종류가 달라**: K3s와 minikube의 오류는 대부분 2초 제한 시간 초과였고 일부는 연결 거부였어. Compose는 연결 거부와 연결 끊김이야. Compose는 바뀐 컨테이너를 멈추고 다시 만들기 때문이야. 네 도구 모두 종료 유예 5초에 종료 전 대기가 없는 같은 앱 설정이고, K3s·minikube의 오류는 종료 전 대기를 넣으면 달라질 수 있어([종료 설정 대조](runtime-comparison-alpha9.md#k3s의-업데이트-오류는-종료-설정에-따라-달라졌어)). 이 시험에서 Shearwater의 오류 0이 어느 동작 덕분인지는 따로 분리하지 않았어.
- **배포 준비 시간은 실행마다 흔들려**: K3s 앱 10개의 중앙값은 이 실행에서 4.73초였지만, 같은 날 앞선 3회 실행(45410765320, Compose 1회 실패)에서는 2.69초였어. 1초 단위 차이는 실행 하나로 판단하지 마.
- **이미지 받기 재시도**: Compose 2회차는 준비 단계에서 `mirror.gcr.io` 요청 한도에 걸려 다시 받았어. 측정 구간과는 상관없고 그 기록은 artifact의 `prepared.json`에 있어.
- alpha.9 표(이 PC, WSL)와 장비·메모리 상한·CPU 요청량이 달라서 두 표의 숫자를 섞으면 안 돼.

## 비교 대상

처음 과제는 "minikube vs K3s vs Shearwater"였어. alpha.9 비교는 minikube 대신 Docker Compose를 썼어. 한 서버에서 작은 앱 몇 개를 돌리는 사람이 실제로 고르는 대안이 Compose라서였어. CI 비교는 둘 다 넣어서 **네 도구**를 재.

- **minikube**: 과제의 원래 비교군이야. 학습과 로컬 개발용 Kubernetes라서 운영 대안이라기보다 "kubectl이 붙는 가장 흔한 단일 노드"의 기준점이야. 기본 설정(docker 드라이버, 노드 컨테이너 하나)으로 재고, 컨테이너 실행기는 다른 도구와 맞춰 containerd를 써.
- **K3s**: 가벼운 운영용 Kubernetes. 기본 구성요소(DNS, Traefik, metrics-server, local-path, ServiceLB)를 켠 상태야.
- **Shearwater**: 이 커밋에서 빌드한 `sw`.
- **Docker Compose**: alpha.9 비교와 이어 보려고 남겼어.

`--tools`로 일부만 고를 수 있어. 표에는 과제의 세 도구를 먼저 놓고 Compose를 마지막에 둬.

## 언제 돌아

push마다 돌리지 않아. 네 도구를 3회씩 재면 한 시간쯤 걸리고, 결과가 커밋 통과 여부를 정하는 시험도 아니기 때문이야.

- **수동 실행**(`workflow_dispatch`): 도구, 앱 수, 반복 횟수를 입력으로 바꿀 수 있어.
- **주기 실행**: 매주 월요일 03:17 UTC.

결과는 실행마다 artifact `runtime-comparison-<실행 ID>`(`github.run_id`)로 90일 동안 남아. 원본 요청 기록(JSONL), 1초 간격 자원 표본, `summary.csv`, `summary.json`, `summary.md`가 들어 있어. `summary.md`는 실행 화면의 Job summary에도 붙어.

## 공유 러너에서 무엇을 믿을 수 있나

GitHub 호스팅 러너는 다른 사람과 같은 물리 장비를 나눠 쓰는 VM이야. 같은 커밋도 실행마다 지연 시간이 흔들려. 그래서 보는 순서를 정해 뒀어.

1. **메모리**(전체 프로세스 PSS, cgroup 전체): 이웃 VM의 영향이 가장 적어.
2. **배포 준비 시간**: 지연 시간보다 덜 흔들려. 다만 1초 안팎의 차이는 반복 범위를 같이 보고, Compose의 긴 값에는 준비 검사 첫 시점과 상태를 알려 주는 방식의 차이가 들어 있다는 걸 기억해.
3. **업데이트 오류 수**: 오류 0과 수백 건의 차이는 잡음으로 생기지 않아. 다만 네 도구 모두 종료 유예 5초에 종료 전 대기가 없는 같은 앱 설정이야. K3s·minikube의 오류는 종료 전 대기를 넣으면 달라질 수 있어([종료 설정 대조](runtime-comparison-alpha9.md#k3s의-업데이트-오류는-종료-설정에-따라-달라졌어)).
4. 부하 CPU와 p95 지연은 참고만 해. 1ms 안팎의 차이는 잡음으로 봐.

한 job 안에서 네 도구를 **번갈아** 돌려. 반복마다 순서를 한 칸씩 돌려서(1회차 compose→k3s→minikube→shearwater, 2회차 k3s→minikube→shearwater→compose ...) 실행 중간에 러너가 느려져도 한 도구만 손해 보지 않게 해. 그래도 실행 사이 비교는 같은 러너 종류끼리만 해.

## 측정 방식

alpha.9 비교의 측정 규칙을 그대로 옮겼어. 숫자를 읽는 방법은 [그 문서](runtime-comparison-alpha9.md#숫자를-읽는-방법)와 같아.

- 같은 nginx 이미지(digest 고정), 요청마다 정확히 1,024바이트 응답, 앱마다 CPU 0.5개·메모리 64MiB, 준비 검사 5초 간격.
- 앱 1개와 앱 10개. 10개는 복제본 10개가 아니라 독립된 앱 10개야.
- 각 도구는 새로 켠 뒤 이미지를 미리 받고, 같은 앱을 한 번 띄웠다 지우는 예열을 한 다음 재. 배포 시간에 다운로드와 첫 압축 해제가 들어가지 않아.
- 구간: 유휴 15초 → 초당 200요청 30초 → 같은 부하 45초 중 10초 지점에 v1→v2 업데이트. 요청마다 새 TCP 연결, 제한 시간 2초, 재시도 없음.
- 업데이트가 부하 시작 후 43초(구간 끝 2초 전) 안에 끝나지 않으면 덜 덮인 구간을 세지 않으려고 그 사례 전체를 실패로 처리해. 표에서 빠지고 `Failed`에 남으니 표의 반복 수가 줄 수 있어.
- 앱 제한은 제출한 설정이 아니라 실제 커널 값(각 앱 cgroup의 `memory.max`, `cpu.max`)을 읽어서 확인해. 네 도구 모두 같은 방법(시험 cgroup 안의 nginx 주 프로세스)으로 확인해.

### 격리

러너는 일회용이지만, 러너에 미리 깔린 Docker와 겹치지 않게 격리해서 재.

- 도구마다 **전용 cgroup**(CPU 4개, 메모리 4GiB, swap 0)을 만들고 엔진, containerd, shim, 앱, 보조 Pod를 전부 그 안에 넣어. 부하 생성기, 수집기, kubectl·docker 같은 CLI는 밖에 있어서 집계에 안 들어가. 엔진을 띄우고 감시하는 작은 Python 프로세스 하나는 네 도구 모두 안에 들어가(alpha.9에서는 약 7MiB).
- 도구마다 **전용 네트워크 공간**(`swbench`)을 새로 만들어. K3s·flannel·kube-proxy·minikube가 남긴 인터페이스, 경로, iptables 규칙이 다음 도구에 섞이지 않게 하려고야. 바깥과는 veth 하나와 이름 붙인 iptables 체인 두 개(`SWBENCH-FWD`, `SWBENCH-NAT`)로만 이어져. 끝나면 지워.
- 한 번에 한 도구만 켜.
- kubelet이 바꾸는 커널 값(`vm.overcommit_memory=1`, `kernel.panic=10` 등)을 시작할 때 모든 도구에 똑같이 맞추고 끝나면 되돌려. minikube의 kubelet은 특권 컨테이너라서 그대로 두면 측정 도중에 호스트 값을 바꿔 버려.

### alpha.9 비교와 다른 점

| 항목 | alpha.9 (이 PC) | CI |
| --- | --- | --- |
| 장비 | Ryzen 5 7500F, WSL 2 | GitHub `ubuntu-24.04` 러너(공유 VM) |
| 도구 | Compose, K3s, Shearwater alpha.9 | minikube, K3s, Compose, 이 커밋의 Shearwater |
| 시험 cgroup 메모리 상한 | 2.5GiB | 4GiB. 3GiB에서는 minikube 노드 컨테이너(2,600MiB)와 dockerd가 상한에 닿아서 올렸고, 네 도구 모두 같아 |
| Kubernetes CPU 요청량 | 제한과 같은 0.5 CPU | 0.1 CPU. 4코어 러너에서는 0.5씩 요청하면 앱 10개 중 3개가 스케줄되지 못해. 커널 제한(`cpu.max`)은 그대로 0.5 CPU야 |
| 반복 | 3회 | 기본 3회(입력으로 변경) |
| 이미지 준비 | 미리 만든 tar를 가져옴 | 각 실행기가 같은 digest를 `mirror.gcr.io`(Docker Hub 미러)에서 직접 받음(시간 측정 밖). 공유 러너 주소가 Docker Hub 비로그인 한도에 걸려서야. K3s 구성요소 이미지는 공식 airgap 묶음으로 넣음 |
| 커널 값 | K3s 때문에 두 값을 바꿈 | kubelet 기본값 여섯 개와 `ip_forward`를 모두에게 맞춤 |

그래서 CI 표와 alpha.9 표의 숫자를 한 표에 섞으면 안 돼.

### minikube에서 다른 점

- minikube는 docker 안에 노드 컨테이너 하나를 띄우고 그 안에서 Kubernetes를 돌려. 그래서 dockerd와 노드 컨테이너가 전부 집계에 들어가. 노드 컨테이너는 `--memory=2600mb --cpus=4`로 띄워.
- Service IP는 노드 컨테이너 안에만 있어. `minikube tunnel`과 같은 방식으로 Service 대역을 노드로 보내는 경로를 네트워크 공간에 하나 추가해서, 다른 Kubernetes 도구처럼 Service IP로 요청을 보내.
- minikube의 Kubernetes는 v1.37.0이고 K3s(v1.37.2+k3s1)와 달리 Traefik·metrics-server·ServiceLB가 기본으로 꺼져 있어. 둘 다 각자의 기본 구성 그대로야.
- 러너에서는 root로 돌기 때문에 `--force`를 붙여. minikube는 docker 드라이버를 root로 쓰는 걸 기본으로 막아.

## 직접 돌리기

root와 cgroup v2가 있는 **일회용 x86_64 Linux**에서만 돌려. 실행하는 동안 iptables, 커널 값, cgroup을 바꾸기 때문에 `--yes` 없이는 시작하지 않아. 운영 앱이 있는 호스트에서는 돌리지 마.

```bash
bash hack/build.sh
go build -trimpath -o dist/benchload ./hack/benchload
sudo python3 hack/bench_runtime.py run --yes --keep-going \
  --out /tmp/swbench-out --tool-dir /path/to/tools \
  --tools shearwater,k3s --apps 1 --repeats 1
```

`run`이 끝나면 `summary.*`가 바로 만들어져. 나중에 집계만 다시 만들려면 root 소유가 된 결과 폴더의 소유자를 바꾼 뒤 `python3 hack/bench_runtime.py summarize /tmp/swbench-out`을 돌려. 스크립트를 직접 돌릴 때 `--repeats` 기본값은 1이고, 워크플로 기본값은 3이야.

WSL처럼 커널을 다른 배포판과 같이 쓰는 환경에서는 `--no-kernel-tuning`으로 커널 값을 건드리지 않을 수 있어. 이때는 `net.ipv4.ip_forward=1`이 미리 켜져 있어야 하고, kubelet이 스스로 커널 값을 바꾸는 K3s·minikube는 같이 잴 수 없어(Compose와 Shearwater만).

`--tool-dir`에는 워크플로가 받는 것과 같은 파일(Docker 정적 묶음 전체(`dockerd`·`containerd`·`containerd-shim-runc-v2`·`ctr`·`runc`·`docker`·`docker-proxy` 등), `docker-compose`, `k3s`, `minikube`, `kubectl`)을 두면 돼. 없으면 PATH에서 찾아. 받은 파일의 버전과 체크섬은 워크플로의 `env`에 고정돼 있어. minikube가 실행 중에 받는 노드 이미지와 Kubernetes v1.37.0은 버전만 고정이고, nginx 이미지 digest는 스크립트에 고정돼 있어.

한 도구가 실패해도 `--keep-going`이면 나머지는 계속 재고, 실패 기록(`failure-*.txt`)은 결과 폴더에 남아. 실패한 도구를 몰래 다시 돌리지 않아. 예외는 측정 시간 밖의 이미지 받기 하나야. 공유 러너 주소가 레지스트리 요청 한도에 걸릴 수 있어서 간격을 두고 최대 5번까지 다시 받고, 실패한 시도는 `prepared.json`에 남겨. 실패가 하나라도 있으면 명령은 0이 아닌 값으로 끝나.

## 이 측정이 말하지 않는 것

alpha.9 비교와 같아. 최대 처리량, 긴 시간 안정성, 다중 노드, 긴 연결, 실제 운영 앱의 성능은 재지 않아. minikube와 K3s는 기능을 끄면 더 작아질 수 있어서, 이 값을 그 도구들의 최소 메모리로 읽으면 안 돼. **표는 제품 순위가 아니야.**
