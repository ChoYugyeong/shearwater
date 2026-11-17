# New app

1. Confirm namespace, app name, image, container port, Service port and replicas.
   An image is required: this interface does not build source code. Do not add
   mounts, privileges or public routes that were not requested.
2. Use `sw_app_plan` with action `create`. Copy the requested values exactly.
   Existing apps must use the update flow; do not silently adopt or replace them.
3. Report the exact plan hash and `applied=false`. Request operator review of the
   saved plan. `sw plan --plan=HASH` displays it; it does not grant approval.
4. After explicit local approval, use the available apply connection. External
   MCP needs `--allow-write`. A one-question Qwen client needs a new invocation
   with `--allow-write --approved-plan=HASH`; the ordinary planning connection
   does not expose apply. Do not combine this invocation with `--target`.
5. Apply once, then use `sw_verify_plan` with the SAME hash. If still preparing,
   report that state and verify the same plan later. Reapplying is not waiting.
6. Report HTTP reachability as untested unless a separate actual HTTP request
   succeeded. A Service's cluster IP is not a promised Windows/public URL.

Review, approval, apply and verification must use the same `--config-dir`,
`--listen` and, if customized, `--plan-dir`. Expired or changed plans require
fresh review and approval. The model cannot renew or manufacture approval.
