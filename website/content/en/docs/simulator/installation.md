---
title: "Installation"
weight: 14
---

This page installs the simulator into a Kubernetes cluster you already have, or runs it from a workstation. To try Joulie with nothing but Docker, use the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) instead: it creates the cluster and installs everything.

The commands use the namespace `joulie-sim-demo` and the release `joulie-simulator`, the defaults of `make simulator-install`.

## Prerequisites

- A Kubernetes cluster (real, or [kind](https://kind.sigs.k8s.io/) for local work), with `kubectl` pointed at it.
- `helm` v3 or later.

## Install with Helm

```bash
helm upgrade --install joulie-simulator oci://registry.cern.ch/joulie/joulie-sim \
  --version <version> -n joulie-sim-demo --create-namespace
```

`helm show values oci://registry.cern.ch/joulie/joulie-sim --version <version>` prints the values you can override; `helm uninstall joulie-simulator -n joulie-sim-demo` removes it. The chart models the Nodes labelled `joulie.io/managed=true` (`SIM_NODE_SELECTOR`); [KWOK and Standalone Modes]({{< relref "/docs/simulator/simulator.md" >}}) says how to point Joulie at it.

## Install from source

```bash
make simulator-build-push TAG=<tag>
make simulator-install TAG=<tag>
```

`make simulator-install` installs `charts/joulie-simulator` and waits for the rollout. `kubectl apply -f simulator/deploy/simulator.yaml` installs the same simulator from plain manifests, with the image `latest`.

## Verify

```bash
kubectl -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim
kubectl -n joulie-sim-demo logs deploy/joulie-telemetry-sim --tail=50
kubectl -n joulie-sim-demo port-forward svc/joulie-telemetry-sim 18080:80 &
curl -s http://127.0.0.1:18080/healthz                # 200 when healthy
curl -s http://127.0.0.1:18080/debug/nodes | jq       # the Nodes it models
curl -s http://127.0.0.1:18080/metrics | head -30     # its Prometheus metrics
```

In the cluster the simulator's URL is `http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local` (Service `joulie-telemetry-sim`, port 80). The endpoints are under [HTTP API]({{< relref "/docs/simulator/simulator.md#http-api" >}}).

## Emulate mode in a cluster

[Emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}) needs a cluster spec and one Node to hold the emulated trees, labelled `joulie.io/hwemu-host=true`. The chart then runs one replica as root, strategy `Recreate`, with `emulate.root` as a hostPath:

```bash
kubectl label node <node> joulie.io/hwemu-host=true
helm upgrade --install joulie-simulator charts/joulie-simulator -n joulie-sim-demo --create-namespace \
  --set emulate.enabled=true \
  --set-file emulate.clusterSpec=tests/simcluster/specs/smoke.yaml \
  --set otel.metricsEndpoint=http://prometheus-operated.monitoring:9090/api/v1/otlp/v1/metrics
```

The pod is Ready once every tree exists, so agent pods that mount the trees (`agent.mode: emulated`, see [Agent chart values]({{< relref "/docs/getting-started/05-configuration-reference.md#agent-chart-values" >}})) can start after it. Apply the spec's Nodes with `clusterspec render-nodes` ([The cluster spec]({{< relref "/docs/simulator/emulate-mode.md#the-cluster-spec" >}})). The chart values are under [Simulator emulate mode]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-emulate-mode" >}}).

## Emulate mode from a workstation

The simulator also runs outside the cluster, against any cluster a kubeconfig reaches: the fastest loop while changing it. Use a throwaway cluster, since the Nodes it renders are fake and carry the KWOK taint.

```bash
export KUBECONFIG=<kubeconfig of a throwaway cluster>
go build -o /tmp/sim/bin/ ./simulator/cmd/simulator ./simulator/cmd/clusterspec
/tmp/sim/bin/clusterspec render-nodes -spec tests/simcluster/specs/smoke.yaml | kubectl apply -f -
SIM_EMULATE=true SIM_CLUSTER_SPEC=tests/simcluster/specs/smoke.yaml \
  SIM_HWEMU_ROOT=/tmp/sim/hwemu SIM_FAKESMI_SOURCE= SIM_DEBUG_PERSIST_DIR=/tmp/sim/run \
  SIM_EXPORTERS=kepler-0.7,dcgm-4.8.4 \
  /tmp/sim/bin/simulator
```

Outside a pod the simulator listens on `127.0.0.1:18080`, and its log names the API server in use. `SIM_FAKESMI_SOURCE=` skips the `fakesmi` install, which only agent pods need. Check it:

```bash
curl -s 127.0.0.1:18080/debug/emulate | jq -c '[.nodes[] | [.name, .host.powerW]]'
curl -s 127.0.0.1:18080/exporters/kepler-0.7/metrics | grep kepler_node_platform_joules_total
head -1 /tmp/sim/run/events.jsonl | jq -r '.resourceLogs[0].scopeLogs[0].logRecords[0].eventName'
```

Add `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://127.0.0.1:9090/api/v1/otlp/v1/metrics` to push to a local Prometheus. Restarting over the same `SIM_HWEMU_ROOT` resumes the trees.
