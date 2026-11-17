# Diagnose and recover

If the operator selected `--target=deploy/NAME -n NS --task=diagnose`, call
`sw_target_diagnose` with `{}`. It follows actual owner UIDs and returns bounded
Pod/container evidence. Return its complete `answer` JSON, including omitted
counts and limitations. Do not guess names or use tools outside that connection.
If state is stale, stop and ask the operator to select the intended current goal.

Begin read-only. Keep the requested namespace/app fixed. Read a short summary
with `sw_observe` or `sw_why`, then inspect the failing Pod if the Deployment's
summary is insufficient. Use names actually returned by tools.

Prefer a compact Pod list (`sw_get`, resource `pods`, namespace set, default
`full=false`) to find the failed Pod, then `sw_why` for that one Pod. Do not dump
every complete object or all logs: a large tool result can exceed the response
budget and hide the relevant failure. Read only the failing container's short
logs if it started; image-pull failures often have no application logs.

Distinguish desired rollout from available old replicas. A healthy old Pod may
still serve while the new image fails to pull. State the observed waiting/error
reason and the actual image. Do not say the whole app is offline without evidence,
or that HTTP works from readiness alone.

Shearwater implements only part of Kubernetes. In the current implementation,
`progressDeadlineSeconds` and `minReadySeconds` are stored but not enforced.
Do not claim a 600-second timeout, `ProgressDeadlineExceeded`, or guaranteed
zero downtime from Kubernetes defaults. Check actual tool evidence and the
project's compatibility guide before promising an unsupported behavior.

For a failed image update, propose returning to the operator-confirmed known-good
image, preserving existing settings and data. Do not guess that `latest` is safe.
Recovery is a NEW plan against the current app state. Have the operator start a
focused image connection, review/approve its exact plan, apply once and verify
that plan's original goal. The original failed plan must remain failed/stale in
the record; do not relabel it as a success after recovery.
