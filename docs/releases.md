# 릴리스와 버전 확인

## 미발행: 0.1.0-alpha.3-dev

로컬 개발 버전에 `sw get`, `logs`, `events`, `why`를 추가했어.
기존 API를 읽기만 하며 대상 UID와 컨테이너 실행 시도를 확인해 진단 기록이
섞이지 않도록 해. 명령 사용법·제한은 [문제 해결 안내](troubleshooting.md),
실제 장애와 회귀 검사 결과는 [검증 기록](diagnostics-verification.md)에 있어.
아직 새 GitHub 태그·실행 파일을 발행한 것은 아니야. 공개 설치 버전은 아래 alpha.2야.

## v0.1.0-alpha.2

첫 알파의 실행 파일은 GitHub 릴리스 이름이 `v0.1.0-alpha.1`이었지만
`sw version`에 개발 초기 값인 `0.0.1`을 표시했어. 이번 릴리스는 제품 버전을
`0.1.0-alpha.2`로 고쳐서 CLI, 도움말, 서버 시작 로그, API `/version`, Node의
버전 정보에 같은 제품 버전을 사용해. 컨테이너·네트워크 동작은 변경하지 않았어.

```text
sw 0.1.0-alpha.2 (Kubernetes API v1.36.0+shearwater.0.1.0-alpha.2)
```

앞의 `0.1.0-alpha.2`가 **Shearwater 제품 버전**이야. 뒤의 `v1.36.0`은
**호환 대상으로 삼은 Kubernetes API 수준**이야. kubectl이 서버 호환성을
판단하므로 제품 버전으로 바꾸지 않아. `+shearwater...`는 어느 Shearwater
버전이 그 API를 제공하는지 알려 주는 부가 정보야. Kubernetes 전체 기능을
구현했다는 뜻은 아니야. [호환표](compatibility.md)에 실제 지원 범위가 있어.

`sw version`은 지금 실행한 **파일**을 확인해. 실행 중인 **서버**는
`kubectl --kubeconfig /실제/kubeconfig get --raw /version`으로 확인해.
새 파일을 받기만 하면 이전 서버가 자동으로 교체되지는 않아.
[설치 안내](install.md)에 다운로드·체크섬·실행 순서와 업데이트 방법이 있어.

## 다음 릴리스를 만드는 개발자에게

제품 버전의 기준값은 `internal/api/server.go`의 `Version` 한 곳이야.
Git 태그에만 붙는 앞의 `v`는 그 값에 넣지 않아. Git이 없는 소스 압축에서도
같은 버전으로 빌드되도록 링크 옵션이나 Git 태그 자동 추측은 사용하지 않아.

1. 기준값과 현재 설치 안내·README의 릴리스 번호를 함께 올려.
2. WSL/Linux에서 아래 검사를 실행해. `--tag`에는 **실제로 발행할 태그**를 넣어.
3. 공개 커밋의 CI가 통과한 뒤, 그 공개 소스를 새로 받아 배포할 파일을 빌드해.
   같은 태그 검사를 다시 하고 `go version -m`의 revision과 modified=false를 확인해.
4. 실제 배포할 파일로 격리된 환경의 런타임 검사를 하고, 라이선스 원문·예제·
   설치 안내·빌드 정보와 함께 압축해. SHA-256은 최종 압축을 만든 뒤 계산해.
5. 새 prerelease에 올리고 다시 다운로드해서 체크섬과 `sw version`을 확인해.
   기존 릴리스의 태그나 파일을 덮어쓰지 않아.

```bash
. hack/env.sh
bash hack/t.sh ./...
python3 -m unittest discover -s hack -p 'test_*.py'
bash hack/build.sh
python3 hack/check_version.py dist/sw --tag v0.1.0-alpha.2
python3 hack/notices.py
```

`check_version.py`는 실행 파일의 `version`, `--version`, `--help`와 API 버전
메타데이터가 일치하는지 검사해. 이전 버전의 실행 파일을 새 태그로 발행하려면
종료 코드 1로 실패해. HTTP `/version` 응답도 Go 테스트에서 따로 검사해.
일반 브랜치 CI는 표시 일관성을 검사하고, 태그 CI는 `GITHUB_REF_NAME`까지
대조해. 태그 CI는 태그를 올린 뒤 실행되므로 발행 전 `--tag` 검사를 생략하면 안 돼.

알파의 지원 환경은 WSL 2 / Ubuntu 24.04 / Linux amd64야. 이번 변경에 대해
실행한 검사와 생략한 검사는 GitHub 릴리스 본문에 기록해. 이전 재부팅·장시간
부하 기록은 [별도 문서](reboot-soak.md)에 있으며 새 릴리스에서 반복했다는 뜻은 아니야.
