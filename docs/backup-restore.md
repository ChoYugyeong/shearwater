# 백업과 복원: 무엇을 살릴 수 있는지부터 확인하기

alpha.4의 `sw backup`은 새 비공개 폴더를 만들고 마지막에 `manifest.json`을 써.
각 파일의 SHA-256, 크기, 권한·소유자와 원본 경로가 목록에 들어가. SHA-256은 파일 손상을
찾는 값이지 디지털 서명이 아니야. 인증키도 포함되므로 이 폴더를 Git이나 공개 저장소에 올리지 마.

## 두 가지 백업 방식

| 방식 | 들어가는 것 | 조건 |
|---|---|---|
| 기본 | SQLite의 일관된 snapshot, CA·서버 인증서/키, 관리자 토큰 | 서버 실행 중 가능 |
| `--include-volumes` | 기본 내용 + Pod 디렉터리(emptyDir 포함) + 선언된 hostPath 파일/디렉터리 | 서버와 Shearwater 컨테이너를 모두 멈춘 상태 |

기본 백업은 **앱 데이터 전체의 백업이 아니야**. SQLite는 서버가 알고 있는 리소스·IP·이력이고,
앱이 hostPath에 쓴 파일은 별도 데이터야. 브라우저 localStorage, 외부 DB, 이미지와 이미지의
쓰기 레이어, 로그, 소켓과 네트워크 네임스페이스는 두 방식 모두 제외해.
앱 데이터베이스는 해당 앱의 정상 종료 또는 자체 백업 절차를 먼저 따라야 해.

## 실행 중 DB·인증 백업

```bash
sudo sw backup --output=/var/lib/shearwater/backups/before-change-001
sudo sw restore --from=/var/lib/shearwater/backups/before-change-001 --verify-only
```

상위 `backups` 폴더는 먼저 소유자만 쓰는 디렉터리로 준비해. 출력 폴더 자체는 없어야 해.
기존 인스턴스가 기본 경로를 쓰지 않으면 backup에 동일한 인스턴스 옵션을 전부 전달해.
`VACUUM INTO`로 SQLite의 일관된 snapshot을 만든 뒤 `integrity_check`와 스키마 버전을 검사해.
실행 중 DB 파일 하나만 복사해서 WAL의 최신 기록을 놓치는 방식을 쓰지 않아.

기본 최대 복사량은 1 GiB야. 필요하면 `--max-bytes=...`로 명시해.
실패한 출력 폴더는 원인 조사용으로 남고, 완료 manifest가 없으면 정상 백업으로 인정하지 않아.
같은 이름으로 덮어쓰는 대신 새 출력 이름을 사용해.

## 앱 볼륨까지 포함하기

이 작업에는 앱 중단 시간이 있어. 사용 중인 서버에서 예제를 그대로 실행하지 마.
서비스 관리 중이면 먼저 `sw service stop`으로 자동 시작을 멈추고, 같은 옵션의
`sw down --all`로 Shearwater 태스크와 네트워크를 정리한 뒤 백업해. DB·인증·볼륨은 보존돼.

```bash
sudo sw backup --include-volumes --output=/var/lib/shearwater/backups/cold-001
```

프로그램은 서버 잠금과 런타임 컨테이너 목록을 확인하고 아직 사용 중이면 거절해.
hostPath는 `/opt/shearwater/` 또는 `/var/lib/shearwater/` 아래의 선언된 경로만 지원해.
시스템 디렉터리 전체, 백업 자신을 포함하는 경로, 심볼릭 링크·소켓·장치 파일은 거절해.
다른 hostPath는 앱별 백업 도구를 써. 파일 복사 중 크기나 수정 시각이 바뀌어도 실패해.
외부 프로그램이 파일을 쓰지 않는 것은 운영자도 확인해야 해.

## 복원은 새 폴더에서

```bash
sudo sw restore --from=/var/lib/shearwater/backups/cold-001 \
  --to=/var/lib/shearwater/recovered-001
```

먼저 모든 체크섬·필수 파일·경로·SQLite 무결성을 검사해. 대상 폴더가 있으면 거절해.
복사 중에도 해시를 확인하고, 데이터는 `data/`, 인증은 `config/`, hostPath 사본은
`volumes/`에 풀어. 기존 서버를 멈추거나 파일을 덮어쓰거나 자동으로 시작하지 않아.
root 복원에서는 앱 파일의 uid/gid와 권한을 복원하고, 인증 파일은 비공개 권한으로 둬.

**hostPath의 선언은 원래 경로를 유지해.** 명령 끝에 출력되는
`원래 경로 <- 복원된 사본` 목록을 보고 앱을 멈춘 상태에서 데이터를 복구하거나,
검토한 새 매니페스트로 경로를 바꿔야 해. 이 도구가 원래 경로를 자동 덮어쓰지는 않아.
emptyDir는 복원된 `data/pods/` 안에 있어 같은 UID로 다시 시작할 때 이어받아.

앱 데이터를 확인한 뒤, 원래 인스턴스의 런타임·주소·노드 옵션에 복원한
`--data-dir=.../data --config-dir=.../config`를 조합해 시작해.
같은 네트워크 공간에서 기존 클러스터와 복원 클러스터를 동시에 켜면 안 돼.
복원했다고 원래 DB 파일을 지우지 말고 결과를 확인할 때까지 보존해.

복원 뒤 `sw status`, `sw get`, `sw logs`, `sw why`, 실제 HTTP와 앱 데이터 확인을 함께 해.
DB가 정상이라는 것만으로 사용자의 파일이나 브라우저 저장 내용까지 돌아왔다는 뜻은 아니야.
