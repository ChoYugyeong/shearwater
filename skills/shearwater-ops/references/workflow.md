# One connected app

This guide requires the development `sw_workflow` tool. Check the actual catalog.
The operator connects once using `sw connect`; the private workflow holds target,
server identity, pending plan, original write goals and last HTTP-verified image.

- Call with only `action` and optional `image`. Never add name/object/namespace/port/UID.
- Deploy: `{"action":"deploy"}`. The initial image is already selected.
- Update with a staged build artifact: `{"action":"update"}`. Do not retype its
  digest. The operator selects it with `sw workflow stage-image --image-file=...`
  or the initial connect command. Without staging, update requires the exact new
  `image` argument. Other fields remain intact.
- Diagnose and prepare recovery: call `diagnose`, use its actual evidence, then
  `recover` to plan the last verified image. An absent verified image is a real blocker.
- Stop at `awaiting_approval`. The operator uses `sw workflow review --file=...`
  and `sw workflow approve --file=... --confirm=EXACT_HASH`. No model approval exists.
  The native chat host ends the turn at this saved boundary. External clients
  should honor `handoff: local_approval`; it is not permission to approve or apply.
- After that exact approval, call `continue`. It applies once and waits briefly for
  the original goal and actual internal Service HTTP. Use `verify` to wait longer.
- A fresh `healthy` tool result is the completion evidence. Do not replace UID or
  generation with newer observations or infer HTTP success from ready containers.
- For stale, uncertain or partial writes, report the evidence and stop automatic
  writes. Never repeat an apply to make it appear successful.

Report the actual structured result and remaining limits. Internal Service HTTP
does not verify public DNS, HTTPS, login or database migrations. Recovery preserves
the current configuration except the image; it is not a data rollback.
