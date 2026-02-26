# Example: Operator Configuration

This example shows how to configure the central operator policy loop without changing code.

## What can be configured

Operator reads these env vars:

- `RECONCILE_INTERVAL` (default `1m`)
- `NODE_SELECTOR` (default in manifest: `joulie.io/managed=true`)
- `RESERVED_LABEL_KEY` (default `joulie.io/reserved`)
- `PERFORMANCE_CAP_WATTS` (default `5000`)
- `ECO_CAP_WATTS` (default `120`)

## 1) Label managed and reserved nodes

```bash
kubectl label node <node-a> joulie.io/managed=true --overwrite
kubectl label node <node-b> joulie.io/managed=true --overwrite

# optional: exclude a node from optimization
kubectl label node <node-c> joulie.io/reserved=true --overwrite
```

## 2) Apply a custom operator config

Use the provided patch file:

```bash
kubectl -n joulie-system patch deployment joulie-operator --type merge \
  --patch-file examples/operator-configuration/operator-env-patch.yaml
kubectl -n joulie-system rollout status deployment/joulie-operator
```

## 3) Verify behavior

```bash
kubectl -n joulie-system logs deploy/joulie-operator --tail=200
kubectl get nodepowerprofiles -o wide
```

You should see periodic assignment logs and one `NodePowerProfile` per eligible (managed, non-reserved) node.

## 4) Quick inline override (alternative)

```bash
kubectl -n joulie-system set env deploy/joulie-operator \
  RECONCILE_INTERVAL=30s \
  NODE_SELECTOR='joulie.io/managed=true' \
  RESERVED_LABEL_KEY='joulie.io/reserved' \
  PERFORMANCE_CAP_WATTS=4500 \
  ECO_CAP_WATTS=180
```

## Notes

- `NODE_SELECTOR` controls which nodes the operator manages. It does **not** control DaemonSet placement.
- Agent placement is configured in the DaemonSet spec (`deploy/joulie.yaml`).
