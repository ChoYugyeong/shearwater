# Delegating an Existing Docker Compose App to AI — `sw compose serve`

*Translated from the [Korean original](../compose-backend.md). If they differ, the Korean version is current.*

> Available since alpha.14. Supports the delegation path (`sw task`) only. Verified against real Docker:
> - Claude, DeepSeek, and Qwen completed all 7 stages (deploy, update, fault diagnosis, recovery) using this flow. Qwen passed 1 out of 2 runs.
> - Tested with a regular user operator in the docker group and a separate AI account without docker privileges.

You can delegate a single service already running under Compose to AI using the same delegation flow, without migrating to the Shearwater engine.

- Exposes the same operations API (`/shearwater/operations`) as the engine in front of a single Compose project.
- The AI receives the same things: a delegation token scoped to one service, exact plan hashes, operator approval, execution records, and per-replica HTTP evidence.
- The AI does not receive the docker socket or the operator token.

## Getting Started

Run this from the operator account. This account must be able to use docker. It does not need to be root; being in the `docker` group is enough. The `docker compose` plugin must also be installed.

```bash
sw compose serve --file /srv/shop/compose.yaml --project shop --listen 127.0.0.1:16444
```

- Operator certificates and tokens are created in `~/.config/shearwater-compose`. You can change this location with `--config-dir`.
- Task records are stored in `~/.local/state/shearwater-compose`. You can change this location with `--data-dir`.
- If you have multiple compose files, repeat `--file` in the same order as `docker compose -f`.

In another terminal, delegate a single service. In Compose mode, pass the project name with `-n`. Running the AI under a different OS account from the operator is recommended. Create the delegation directory inside an exchange directory where it can be handed off between the two accounts:

```bash
sudo install -d -o "$USER" -m 0755 /srv/sw-handoff        # One-time setup
sw task create --config-dir ~/.config/shearwater-compose --listen 127.0.0.1:16444 \
  -n shop --name web --port 80 --allow update,recover --expect-body "Welcome" \
  --out /srv/sw-handoff/web --ttl 2h
sudo chown -R ai-user: /srv/sw-handoff/web                # Hand off to the AI account
```

From the AI account, connect using the `work.json` inside that directory with `sw mcp --workflow=/srv/sw-handoff/web/work.json --allow-write`.

- **A directory created in the operator's home cannot be handed off.** If you create it under the operator's home (such as `--out ~/agent-web`), the AI account cannot open it (`permission denied`). Moving it elsewhere does not work either because `work.json` records the directory's absolute path. Only create it in the home directory when running the AI under the same account.
- **Do not add the AI account to the `docker` group.** After handoff, the AI account can only access the delegation directory, not the operator token or the docker socket.

Plan review and approval are the same as in engine mode: `sw task review`, `sw task approve --confirm <hash>`, `sw task stage-image`.

## How It Works

| Item | Behavior |
| --- | --- |
| App identity | Compose project + service name |
| Generation | The container's `shearwater.io/generation` label |
| Apply | Writes an override file containing only the image and Shearwater labels to `--data-dir/compose/`, then runs `docker compose up --detach --no-deps --no-build --pull missing <service>`. Does not modify your compose files. |
| Duplicate apply protection | Checked using the container's plan hash label and the override file. Even if a response is lost and `continue` is retried, the same plan is not applied twice. |
| Status reads | Calls the Docker Engine API directly over the Unix socket. |
| Readiness check | Containers must be running and `healthy` if a healthcheck is defined. The container count must also match the desired replica count. |
| HTTP evidence | Checks each container IP directly. If the service exposes a host port, checks that port as well. |
| Failure handling | Failures that leave containers unchanged (such as an image pull failure) close the plan so a recovery plan can be created immediately. Failures where containers partially changed can be finished by retrying `continue` on the same plan, or discarded by the operator. |
| Manual changes | Running `docker compose up` again without the override drops the labels. The task reports this state as `stale` and does not accept it as the target. |

## Differences from Engine Mode

- **Brief downtime during updates:** Compose recreates containers, so a brief interruption can occur during updates. Use the Shearwater engine if you need zero-downtime rolling updates.
- **Scope:** Supports only the `sw task` delegation path. The legacy `sw connect` flow and kubectl are not available.
- **New services:** A service defined in the compose file with no existing containers can be started for the first time with `deploy`. Services not defined in the compose file cannot be created.
- **Direct changes while running:** If you run `up` again on a managed service without going through `sw compose serve`, the task stops (stale). The operator must create a new delegation.
- **Security boundary:** The delegation token cannot access other services or approval functions on this server. However, if the AI can reach the docker socket or operator directories through another tool, this does not block that access. Separate the OS accounts.
