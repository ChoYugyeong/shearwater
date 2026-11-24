# Install Shearwater v0.1.0-alpha.14 and deploy your first web app
*Translated from the [Korean original](../install.md). If they differ, the Korean version is current.*

To start with the installer, follow the [installation, authentication, and AI connection guide](../onboarding.md). This guide covers prerequisites, manual installation, and upgrades for an existing server.

This guide is for first-time users running **WSL 2 / Ubuntu 24.04 / x86_64(amd64)**. It uses a prebuilt binary, so **you do not need to install Go or build from source**. This binary has not been validated for ARM64, native Windows execution, or other Linux distributions.

Shearwater is an **experimental alpha** for trusted local administrators. It supports a single node, IPv4, and TCP ClusterIP Services. It does not provide DNS, RBAC, or NetworkPolicy. It stores securityContext settings without enforcing them. Shearwater runs configured probes and uses startup and readiness probe results to determine Ready status. For apps without probes, use the curl command below to check the actual response.

To decide what to test first, see the [first-run guide](../first-try.md). The steps below are for a new installation. If you already have a running instance, read **Upgrade an existing installation to alpha.14** first.

## 0. Install on a new Ubuntu or Debian server with `install.py --host`

> The installer includes `--host` starting in alpha.14. The `install.py` shipped with alpha.13 and earlier does not have this option.

On a dedicated Ubuntu or Debian server (VPS or VM) running systemd, the installer handles sections 1 through 4 for you. Preview the changes first. The installer makes changes only when you add `--apply`.

```bash
sudo python3 install.py --version vX.Y.Z --host            # Preview only. No changes are made.
sudo python3 install.py --version vX.Y.Z --host --apply    # Install and start the server
```

With `--apply`, the installer performs these steps in order.

| Step | Action |
|---|---|
| Packages | Installs `ca-certificates`, `iproute2`, and `iptables` if missing. It installs `containerd` and `runc` from the distribution's signed repository only if containerd is absent. **It reuses an existing containerd installation, including one supplied by Docker.** Docker's `containerd.io` conflicts with the distribution packages. |
| containerd | `systemctl enable --now containerd` |
| IPv4 forwarding | Writes `net.ipv4.ip_forward = 1` to `/etc/sysctl.d/90-shearwater.conf` and applies it. This setting affects the entire host. |
| Release | Verifies the checksums, installs the release in `/opt/shearwater/releases/<version>`, and creates the `/usr/local/bin/sw` link. It stops if another program already provides `sw` there. |
| Service | Runs `sw service install` and `start`, then waits up to 120 seconds for the node to become Ready. It leaves an existing `shearwater.service` unchanged. |
| Regular user | If invoked with `sudo`, runs `sw setup --user <user> --apply` to give the invoking user client credentials. |

Running the installer again skips completed steps. For an offline installation, use `--archive` and `--sums` to specify the release archive and checksum file you downloaded in advance.

- **Deploy your first app after installation.** Run `sudo kubectl --kubeconfig /etc/shearwater/kubeconfig apply -f /opt/shearwater/releases/<version>/examples/web.yaml`. You need to install kubectl separately.
- **Run queries as a regular user.** With no options, `sw status`, `sw get`, `sw observe`, and `sw task` use the connection saved by `sw setup` in `~/.local/state/shearwater/client`. Explicit `--config-dir` or `--listen` values take precedence.

**Tested on 2026-11-19** on a fresh Ubuntu 24.04.5 cloud VM under Hyper-V with 512–1536 MiB of dynamic memory.

- The installer installed the distribution's containerd 2.2.1 and runc 1.3.4. The example app returned HTTP 200.
- A second installer run made no changes.
- After a reboot, containerd and the service started automatically. The server came up in 4.6 seconds, and the app responded again.
- The installer itself took about 5 seconds. Of the 538 seconds total, about 533 seconds were spent in `apt-get update` retrying failed connections to an Ubuntu mirror.
- Testing has not yet covered a cloud VPS with a public IP or a Debian host.

## 1. Check WSL and prerequisites

In Windows PowerShell, run `wsl --list --verbose` to identify your distribution. Set `$distro` below to the exact name in the `NAME` column. A default installation may be named `Ubuntu`. `Ubuntu-24.04` is an example. Once the name matches, start Ubuntu.

```powershell
wsl --list --verbose
$distro = 'Ubuntu-24.04'  # Use the actual name from the NAME column
wsl -d $distro
```

**Run the remaining commands in an Ubuntu Bash terminal.**

```bash
uname -m                         # Must print x86_64
cat /etc/os-release              # Check for an Ubuntu 24.04 release
findmnt -n -o FSTYPE /sys/fs/cgroup  # cgroup2
cat /proc/sys/net/ipv4/ip_forward    # 1
for tool in containerd runc iptables iptables-restore ip curl sha256sum tar; do
  if command -v "$tool" >/dev/null; then
    printf 'found: %s\n' "$tool"
  else
    printf 'missing: %s\n' "$tool"
  fi
done
```

Run only one Shearwater instance per network namespace. WSL distributions with different names can share the host network. If `sw-br0` or `sw-svc0` already exists, or a Shearwater app is running, do not run this fresh-install example alongside it. Keep SQLite data on the WSL ext4 filesystem at the default `/var/lib/shearwater`. Do not move it to `/mnt/c`.

On a **fresh, dedicated Ubuntu installation** with no container runtime, install the required packages as shown below. Do not use these commands to overwrite the containerd installation supplied by Docker.

```bash
sudo apt-get update
sudo apt-get install --no-install-recommends ca-certificates curl iproute2 iptables containerd runc
sudo systemctl start containerd
```

The last command requires systemd. Without systemd, you need a separate procedure to run containerd. If you use an existing runtime, use its socket and leave its configuration file unchanged. The default socket is `/run/containerd/containerd.sock`.

If IPv4 forwarding is 0 and you have decided to use this host for Pod networking, run `sudo sysctl -w net.ipv4.ip_forward=1` to enable it for the current boot. This is a host-wide setting. If other apps share the host, check the impact first. After a reboot, check the value again and verify that containerd is running.

## 2. Download, verify checksums, and install

The following block installs only into a new version directory. It stops if the directory already exists. If any step fails, do not continue. The commands verify the SHA-256 checksum before extracting the archive. They do not execute the download directly in a shell.

```bash
(
  set -euo pipefail
  version=v0.1.0-alpha.14
  base="https://github.com/ChoYugyeong/shearwater/releases/download/$version"
  archive="shearwater-${version}-linux-amd64.tar.gz"
  install_dir="$HOME/.local/share/shearwater/$version"
  [ "$(uname -m)" = x86_64 ] || { echo 'This file is for Linux x86_64.'; exit 1; }
  if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
    echo "Already exists: $install_dir; not overwriting an existing install."
    exit 1
  fi
  download_dir=$(mktemp -d /tmp/shearwater-download.XXXXXXXX)
  curl --fail --location "$base/$archive" -o "$download_dir/$archive"
  curl --fail --location "$base/SHA256SUMS" -o "$download_dir/SHA256SUMS"
  (cd "$download_dir" && sha256sum --check --strict --ignore-missing SHA256SUMS)
  mkdir -p "$install_dir"
  tar -xzf "$download_dir/$archive" --strip-components=1 -C "$install_dir"
  (cd "$install_dir" && sha256sum --check --strict sw.sha256)
  "$install_dir/sw" version
  printf 'Installed in: %s\nDownloads kept in: %s\n' "$install_dir" "$download_dir"
)
```

Look for `...tar.gz: OK` and `sw: OK`. If a hash differs, check the download before running the binary. SHA256SUMS verifies file integrity against the same release. It does not provide a separate digital signature.

The archive contains `sw`, `sw.sha256`, this guide (`INSTALL-ko.md`), the troubleshooting guide (`TROUBLESHOOTING-ko.md`), and guides to automatic startup, backup and restore, probes, AI connections, connection continuity, and your first run. It also includes `BUILDINFO.txt`, `examples/web.yaml`, `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`, and `LICENSES/`. Images, containerd, and kubectl are not included. Include the license notices when redistributing the binary.

**Check the version.** The binary should print the release version shown below. If it prints a different value, check the executable path first. The bundled `BUILDINFO.txt` contains the public source commit, build information, and file checksums.

```bash
"$HOME/.local/share/shearwater/v0.1.0-alpha.14/sw" version
# sw 0.1.0-alpha.14 (Kubernetes API v1.36.0+shearwater.0.1.0-alpha.14)
```

The first number is the Shearwater product version. `v1.36.0` is the Kubernetes API compatibility level. The first release, alpha.1, displayed `sw 0.0.1`. This mismatch was fixed in alpha.2.

### Connect an AI client for the first time

Follow the [installation, authentication, and AI connection guide](../onboarding.md) to run `sw setup` and generate the MCP configuration. The release provides `install.py`. The archive includes `hack/install.py`, `ONBOARDING-ko.md`, and `ONBOARDING-VERIFICATION-ko.md`. See also [Deploy, update, diagnose, and recover with one connection](../agent-workflow.md).

`sw connect` saves the target and connection details. Subsequent operations use the image specified in the build output file and require local approval for the exact operation. The [trial app walkthrough](../agent-workflow-trial.md) includes success criteria and a form for recording results. The same guidance is available in `AGENT-WORKFLOW-ko.md`, `CHOOSING-SHEARWATER-ko.md`, and `AGENT-WORKFLOW-TRIAL-ko.md` in the archive. You can continue using the existing focused mode.

Once the app is running successfully, follow the [AI workflow](../agent-interface.md) and [Focus on one app](../focused-tools.md). The Linux archive also contains `AGENT-INTERFACE-ko.md`, `FOCUSED-TOOLS-ko.md`, and `hack/chat-windows.py`. Connect your existing Claude, Codex, or DeepSeek app through MCP. For a local Qwen installation, use `sw chat`.

`AGENT-SKILLS-ko.md` and `skills/shearwater-ops/` describe how to generate a plan, have a person review and approve it, execute the approved plan, and verify the result. Follow the [task-specific guide](../agent-skills.md). Have the AI read `SKILL.md`, then provide only the detailed files for the task. Reading these files does not grant server access or approval privileges.

If Qwen runs on Windows, you can use the separate `chat-windows.py` file from the release. Find its hash in `SHA256SUMS` from the same release and compare it with the output of `Get-FileHash .\chat-windows.py -Algorithm SHA256` in PowerShell. The helper connects Ollama on Windows to sw in WSL. It does not install a model or configure a new port. The AI workflow above covers command arguments and user-owned credentials. To verify web access, check the actual HTTP response as well as API readiness.

### Upgrade an existing installation to alpha.14

The download above installs into a new version directory and preserves existing files. **Stop the existing server before starting the new server in section 4.** First record the current server's executable path and its values for `--data-dir`, `--config-dir`, `--run-dir`, `--log-dir`, `--containerd-socket`, `--node-name`, and `--listen`. If you are unsure whether you used the defaults, check how the current server was started.

Before replacing the binary, take a consistent SQLite backup and make sure you can recover credentials, volumes, and other app data. Copying only the database file while SQLite is running can miss recent changes in the WAL. Use a consistent backup method such as the SQLite backup API.

Keep the existing binary for recovery. To roll back from alpha.5 to alpha.4 or earlier, you must also revert the new kernel Service rules. Reversing the file replacement alone is insufficient. Do not try to resolve this by deleting apps with `down --all`.

Starting in alpha.12, the first use of the `sw task` API adds the internal `agent_operations` table to SQLite. Existing app declarations and containers are not recreated. If you revert to an earlier binary, do not overwrite the current database with an old backup. To grant an AI client per-app permissions, update both the server and the client. Then follow the [delegated operations guide](agent-operations.md) to create a new restricted connection. Existing administrator credentials and `sw connect` files do not automatically convert to restricted tokens.

Upgrading from alpha.13 also preserves the database format.

- New Secret resources are stored in the existing object table.
- Replace the server binary to enable Secret support and container environment variable injection, serve the empty OpenAPI v2 document needed to apply `kind: List`, and use the new fields in plan execution records.
- The public entry point (`sw edge`) is off by default. It does not use ports 80 or 443 until enabled. See the [public entry point guide](../public-entry.md).
- `sw compose serve` runs separately from the engine and provides the same delegation workflow for an existing Compose app. See the [Compose guide](compose-backend.md).
- Replacing only the client binary is enough for regular users' read commands to use the connection saved by `sw setup` by default.

When upgrading from alpha.12, replace the server binary to enable readiness checks for new Pods and the TCP tracking policy for direct connections from the host to a Pod. After replacing the binary, check that the server's `/version` reports `v1.36.0+shearwater.0.1.0-alpha.14`. See [When the two changes take effect](../connection-continuity.md).

**Starting in alpha.4, Shearwater runs existing probes that alpha.3 only stored.** Read the settings with `sw get pods --full` and `sw get deployments --full`. Verify the actual paths, ports, and commands first. Test apps with probes in a separate QA environment before replacing the binary. Reverting to the older binary also stops probe execution.

Run `sw down` with the existing binary to stop only the server. Then run `sw up` with the new binary, using **the same paths, socket, node name, and address**. Do not use `down --all` for this step. This release preserves the database format and takes over running containers. Check `sw status`, `sw doctor`, and the app's HTTP response and logs. Verify both versions. `sw version` reports the local binary version, and `kubectl --kubeconfig /actual/kubeconfig get --raw /version` reports the server version.

**On the first upgrade from alpha.4 or earlier**, TCP sockets owned by the old process close, so requests can fail. After upgrading to alpha.5, restarting only sw on the same host preserves kernel Service forwarding. The API, health checks, and automatic recovery pause while the server is down. Requests can still fail even if the containers keep running. Compare Pod UIDs, IPs, and restart counts in `get pods`. Verify the app's stored data using a method appropriate for the app.

## 3. Set up kubectl

`kubectl` sends commands to the Shearwater API. If it is already installed, check it with `kubectl version --client`. The tested Linux version is v1.36.6. Run the following block only if kubectl is missing. It downloads kubectl to a new location under your home directory.

```bash
(
  set -euo pipefail
  tool_dir="$HOME/.local/share/shearwater-tools/kubectl-v1.36.6"
  if [ -e "$tool_dir" ] || [ -L "$tool_dir" ]; then
    echo "Already exists: $tool_dir"
    exit 1
  fi
  mkdir -p "$tool_dir"
  curl --fail --location https://dl.k8s.io/release/v1.36.6/bin/linux/amd64/kubectl -o "$tool_dir/kubectl"
  curl --fail --location https://dl.k8s.io/release/v1.36.6/bin/linux/amd64/kubectl.sha256 -o "$tool_dir/kubectl.sha256"
  (cd "$tool_dir" && printf '%s  kubectl\n' "$(cat kubectl.sha256)" | sha256sum --check --strict)
  chmod 755 "$tool_dir/kubectl"
)
export PATH="$HOME/.local/share/shearwater-tools/kubectl-v1.36.6:$PATH"
kubectl version --client
```

The PATH setting applies only to the current terminal. To use this kubectl in a new terminal, run the `export PATH=...` line again.

## Keep track of the executable path

This manual installation does not add `sw` to the system PATH or create `/opt/shearwater/current/sw`. If `sw version` fails when run by name, the installation may still have succeeded. In a new Ubuntu terminal, specify the installed executable as shown below. If you installed it elsewhere, use its actual path.

```bash
sw="$HOME/.local/share/shearwater/v0.1.0-alpha.14/sw"
test -x "$sw" && "$sw" version
printf 'Absolute Linux path to give MCP: %s\n' "$sw"
```

Use this **absolute Linux path** in the MCP configuration too. A Windows `C:\...` path or the literal string `$HOME` is not equivalent. The [AI connection guide](../agent-mcp.md) includes the Windows commands.

## 4. Start the server and deploy the web app

The following variables and function also apply only to the current terminal. Define them again if you continue in a new terminal.

```bash
sw="$HOME/.local/share/shearwater/v0.1.0-alpha.14/sw"
example="$HOME/.local/share/shearwater/v0.1.0-alpha.14/examples/web.yaml"
export PATH="$HOME/.local/share/shearwater-tools/kubectl-v1.36.6:$PATH"
kubectl_bin="$(command -v kubectl)"
k() {
  [ -n "$kubectl_bin" ] || { echo 'kubectl not found. Start with section 3 of the install guide.' >&2; return 1; }
  sudo "$kubectl_bin" --kubeconfig /etc/shearwater/kubeconfig "$@"
}

sudo "$sw" doctor --node-name=shearwater
sudo "$sw" up --node-name=shearwater
k get nodes
k apply -f "$example"
k rollout status deployment/web --timeout=120s
k get pods,service,endpointslices -o wide
```

On the first run, `doctor` may warn that the server does not exist yet. A `FAIL` for a containerd, cgroup, or network prerequisite requires action. Resolve prerequisite failures before running `up`. The example sets a fixed node name to avoid hostname differences such as uppercase letters. Use the same name for subsequent `up`, `down`, and `doctor` commands.

`sudo` supplies the administrator privileges needed to run the node and configure networking. The default kubeconfig at `/etc/shearwater/kubeconfig` contains an administrator token. Specifying its exact path, as above, helps avoid accidental changes to another Kubernetes cluster. Do not share the kubeconfig or token.

If the socket differs from the default, add `--containerd-socket=/actual/socket` to every `up`, `down`, and `doctor` command, using **the same value each time**. The regular `status` and `tree` commands query the API and do not accept node-name or containerd options.

## 5. Check the HTTP response and logs

```bash
service_ip=$(k get service web -o jsonpath='{.spec.clusterIP}')
curl --noproxy '*' --fail --retry 10 --retry-connrefused --retry-delay 1 "http://$service_ip/"
k logs deployment/web --tail=20
sudo "$sw" status
sudo "$sw" tree
```

The check succeeds when the page contains `Welcome to nginx!` and the logs show `GET / HTTP`. The example Deployment maintains one Pod. The Service forwards TCP connections to the Pod labeled `app: web`. ClusterIP is an internal address. Run the curl command above in WSL. This example does not provide Windows browser access, the `http://web/` DNS name, or `kubectl port-forward`.

### Four new diagnostic commands

Use `sw` to list resources, read app logs and events, and look up reasons for failures. The `pod` variable below holds the actual Pod name from the nginx example. For another running app, use its Pod name and namespace (`-n`). These four commands are read-only.

```bash
sudo "$sw" get pods -n default -l app=web
pod=$(k get pods -l app=web -o jsonpath='{.items[0].metadata.name}')
sudo "$sw" logs "pod/$pod" -n default --tail=20
sudo "$sw" events --for="pod/$pod" -n default
sudo "$sw" why "pod/$pod" -n default
```

By default, `logs` reads the last 50 lines, up to 64 KiB, then exits. Use `--previous` to read logs from the previous run. If the Pod has multiple containers, select one with `-c NAME`. A successful `why` means the command collected status, events, and a limited log sample. To determine whether the app is serving traffic, check the HTTP response separately. For help interpreting output and resolving errors, see the bundled `TROUBLESHOOTING-ko.md` or the [online troubleshooting guide](https://github.com/ChoYugyeong/shearwater/blob/v0.1.0-alpha.14/docs/troubleshooting.md).

## 6. Restart and shut down

In alpha.5, stopping and restarting only the server preserves existing app containers and kernel Service forwarding rules. The API, health checks, and automatic recovery pause during the restart. Service TCP connections to apps that are still running remain open. This does not cover host or WSL reboots or app container termination. For possible interruptions during the first upgrade from alpha.4 or earlier, see the upgrade section above.

```bash
sudo "$sw" down --node-name=shearwater
sudo "$sw" up --node-name=shearwater
k get pods -o wide
service_ip=$(k get service web -o jsonpath='{.spec.clusterIP}')
curl --noproxy '*' --fail --retry 10 --retry-connrefused --retry-delay 1 "http://$service_ip/"
```

To finish the example, run these commands.

```bash
k delete -f "$example" --wait=true --timeout=60s
sudo "$sw" down --all --node-name=shearwater
```

`down --all` also stops the apps managed by this Shearwater instance and takes down its dedicated network. Any other apps you added will stop too. It preserves SQLite data, certificates, volumes, and logs. It does not automatically remove the installation directory or its data, or configure automatic startup at OS boot.

After a WSL reboot, check containerd and IPv4 forwarding, then run `up` with the same paths and options. Starting with a different data path does not recover the existing state.

## 7. Troubleshooting

| Symptom | What to check first |
|---|---|
| Exec format error | Check that `uname -m` reports x86_64 and that you ran the command in WSL Bash rather than Windows. |
| Cannot connect to containerd | Check the socket path, whether containerd is running, and whether you used the same options throughout. |
| IPv4 forwarding issue or overlapping address ranges | Check the `doctor` output and existing WSL and container networks. Do not arbitrarily delete existing devices or rules. |
| ImagePullBackOff | Check the messages from `k describe pod <name>` and connectivity to the image registry. |
| Pod is Ready but HTTP fails | Check how long the app takes to become ready, the Service selector, the EndpointSlice, and the logs. |
| Commands fail in another terminal | Check that you set the sw, example, and kubectl_bin variables, defined the k function, and set PATH again. |

See the [compatibility details](https://github.com/ChoYugyeong/shearwater/blob/v0.1.0-alpha.14/docs/compatibility.md), [operations guide](https://github.com/ChoYugyeong/shearwater/blob/v0.1.0-alpha.14/docs/operations.md), and [security reporting page](https://github.com/ChoYugyeong/shearwater/security/advisories/new).

## 8. New operations features in alpha.4

Use `sw service` for automatic startup and recovery, `sw backup/restore` to back up and restore to a new path, and `sw mcp` to connect an AI client. MCP defaults to read-only access. To make changes, review the plan and approve its exact hash locally. Replacing the executable alone does not install a service or configure startup at Windows login.

Read the examples and limitations in the bundled `AUTOMATIC-START-ko.md`, `BACKUP-RESTORE-ko.md`, `HEALTH-PROBES-ko.md`, and `AGENT-MCP-ko.md`. Use `--include-volumes` only when the app is stopped. An online backup alone does not ensure that app volumes are protected.

## 9. Connection continuity in alpha.5

During a rolling replacement, Shearwater stops routing new connections to the old Pod and waits up to 30 seconds for existing Service connections to finish. It then starts the Pod's normal termination grace period. Connection continuity is not guaranteed for connections lasting longer than 30 seconds, direct connections to a Pod IP, forced deletion, or app, host, or WSL restarts. See [How connection continuity works, how to diagnose it, and how to reproduce it](../connection-continuity.md), [measurements taken during development](../connection-verification.md), and the [release history](../releases.md).

## 10. Windows forwarding and automatic startup in alpha.6

The separate `shearwater-v0.1.0-alpha.14-windows-forward.zip` contains an optional TCP forwarder for cases where only Windows localhost access stops working. It also includes a tool for automatic startup at login. The Linux archive includes the same `hack/` files. These tools require Python 3.11 or later on both Windows and WSL. They support NAT with a single IPv4 address on eth0. The package does not contain a native Windows sw binary.

Follow the [automatic startup guide](../automatic-start.md) in order. Verify the ZIP checksum, copy the files to a fixed path, check the Linux service, and register the scheduled task. The bundled `WINDOWS-LOCALHOST-ko.md` explains how to identify which part of the connection has failed. Connections drop during a reboot. When disabling automatic startup, shut down the supervisor cleanly first to prevent it from starting WSL again.

If systemd already manages sw, do not mix systemd management with the manual `down/up` procedure above. Back up the existing service file. Change only `ExecStart` to point to the new version, keeping the same options. Then run `sudo systemctl daemon-reload` and `sudo systemctl restart shearwater.service`. If the service has a different name, use that name. This procedure does not require deleting apps, restoring the database, or regenerating credentials.

Use the existing Windows forwarder's `stop` command to shut it down cleanly. Then update the scheduled task's executable path to point to the new package at its fixed location. The installer does not automatically overwrite a task with the same name. First back up the task with `Export-ScheduledTask` and check its actual launch arguments. Windows access may be briefly interrupted while the current forwarder is stopped.