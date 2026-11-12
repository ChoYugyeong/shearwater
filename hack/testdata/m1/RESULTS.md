> 공개 준비 과정에서 개인 경로·내부 세션 식별자·로컬 앱 이름을 일반화했어. 측정값과 당시 검증 범위는 유지했고, `<private-evidence>`는 공개 저장소에 없는 원본 기록을 뜻해.

# M1 actual kubectl results

- Server source: `326187edd52be9bde6b4426870939004313834cb`.
- Date: 2026-11-11 KST (trace timestamps are UTC on 2026-11-10).
- Final M1: **103 passed, 0 failed** — Linux kubectl v1.36.6: 91 checks; Windows v1.36.0: 12 checks.
- M0 regression: **47 passed, 0 failed** on the same server implementation.
- No changes to `internal/` or `cmd/`, no commits or pushes.
- The test server and background clients were stopped; port 16443 was verified free.

## Re-run

From WSL Ubuntu-24.04, in `$SOURCE_DIR`:

```sh
WIN_KUBECTL=/mnt/c/ProgramData/chocolatey/bin/kubectl.exe bash hack/m1-check.sh
WIN_KUBECTL=/mnt/c/ProgramData/chocolatey/bin/kubectl.exe bash hack/m0-check.sh
```

Requirements already present in the measured environment: Go/kubectl from `hack/env.sh`, bash, curl, Python 3, GNU timeout, util-linux script, sed. No root, containers or installed software changes. Each invocation creates its own ext4 state directory under `~/.cache/shearwater-dev/`, builds sw and starts it on 16443 (override `SW_PORT` only if needed). A busy port is refused. Data and evidence are retained.

The script accumulates assertions and exits nonzero if any fail. Each check records its command, exit code, output file and completed-request trace range in `checks.jsonl`. `report.md` contains failures with their outputs and request lines. Watch output and edit terminal output have separate files. Because watch requests are recorded when they end, a watch's request line may appear after the check that started it.

## Evidence for the final run

- M1: `$HOME/.cache/shearwater-dev/m1-20261111-001244-539`.
- M0: `$HOME/.cache/shearwater-dev/m0-20261111-000956`.
- `checks.observed.jsonl`: the final run's 103 assertions, copied without changing results. This is historical evidence, not an expected-output fixture.
- `trace.samples.jsonl`: 35 verbatim request records selected from the final trace (PATCH, watch, Protobuf, held-object DELETE).
- `docs/kubectl-trace.md` rows 26–41 describe the observed protocols.

## Scope and initial failures

The first exploratory run (`m1-20261111-000707-569`) was 95/8; the second (`m1-20261111-001059-544`) was 101/2. The final run above supersedes their pass/fail counts for the supported M1 cases.

- Service's printed column is `PORT(S)`. The initial test expected `PORT`; corrected the assertion.
- Stored-field warnings are emitted for newly set/changed values. The initial test incorrectly expected a second warning for an unchanged livenessProbe. The final test checks both `Warning:` and the livenessProbe field name in the creation output.
- `hostPath.type: DirectoryOrCreate` is not present in the current server's own type. A Strict apply returns 400 with `unknown field "spec.template.spec.volumes[0].hostPath.type"`. The final volume swap uses supported `hostPath.path` only. This does **not** establish support for hostPath.type. The exact failing request is in `hostpath-type.failure.jsonl` and the exploratory run's report contains kubectl's full output. To reproduce against a fresh test server:
  ```sh
  kubectl apply -f hack/testdata/m1/apply-05-strategy.json
  kubectl apply -f hack/testdata/m1/hostpath-type.json
  ```
- Noninteractive WSL edit stalled before PATCH, even with stdin from /dev/null. Running just edit inside `script` with a PTY and `KUBE_EDITOR="sed -i ..."` succeeded. The script uses this environment adjustment plus an enforced kill-after timeout; no server workaround was made.

The final run proves API behavior without running containers. Watch-list is checked via a raw request through its initial ADDED events and end BOOKMARK. A real controller's rollout completion, logs, runtime execution, volume mounting and probes are outside this run. Windows checks are a 12-case subset, not a repetition of all 91 Linux cases.

## Files

- `hack/m1-check.sh`: runner and assertions.
- `hack/testdata/m1/assert.py`: stdlib-only stream/trace assertions and report writer.
- `pod.json`, `service.json`, `namespace.json`, `watch.json`, `finalizer.json`, `warning.json`: command fixtures.
- `apply-00.json` through `apply-06-volume.json`: successive desired Deployment states.
- `hostpath-type.json`: optional-field limitation reproducer; not a supported-case success fixture.
- The observed checks, selected traces, limitation trace and this result document are evidence for reviewer handoff.
