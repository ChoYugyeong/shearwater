# Update one existing app

Prefer an operator-selected connection such as `--target=deploy/web -n demo
--task=image --allow-write`. It pins the app's UID and generation. Do not assume
`default` or copy an app ID from an earlier question.

- Image task: call `sw_target_image` with only the requested image. For multiple
  containers the operator must select `--target-container` when connecting.
- Scale task: call `sw_target_scale` with only the requested replica count.
- For either task, return the tool's complete `answer` JSON. `applied=false`
  remains false until the separate reviewed plan is actually applied.
- If the target is stale, stop and explain. Do not open a new connection to
  silently accept a replacement object or new generation.

Then follow deploy guide steps 3–6: review, exact local approval, one application,
and verification of the receipt's original goal. The focused planning connection
has no apply tool. A separate approved execution connection is intentional.

Keep existing volumes, environment, readiness checks and other containers.
Do not rebuild a whole Deployment from memory to change one image. If focused
tools are unavailable, use current `sw_observe` evidence and `sw_app_plan` with
the observed UID and resourceVersion; explain any conflict before replanning.
