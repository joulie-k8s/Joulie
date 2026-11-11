---
title: "Alerts and Runbooks"
weight: 70
---

The Joulie chart ships four alerting rules, each with a runbook on this page. They catch the failures that leave a cluster looking managed while it is not: a cap that did not apply, an agent that cannot write its status, a digital twin nobody updates, and nodes the measured power source returns nothing for. The runbooks are written so that an operator, or an SRE agent, can follow them step by step: what the alert means, how to confirm it, the likely causes and the fix.

## Installing the rules

The rules live in `charts/joulie/alerts/joulie-alerts.yaml`, in the Prometheus rule file format. With the Prometheus Operator, turn on the chart's PrometheusRule:

```bash
helm upgrade --install joulie oci://registry.cern.ch/joulie/joulie \
  --version <version> -n joulie-system \
  -f values/joulie.yaml \
  --set prometheusRule.enabled=true \
  --set prometheusRule.labels.release=kube-prometheus-stack
```

| Value | Default | Effect |
|-------|---------|--------|
| `prometheusRule.enabled` | `false` | Render the PrometheusRule `<release>-joulie-alerts`. It needs the PrometheusRule CRD of the Prometheus Operator. |
| `prometheusRule.namespace` | `""` (the release namespace) | Namespace of the PrometheusRule, one your Prometheus `ruleNamespaceSelector` selects. |
| `prometheusRule.labels` | `{}` | Labels added to the PrometheusRule, those your Prometheus `ruleSelector` selects (kube-prometheus-stack selects `release: <its release name>` by default). |

A Prometheus without the operator loads the same file with `rule_files`. Either way, Prometheus must scrape the agent and the controller manager (their Services carry the `prometheus.io/*` annotations; see [Metrics]({{< relref "/docs/architecture/metrics.md" >}})).

The rules read only Joulie's own metrics. When a component is not scraped at all its series disappear and its alerts cannot fire, so pair them with your usual alert on a scrape target that is down (`up == 0`).

The commands below assume the release namespace `joulie-system`. `<node>` is the node label of the alert. The `NodeTwin` and `NodeHardware` of a node are named after it, lowercased, with every character other than a letter, a digit or a hyphen turned into a hyphen (`pkg/api.ObjectNameForNode`).

## JoulieCapNotApplied

**Meaning.** The agent reports that the CPU or GPU cap the controller manager planned for a node is not in force: result `error` (the write or the inventory before it failed) or `blocked` (the node offers no way to apply it) in `status.controlStatus` of the node's `NodeTwin`. The node draws more than its profile plans, and the twin counts a GPU cap that does not apply as no cap.

**Expression.** `max by (node, component, result) (joulie_agent_cap_result{result=~"error|blocked"}) == 1` for 15 minutes. The agent reconciles every 20 seconds (`RECONCILE_INTERVAL`), so a write that fails once and works on the next reconcile never fires; 15 minutes of the same result is about 45 reconciles, never a transient.

**Confirm.**

```bash
kubectl get nodetwin <node> -o jsonpath='{.status.controlStatus}{"\n"}'
```

The `cpu` or `gpu` entry names the backend, the result and the reason, for example:

```text
{"cpu":{"backend":"none","message":"RAPL unavailable and DVFS disabled","result":"blocked","updatedAt":"..."}, ...}
```

The agent of the node logs the same reason:

```bash
kubectl -n joulie-system logs $(kubectl -n joulie-system get pod -l app.kubernetes.io/name=joulie-agent \
  --field-selector spec.nodeName=<node> -o name) | grep -E 'warning|failed' | tail -20
```

**Likely causes.**

- `blocked`, CPU: the node has no RAPL power limit files (a virtual machine, a CPU or kernel without the `intel-rapl` or `amd-rapl` powercap driver) and no DVFS fallback (no writable cpufreq under `/host-sys`, no HTTP control backend). `kubectl get nodehardware <node> -o jsonpath='{.status.cpu.controlAvailable}'` prints `false`.
- `blocked`, GPU: the GPUs report no settable power limit (NVIDIA boards that print `[N/A]` for `power.limit`, Intel GPUs without `power1_rated_max`). The message names the GPUs; `status.gpu.controlAvailable` of the `NodeHardware` is `false`.
- `error`: the write itself failed: a RAPL limit file that refuses the value, `nvidia-smi -pl` refused, an amdgpu or i915 limit that does not read back, or an HTTP control backend that answered `ok: false`. The message holds the error.

**Fix.**

- For `blocked` on hardware that cannot be capped, decide whether the node should be managed at all: take it out of `NODE_SELECTOR` (label `joulie.io/managed`), or keep it managed and silence the alert for that node. On a GPU node whose GPUs cannot be capped, `CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES=true` caps its CPU in eco instead ([Power cap configuration]({{< relref "/docs/getting-started/05-configuration-reference.md#power-cap-configuration" >}})).
- For a CPU without RAPL where cpufreq is available, check that the agent mounts the host's `/sys` (`agent.privileged`, the `host-sys` mount) and that its log has no `DVFS controller disabled` line, which an invalid `DVFS_*` setting causes.
- For `error`, act on the message: a cap outside the range the hardware accepts (compare it with `capRange` and `capRangePerGpu` in the `NodeHardware`), a missing GPU tool in the agent image (`agent.runtimeClassName`, [Agent GPU tools]({{< relref "/docs/getting-started/05-configuration-reference.md#agent-gpu-tools" >}})), or a driver that refuses limits. The alert clears at the first reconcile that applies the cap.

## JoulieAgentWritesBlocked

**Meaning.** The API server refuses the agent's writes of its own status: the `NodeHardware` status, or `status.controlStatus` of the `NodeTwin`. The caps may still be applied, but what the cluster shows is out of date: the twin and `kubectl joulie` read an old control result, and after 15 minutes without a `NodeHardware` write the controller manager marks the node `AgentReporting=False` and treats its caps as not enforced.

**Expression.** `sum by (node, object) (increase(joulie_agent_status_write_errors_total[10m])) > 0` for 20 minutes. The agent skips a write when nothing changed and rewrites an unchanged status every 5 minutes, so a blocked agent fails at least once every 5 minutes and the 10 minute window always holds a failure. A single refused write, such as a conflict during an API server restart, keeps the expression true for at most 10 minutes, under the 20 minute `for`. The alert fires 20 to 25 minutes after the writes start failing: the counter of a node's first failure starts at 1, which `increase` cannot tell from a counter that was always there, so the window holds a failure from the second one on, 5 minutes later.

**Confirm.**

```bash
# when the agent last managed to write
kubectl get nodehardware <node> -o jsonpath='{.status.updatedAt}{"\n"}'
kubectl get nodetwin <node> -o jsonpath='{.status.controlStatus.cpu.updatedAt}{"\n"}'
# whether RBAC lets the agent write
kubectl auth can-i patch nodetwins.joulie.io --subresource=status --as=system:serviceaccount:joulie-system:joulie-agent
kubectl auth can-i update nodehardwares.joulie.io --subresource=status --as=system:serviceaccount:joulie-system:joulie-agent
```

A timestamp older than 6 minutes (the 5 minute rewrite plus a reconcile) while the agent pod runs, or `no` from `kubectl auth can-i`, confirms it. A refused `NodeHardware` write is also in the agent's log:

```text
warning: failed to upsert NodeHardware status for node=<node>: nodehardwares.joulie.io "<node>" is forbidden: ...
```

A refused `NodeTwin` write is not logged; the counter and the stale `updatedAt` are the evidence.

**Likely causes.**

- RBAC: the ClusterRole of the agent lost a verb on `nodetwins/status`, `nodehardwares` or `nodehardwares/status`, or the agent runs under another ServiceAccount than `joulie-agent`.
- An admission webhook or a policy engine (Kyverno, Gatekeeper) rejects or mutates the write; the error in the log names the webhook.
- CRDs older than the agent: the API server rejects a value the installed schema does not accept, such as an enum value it does not list. `helm upgrade` never updates CRDs ([Upgrading]({{< relref "/docs/getting-started/06-upgrading.md" >}})).
- The API server throttles or is unreachable from the node (network policy, an overloaded control plane).

**Fix.** Restore the verbs (re-render the chart's `rbac.yaml`), exempt the `joulie-agent` ServiceAccount or the `joulie.io` resources from the rejecting policy, or apply the CRDs of the running release with `kubectl apply -f charts/joulie/crds/`. The counter stops growing at the first accepted write; the alert clears 10 minutes later.

## JoulieTwinStale

**Meaning.** The controller manager has not written the `NodeTwin` status of a managed node for more than 10 minutes. The status (`schedulableClass`, the predicted scores, the measured power, `lastUpdated`) is frozen: the scheduler extender gives such a node a neutral score once `lastUpdated` is older than `TWIN_STALENESS_THRESHOLD` (5 minutes), and `kubectl joulie` shows old numbers.

**Expression.** `time() - max by (node) (joulie_twin_status_updated_timestamp_seconds) > 300` for 5 minutes. The controller manager reconciles every minute (`RECONCILE_INTERVAL`), so a healthy twin is at most a minute or two old; 300 seconds is the extender's default staleness threshold, and the 5 minute `for` lets a slow reconcile or a restart pass. A node planned but never written counts from the reconcile that first planned it, so a twin the controller manager has never managed to write fires too. If you raise `RECONCILE_INTERVAL` above 2 minutes, raise the 300 to more than twice the interval.

**Confirm.**

```bash
kubectl get nodetwin <node> -o jsonpath='{.status.lastUpdated}{"\n"}'
kubectl -n joulie-system logs deploy/joulie-controller-manager --since=15m | grep -E 'reconcile failed|reconcileNodeTwin|assigned profiles' | tail -20
```

A healthy controller manager logs `assigned profiles policy=...` once per reconcile. `warning: reconcileNodeTwin <node>: ...` names a refused status write of one node; `reconcile failed: ...` stops every write of that reconcile.

**Likely causes.**

- The status write is refused: RBAC on `nodetwins/status` for `joulie-controller-manager`, an admission policy, or CRDs older than the controller manager.
- The reconcile fails before the twins: a failed list of nodes or of `NodeHardware` (`reconcile failed: list node hardware: ...`), or a refused `NodeTwin` spec write, which ends the reconcile at that node.
- The reconcile loop is stuck or much slower than its interval: API server throttling (`KUBE_CLIENT_QPS`, `KUBE_CLIENT_BURST`), or a slow Prometheus answering the input queries of every reconcile.
- With leader election, the leader lost its Lease and no replica reconciles.

When the controller manager is down the series disappear, and this alert resolves instead of firing; the alert on its scrape target being down covers that case.

**Fix.** Act on the logged error: restore RBAC, exempt the `joulie-controller-manager` ServiceAccount from the rejecting policy, apply the current CRDs, or raise the client limits. Restart the controller manager when the loop is stuck (`kubectl -n joulie-system rollout restart deploy/joulie-controller-manager`) and keep its log of the stuck run for the bug report. The alert clears at the next written status.

## JoulieNodePowerNoData

**Meaning.** For some managed nodes the measured power source returned no reading in the last reconciles: `status.powerMeasurement.source` of their `NodeTwin` is `prometheus-no-data` (no positive reading for the node in the answer to the node power query), `http-error` (the HTTP telemetry endpoint failed) or `http-no-endpoint` (`NODE_POWER_SOURCE=http` without `NODE_POWER_HTTP_ENDPOINT`). Their twins count 0 W measured, so their power headroom and trend are wrong. The value of the alert is the number of nodes.

**Expression.** `max by (source) (joulie_twin_nodes_power_source{source=~"prometheus-no-data|http-error|http-no-endpoint"}) > 0` for 15 minutes. The default query takes a 5 minute `rate()` (`PROMETHEUS_RATE_WINDOW`), so a new node, or one whose exporter restarted, has no reading for up to one window plus a scrape or two; 15 minutes is three windows. `static` (nothing measured, by configuration) never fires.

**Confirm.**

```bash
kubectl get nodetwins -o custom-columns=NODE:.spec.nodeName,SOURCE:.status.powerMeasurement.source
kubectl -n joulie-system logs deploy/joulie-controller-manager | grep -E 'node_power|node power' | head -20
```

Then, in Prometheus, how each node power query ended:

```promql
sum by (result) (increase(joulie_twin_input_queries_total{input="node_power"}[15m]))
```

`error` means Prometheus failed the query or could not be reached; `no_data` that the query returned nothing usable; `success` with some nodes at `prometheus-no-data` that those nodes are missing from the answer. Run the query from the `input node_power: ...` startup line in Prometheus to see which nodes it returns.

**Likely causes.**

- Every node: a wrong `PROMETHEUS_ADDRESS`, a Prometheus without the OTLP receiver or without `k8s.node.name` promoted, a name translation that does not match `PROMETHEUS_NAME_TRANSLATION`, or a preset that does not match the exporter (Kepler 0.7 to 0.9 needs `NODE_POWER_PRESET=kepler-legacy`). The controller manager logs a one-time hint naming which.
- Some nodes: no power exporter runs there (a node without RAPL or GPU telemetry, a hypervisor guest), or its series carry another node label than `NODE_POWER_NODE_LABEL`.
- `http-error`: the endpoint is down or answers in another format than the OTLP JSON the `http` source reads.

**Fix.** Correct the address, the preset or the translation from the hint ([Measured node power]({{< relref "/docs/getting-started/05-configuration-reference.md#measured-node-power" >}}), [Prometheus settings]({{< relref "/docs/architecture/telemetry.md#prometheus-settings" >}})), deploy the exporter on the missing nodes, or, for nodes that cannot be measured, silence the alert for them. The alert clears at the first reconcile that reads every node.
