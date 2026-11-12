# M2 첫 확인 결과 (2026-11-11)

설계 7절의 "M2 첫 확인 목록" 네 가지를 구현 전에 작은 실험으로 봤다. 스크립트는 `hack/m2-probe.sh`(root, `bash hack/m2-probe.sh all`)이고, 이 WSL에서 **37개 검사가 모두 통과**했다. 실험이 끝나면 만든 것을 전부 지운다(브리지, 네임스페이스, 규칙, 컨테이너). containerd 네임스페이스 `shearwater`와 그 안의 `busybox:1.36` 이미지는 남는다.

환경: 커널 6.6.87.2-microsoft-standard-WSL2, containerd v2.3.3, iptables v1.8.10(nf_tables), eth0 MTU 1500, FORWARD 기본 정책 DROP, `bridge-nf-call-iptables=1`.

실험은 `ctr`, `ip`, `iptables` 명령으로 했다. 여기서 묻는 것은 "이 방식이 이 호스트에서 되는가"이고, 우리 netlink 코드가 맞는지는 구현 뒤 `hack/m2-check.sh`가 본다.

## 결론

| | 확인 대상 | 결과 |
|---|---|---|
| ① | shim이 로그 파일에 직접 쓰는가 | **된다.** 태스크를 `file:///var/log/shearwater/...` 주소로 만들면 `containerd-shim-runc-v2`가 그 파일을 열어 쥐고, 컨테이너 프로세스는 파이프에 쓴다. 붙어 있는 클라이언트 없이 줄이 늘었고(10 → 16줄), 표준 출력과 표준 오류가 같은 파일에 들어간다. 컨테이너를 KILL한 뒤에도 파일이 남는다 |
| ② | `DOCKER-USER` 규칙으로 Pod ↔ Pod, Pod → 밖 | **된다.** 규칙이 없을 때는 Pod ↔ Pod와 Pod → 밖이 둘 다 막힌다(예상대로). 전용 체인을 넣은 뒤 Pod ↔ Pod, Pod → 게이트웨이, 호스트 → Pod, Pod → 1.1.1.1(ping, TCP)이 된다. 밖에서 본 출발지는 공인 주소다(주소 변환 적용). 미리 만든 네임스페이스에 들어간 컨테이너 안에서도 같다 |
| ③ | MTU | **1500 그대로 쓴다.** 호스트와 Pod 모두 1500바이트 패킷(조각 금지)이 1.1.1.1까지 가고, Pod 사이도 간다. 1바이트 더 크면 바로 거절된다. Pod에서 TLS로 20MB를 끝까지 받았다 |
| ④ | 재부팅 뒤 같은 IP 복구 | **흉내로는 된다.** 네임스페이스·veth·브리지·규칙을 손으로 지운 뒤 같은 주소로 다시 만들면 통신이 돌아오고, 새 컨테이너가 같은 IP를 본다. 이미지는 네임스페이스에 남아 있다. **WSL을 실제로 껐다 켜지는 않았다**(다른 세션과 Docker가 같이 죽는다) |

**결정 6을 다시 볼 조건에 해당하지 않는다.** ②가 이 WSL에서 풀렸다.

## 실험에서 쓴 규칙 (구현이 그대로 따른다)

```
filter:
  -N SHEARWATER-FORWARD
  -A SHEARWATER-FORWARD -i sw-br0 -o sw-br0 -j ACCEPT
  -A SHEARWATER-FORWARD -i sw-br0 ! -o sw-br0 -j ACCEPT
  -A SHEARWATER-FORWARD -o sw-br0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
  -I DOCKER-USER 1 -j SHEARWATER-FORWARD
nat:
  -N SHEARWATER-POSTROUTING
  -A SHEARWATER-POSTROUTING -d 10.244.0.0/24 -j RETURN
  -A SHEARWATER-POSTROUTING -d 10.96.0.0/16 -j RETURN
  -A SHEARWATER-POSTROUTING ! -o sw-br0 -j MASQUERADE
  -A POSTROUTING -s 10.244.0.0/24 -j SHEARWATER-POSTROUTING
```

nat 표에는 `DOCKER-USER` 같은 자리가 없어서 `POSTROUTING`에 점프 한 줄을 직접 넣는다(Docker의 줄은 건드리지 않는다).

## 실험에서 알게 된 것

- **이름 풀이**: 이 WSL의 `/etc/resolv.conf`는 `nameserver 10.255.255.254`다. 호스트가 가진 주소라서 Pod의 네트워크 네임스페이스에서도 닿았다(게이트웨이를 거쳐 호스트의 INPUT으로 들어간다). 호스트의 `resolv.conf`를 Pod에 복사하는 1단계 방식이 이 WSL에서는 통한다. `127.0.0.53` 같은 루프백 주소를 쓰는 호스트에서는 안 통한다(`sw doctor`가 볼 일, M6).
- **호스트 → Pod**는 FORWARD가 아니라 OUTPUT이라 규칙 없이도 된다. Pod → 게이트웨이도 INPUT이라 규칙 없이 된다.
- busybox의 `wget`은 `http://1.1.1.1`의 https 리디렉션을 따라가다 실패한다(TLS 없음, 컨테이너에 `resolv.conf` 없음). 컨테이너 안 TCP 확인은 "443으로 넘어갔는가"로 봤다.
- `ctr`의 네임스페이스 목록에 `shearwater`가 새로 생긴다. `moby`, `moby_history`는 이름만 봤고 안은 읽지 않았다.

## 하지 않은 것

- 실제 재부팅(`wsl --shutdown`). 재부팅 뒤 containerd에 컨테이너 메타데이터가 어떻게 남는지는 그래서 보지 못했다.
- Docker 재시작 뒤 `DOCKER-USER`의 점프가 남는지.
- IPv6.
