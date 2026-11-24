# Give an agent a scoped kubeconfig

*Translated from the [original guide](../access-control.md) (in Korean).*

As an administrator, use the connection details saved on the server to create a kubeconfig for `alice` in the `team-a` namespace. Choose `view` for an agent that only needs to read resources. Choose `edit` for an agent that deploys workloads. An agent with `edit` can also read and change Secrets in the same namespace.

```sh
sw user add alice --namespace team-a --role view --out ./alice.kubeconfig
kubectl --kubeconfig=./alice.kubeconfig auth whoami
kubectl --kubeconfig=./alice.kubeconfig auth can-i list pods -n team-a
kubectl --kubeconfig=./alice.kubeconfig auth can-i list pods -n team-b
kubectl --kubeconfig=./alice.kubeconfig auth can-i --list -n team-a
```

Give the agent only `alice.kubeconfig`. In this example, `auth whoami` reports the username `alice`. The two checks for permission to list Pods should return `yes` for `team-a` and `no` for `team-b`.

The default namespace in the kubeconfig is a convenience. The server evaluates RoleBindings and ClusterRoleBindings on every request, so specifying `-n team-b` grants no additional permissions. An unauthorized list request returns HTTP 403 with a Status object containing `User "alice" cannot list resource "pods"`.

### Manage roles and tokens

| Role | Permissions |
| --- | --- |
| `view` | Read workloads, Services, and other resources. Excludes Secrets. |
| `edit` | Read and change workloads, Services, Secrets, and other resources. |
| `admin` | Manage Roles and RoleBindings in the namespace and read ResourceQuotas. |
| `cluster-admin` | Access all cluster resources and non-resource URLs. |

To grant permissions to someone else, you must already hold those permissions or have `bind` permission on the target role. To add a rule to a role that grants permissions you do not hold, you need `escalate`. Use the local administrator token to create or remove users and to issue tokens.

```sh
sw user list
sw user token alice
sw user remove alice
```

The server returns a new token only once, when it issues it. Removing a user revokes the token and removes the bindings marked by the user management API. The namespace and its data remain.

`sw user token alice --out ./alice.kubeconfig` rotates the token and writes a new kubeconfig. After rotation, the next request using the old token receives HTTP 401. The `--cluster-role NAME` flag adds a separate cluster-wide binding. For an agent that works in only one namespace, use `--namespace` and `--role`.

### Tenant workloads

New tenant namespaces get the label `pod-security.kubernetes.io/enforce=baseline`. On create and update, the server checks Pods and the Pod templates in Deployments and ReplicaSets. For example, `baseline` rejects a request to create a Deployment with a `hostPath` volume.

The `restricted` level also checks that each container disallows privilege escalation, declares that it runs as non-root, has a seccomp profile, and meets the capability restrictions. These checks validate the specification in the API request. The [compatibility table](../compatibility.md) (in Korean) lists the security settings that the runtime only stores.

As an administrator, you can set limits with `kubectl create quota team-limit --hard=pods=10,requests.cpu=4 -n team-a`. When you create a Pod in that namespace, specify its CPU request so the quota can account for it. If a request exceeds the quota, the server returns HTTP 403 with an `exceeded quota` message showing the requested amount, current usage, and limit. Run `kubectl get quota team-limit -n team-a -o yaml` to see the totals in `status.used` and the limits in `status.hard`.

In a namespace with a ResourceQuota, the ReplicaSet controller also runs admission before creating each Pod. These Pods count toward namespace usage. If creating a Pod would exceed a `pods`, CPU, or memory quota, the controller records a `FailedCreate` event on the ReplicaSet and leaves the Pod uncreated.
