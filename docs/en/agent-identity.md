# Create an agent identity in one command (`sw agent`)

*Translated from the [original guide](../agent-identity.md) (in Korean).*

`sw agent` creates everything an AI agent needs to connect to Shearwater inside one namespace in a single command. It uses the same administrator API as `sw user` and marks the agent's RoleBinding (and newly created namespace) with the label `shearwater.io/agent=NAME`.

```sh
sw agent add ops-bot --namespace team-a
sw agent list
sw agent remove ops-bot
```

## What `sw agent add` creates

By default, `sw agent add NAME --namespace NS` creates `./NAME-agent` (mode `0700`, files `0600`). It refuses to overwrite an existing non-empty directory unless `--force` is passed.

| Path | Purpose |
| --- | --- |
| `kubeconfig` | Kubeconfig scoped to namespace `NS` (default namespace `NS`). Uses `--server` or the control plane's advertised address (`--advertise-address`) |
| `sw/ca.crt`, `sw/token` | Connection folder for `sw mcp --config-dir <DIR>/sw --namespace NS` and `sw` CLI commands. **Never contains `admin.token` or the administrator token** |
| `mcp/claude.json` | Claude Code `.mcp.json` configuration |
| `mcp/codex.toml` | Codex CLI `[mcp_servers.shearwater]` configuration |
| `mcp/gemini.json` | Gemini CLI `settings.json` `mcpServers` configuration |
| `mcp/qwen.json` | Qwen CLI `mcpServers` configuration |
| `skills/shearwater-ops/` | Copy of the operations skill embedded in the `sw` binary |
| `AGENT.md` | One-page guide for the agent (name, namespace, RBAC role, human approval of exact plan hashes, untrusted evidence rule, and skill path) |

The generated directory contains the agent's bearer token (`kubeconfig` and `sw/token`). Keep it private (`0700`) and out of version control.

## Options

```sh
sw agent add NAME --namespace NS \
  [--role edit|view|admin] \
  [--client claude|codex|gemini|qwen|all] \
  [--out DIR] \
  [--server URL] \
  [--config-dir DIR] \
  [--listen ADDR] \
  [--force]
```

- `--role`: defaults to `edit`. Use `view` for read-only agents or `admin` for namespace-scoped RBAC administration.
- `--client`: defaults to `all`. You may also pass a comma-separated list such as `--client claude,codex`. Only the selected client config files are written, and the command prints a ready-to-run attach command for each selected client.
- If the namespace does not exist yet, the server creates it with `pod-security.kubernetes.io/enforce=baseline` and `shearwater.io/agent=NAME`. The RoleBinding (`sw-user.NAME`) is also labeled `shearwater.io/agent=NAME`.

## `sw mcp` tools with a namespace-scoped agent token

The agent connection folder (`sw/`) stores the agent token in `token` instead of `admin.token`, and the generated MCP configs and attach commands pass `--namespace NS`. `sw mcp --namespace NS` uses `NS` as the default when a tool call omits `namespace`. `sw` CLI and `sw mcp` read `token` when present and never fall back to another credential when a call is forbidden.

| Tool | Behavior with a namespace-scoped token | Notes |
| --- | --- | --- |
| `sw_capabilities` | Works | No API call required |
| `sw_status`, `sw_tree` | Works (default `NS` or `namespace: NS`) | Reads only lists in `NS`. If `/api/v1/nodes` returns 403, continues and reports `nodes: not visible to this identity`. Cluster-wide calls without a namespace or calls for another namespace return HTTP 403 (`UNAUTHORIZED`) |
| `sw_observe`, `sw_verify` | Works (default `NS` or `namespace: NS`) | Reads only Deployments, ReplicaSets, Pods, Services, and EndpointSlices in `NS`. Includes `nodes: not visible to this identity` in `notes` when nodes are hidden. Other namespaces return HTTP 403 |
| `sw_verify_plan` | Works when every goal in the receipt is in `NS` | Reads only `NS` when all receipt goals share one namespace. Multi-namespace or other-namespace goals return HTTP 403 |
| `sw_changes` | Works (default `NS` or `namespace: NS`) | Server filters change entries according to the caller's RBAC |
| `sw_get` | Works (default `NS` or `namespace: NS`) | Other namespaces and cluster resources (`nodes`, `namespaces`) return HTTP 403 (`UNAUTHORIZED`) |
| `sw_logs` | Works (default `NS` or `namespace: NS`) | `view` and above can read `pods` and `pods/log` |
| `sw_events` | Works (default `NS` or `namespace: NS`) | `view` and above can list `events` in `NS` |
| `sw_why` | Works (default `NS` or `namespace: NS`) | Reads Pod, Events, and logs within `NS` |
| `sw_receipt` | Works | Reads local plan receipt files |
| `sw_plan`, `sw_app_plan`, `sw_apply` | Works with `--allow-write` under `edit` or `admin` (`NS`) | Immediate post-apply verification (`verifyAppliedPlan`) also succeeds when all goals are in `NS`. Fails with HTTP 403 under `view`, for other namespaces, or for `Namespace` objects. Local `sw approve` of the exact plan hash is still required |
| Focused tools (`sw_target_*`) | Works (`sw mcp --target=deploy/NAME -n NS`) | Queries only Deployment, ReplicaSet, and Pods in `NS` |

> Still requires administrator access: cluster-wide inspection without a namespace (`sw status`, `sw tree`, `sw observe`), node inspection (`nodes`), listing or creating `namespaces`, accessing other namespaces, multi-namespace plans and verification, and `sw user`, `sw agent`, `sw node`, or server lifecycle commands.

## Listing and removing agents

```sh
sw agent list
sw agent remove ops-bot
```

- `sw agent list` prints only identities created with `sw agent add`, showing `NAME`, `NAMESPACE`, `ROLE`, and `CREATED` (RFC 3339). Non-agent users created with `sw user add` are omitted.
- `sw agent remove NAME` revokes the token and deletes the labeled RoleBindings. **The namespace and its applications are kept**, and the output states this explicitly.

See also: [Access control and tenant workloads](access-control.md)
