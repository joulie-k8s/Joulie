"""Unit tests for experiment 02 (heterogeneous benchmark) sweep logic.

These tests exercise the pure-Python trace-manipulation functions without
requiring a Kubernetes cluster.  All kubectl / subprocess calls are mocked.
"""

import importlib
import json
import pathlib
import re
import subprocess
import sys
from collections import deque
from unittest.mock import MagicMock, patch

import pytest
import yaml

# ---------------------------------------------------------------------------
# Import the sweep module via importlib (filename starts with a digit).
# ---------------------------------------------------------------------------
SCRIPTS_DIR = pathlib.Path(__file__).resolve().parent
if str(SCRIPTS_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_DIR))

_spec = importlib.util.spec_from_file_location("sweep_02", SCRIPTS_DIR / "05_sweep.py")
sweep = importlib.util.module_from_spec(_spec)
with patch("subprocess.run", return_value=MagicMock(stdout="{}", returncode=0)):
    _spec.loader.exec_module(sweep)


# ---------------------------------------------------------------------------
# Helpers -- sample trace records
# ---------------------------------------------------------------------------

def _make_job(
    intent_class="standard",
    workload_class="cpu_preprocess",
    cpu="500m",
    memory="256Mi",
    gpu=None,
    gpu_key="nvidia.com/gpu",
    affinity=None,
    cpu_units=100.0,
    gpu_units=0.0,
    workload_type="cpu_preprocess",
):
    """Return a single trace job record as a dict."""
    requests = {"cpu": cpu, "memory": memory}
    if gpu is not None:
        requests[gpu_key] = gpu
    rec = {
        "type": "job",
        "intentClass": intent_class,
        "workloadClass": workload_class,
        "workloadType": workload_type,
        "work": {"cpuUnits": cpu_units, "gpuUnits": gpu_units},
        "podTemplate": {
            "requests": requests,
        },
    }
    if affinity is not None:
        rec["podTemplate"]["affinity"] = affinity
    return rec


def _make_metadata_record():
    return {"type": "metadata", "info": "test"}


def _power_profile_affinity(profile_value="eco"):
    return {
        "nodeAffinity": {
            "requiredDuringSchedulingIgnoredDuringExecution": {
                "nodeSelectorTerms": [
                    {
                        "matchExpressions": [
                            {
                                "key": "joulie.io/power-profile",
                                "operator": "In",
                                "values": [profile_value],
                            }
                        ]
                    }
                ]
            }
        }
    }


def _hw_kind_affinity(kind="cpu-only"):
    return {
        "nodeAffinity": {
            "requiredDuringSchedulingIgnoredDuringExecution": {
                "nodeSelectorTerms": [
                    {
                        "matchExpressions": [
                            {
                                "key": "joulie.io/hw.kind",
                                "operator": "In",
                                "values": [kind],
                            }
                        ]
                    }
                ]
            }
        }
    }


def _combined_affinity():
    """Affinity with both power-profile and hw.kind expressions."""
    return {
        "nodeAffinity": {
            "requiredDuringSchedulingIgnoredDuringExecution": {
                "nodeSelectorTerms": [
                    {
                        "matchExpressions": [
                            {
                                "key": "joulie.io/power-profile",
                                "operator": "In",
                                "values": ["eco"],
                            },
                            {
                                "key": "joulie.io/hw.kind",
                                "operator": "In",
                                "values": ["cpu-only"],
                            },
                        ]
                    }
                ]
            }
        }
    }


def _write_trace(tmp_path, records):
    p = tmp_path / "trace.jsonl"
    p.write_text("\n".join(json.dumps(r, separators=(",", ":")) for r in records) + "\n")
    return p


def _fake_kwok_nodes_heterogeneous():
    """Return kubectl JSON for a mixed CPU + GPU cluster."""
    items = [
        # CPU-only nodes
        {
            "metadata": {
                "name": "kwok-cpu-0",
                "labels": {
                    "type": "kwok",
                    "joulie.io/hw.cpu-model": "xeon-8280",
                },
            },
            "status": {"allocatable": {"cpu": "32", "memory": "128Gi"}},
        },
        {
            "metadata": {
                "name": "kwok-cpu-1",
                "labels": {
                    "type": "kwok",
                    "joulie.io/hw.cpu-model": "epyc-7763",
                },
            },
            "status": {"allocatable": {"cpu": "64", "memory": "256Gi"}},
        },
        # NVIDIA GPU node
        {
            "metadata": {
                "name": "kwok-gpu-nv-0",
                "labels": {
                    "type": "kwok",
                    "joulie.io/hw.cpu-model": "xeon-8380",
                    "joulie.io/gpu.product": "A100",
                    "joulie.io/hw.gpu-count": "4",
                },
            },
            "status": {"allocatable": {"cpu": "64", "memory": "512Gi", "nvidia.com/gpu": "4"}},
        },
        # AMD GPU node
        {
            "metadata": {
                "name": "kwok-gpu-amd-0",
                "labels": {
                    "type": "kwok",
                    "joulie.io/hw.cpu-model": "epyc-9534",
                    "joulie.io/gpu.product": "MI300X",
                    "joulie.io/hw.gpu-count": "8",
                    "feature.node.kubernetes.io/pci-1002.present": "true",
                },
            },
            "status": {"allocatable": {"cpu": "128", "memory": "1Ti", "amd.com/gpu": "8"}},
        },
    ]
    return json.dumps({"items": items})


# ---------------------------------------------------------------------------
# Trace retargeting tests
# ---------------------------------------------------------------------------

class TestRetarget:

    def _retarget(self, tmp_path, records):
        trace_path = _write_trace(tmp_path, records)
        fake_result = MagicMock()
        fake_result.stdout = _fake_kwok_nodes_heterogeneous()
        with patch.object(sweep, "run", return_value=fake_result):
            return sweep.retarget_trace_for_cluster(trace_path)

    def _load(self, path):
        return [json.loads(l) for l in path.read_text().splitlines() if l.strip()]

    def test_retarget_adds_hw_kind_affinity_for_cpu_jobs(self, tmp_path):
        records = [_make_job()]  # CPU-only job (no GPU request)
        out = self._retarget(tmp_path, records)
        jobs = self._load(out)
        aff = jobs[0]["podTemplate"]["affinity"]
        exprs = aff["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"][
            "nodeSelectorTerms"
        ][0]["matchExpressions"]
        keys = [e["key"] for e in exprs]
        assert "joulie.io/hw.kind" in keys

    def test_retarget_never_adds_node_name(self, tmp_path):
        records = [
            _make_job(),
            _make_job(gpu="1", gpu_units=50.0, workload_type="single_gpu_training"),
        ]
        out = self._retarget(tmp_path, records)
        raw = out.read_text()
        assert "nodeName" not in raw
        for rec in self._load(out):
            if rec.get("type", "job") != "job":
                continue
            aff = rec.get("podTemplate", {}).get("affinity") or {}
            for term in (
                aff.get("nodeAffinity", {})
                .get("requiredDuringSchedulingIgnoredDuringExecution", {})
                .get("nodeSelectorTerms", [])
            ):
                for expr in term.get("matchExpressions", []):
                    assert expr.get("key") != "kubernetes.io/hostname"

    def test_retarget_preserves_intent_class(self, tmp_path):
        records = [
            _make_job(intent_class="performance"),
            _make_job(intent_class="standard", gpu="2", gpu_units=200.0),
        ]
        out = self._retarget(tmp_path, records)
        jobs = self._load(out)
        assert jobs[0]["intentClass"] == "performance"
        assert jobs[1]["intentClass"] == "standard"

    def test_retarget_preserves_workload_class(self, tmp_path):
        records = [_make_job(workload_class="debug_eval")]
        out = self._retarget(tmp_path, records)
        jobs = self._load(out)
        assert jobs[0]["workloadClass"] == "debug_eval"

    def test_retarget_preserves_work_units(self, tmp_path):
        records = [_make_job(cpu_units=42.5, gpu_units=7.0)]
        out = self._retarget(tmp_path, records)
        jobs = self._load(out)
        assert jobs[0]["work"]["cpuUnits"] == 42.5
        assert jobs[0]["work"]["gpuUnits"] == 7.0

    def test_retarget_preserves_pod_template_requests(self, tmp_path):
        records = [_make_job(cpu="4", memory="8Gi")]
        out = self._retarget(tmp_path, records)
        jobs = self._load(out)
        req = jobs[0]["podTemplate"]["requests"]
        assert req["cpu"] == "4"
        assert req["memory"] == "8Gi"

    def test_retarget_gpu_job_gets_vendor_specific_key(self, tmp_path):
        """GPU jobs should have their generic 'gpu' key replaced with a vendor-specific one."""
        records = [_make_job(gpu="1", gpu_key="gpu", gpu_units=100.0)]
        out = self._retarget(tmp_path, records)
        jobs = self._load(out)
        req = jobs[0]["podTemplate"]["requests"]
        assert "gpu" not in req
        assert "nvidia.com/gpu" in req or "amd.com/gpu" in req

    def test_retarget_passes_through_metadata_records(self, tmp_path):
        records = [_make_metadata_record(), _make_job()]
        out = self._retarget(tmp_path, records)
        jobs = self._load(out)
        assert jobs[0]["type"] == "metadata"


# ---------------------------------------------------------------------------
# Baseline derivation tests
# ---------------------------------------------------------------------------

class TestDeriveBaseline:

    def _targeted_trace(self, tmp_path, records):
        p = tmp_path / "seed_1_jobs_10_canonical_targeted.jsonl"
        p.write_text("\n".join(json.dumps(r, separators=(",", ":")) for r in records) + "\n")
        return p

    def _load(self, path):
        return [json.loads(l) for l in path.read_text().splitlines() if l.strip()]

    def test_baseline_a_strips_power_profile_affinity(self, tmp_path):
        records = [_make_job(affinity=_combined_affinity())]
        trace = self._targeted_trace(tmp_path, records)
        out = sweep.derive_baseline_trace("A", trace, strip_affinity_for_a=True)
        jobs = self._load(out)
        aff = jobs[0]["podTemplate"].get("affinity", {})
        for term in (
            aff.get("nodeAffinity", {})
            .get("requiredDuringSchedulingIgnoredDuringExecution", {})
            .get("nodeSelectorTerms", [])
        ):
            for expr in term.get("matchExpressions", []):
                assert expr["key"] != "joulie.io/power-profile"

    def test_baseline_a_preserves_hw_affinity(self, tmp_path):
        records = [_make_job(affinity=_combined_affinity())]
        trace = self._targeted_trace(tmp_path, records)
        out = sweep.derive_baseline_trace("A", trace, strip_affinity_for_a=True)
        jobs = self._load(out)
        aff = jobs[0]["podTemplate"]["affinity"]
        exprs = aff["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"][
            "nodeSelectorTerms"
        ][0]["matchExpressions"]
        keys = [e["key"] for e in exprs]
        assert "joulie.io/hw.kind" in keys

    def test_baseline_b_preserves_all_affinity(self, tmp_path):
        records = [_make_job(affinity=_combined_affinity())]
        trace = self._targeted_trace(tmp_path, records)
        out = sweep.derive_baseline_trace("B", trace, strip_affinity_for_a=True)
        jobs = self._load(out)
        aff = jobs[0]["podTemplate"]["affinity"]
        exprs = aff["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"][
            "nodeSelectorTerms"
        ][0]["matchExpressions"]
        keys = [e["key"] for e in exprs]
        assert "joulie.io/power-profile" in keys
        assert "joulie.io/hw.kind" in keys

    def test_baseline_c_preserves_all_affinity(self, tmp_path):
        records = [_make_job(affinity=_combined_affinity())]
        trace = self._targeted_trace(tmp_path, records)
        out = sweep.derive_baseline_trace("C", trace, strip_affinity_for_a=True)
        jobs = self._load(out)
        aff = jobs[0]["podTemplate"]["affinity"]
        exprs = aff["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"][
            "nodeSelectorTerms"
        ][0]["matchExpressions"]
        keys = [e["key"] for e in exprs]
        assert "joulie.io/power-profile" in keys

    def test_baseline_preserves_intent_class(self, tmp_path):
        for bl in ("A", "B", "C"):
            records = [_make_job(intent_class="performance", affinity=_power_profile_affinity())]
            trace = self._targeted_trace(tmp_path, records)
            for f in tmp_path.glob("*_baseline_*.jsonl"):
                f.unlink()
            out = sweep.derive_baseline_trace(bl, trace, strip_affinity_for_a=True)
            jobs = self._load(out)
            assert jobs[0]["intentClass"] == "performance", f"baseline {bl} lost intentClass"

    def test_baseline_a_with_strip_disabled(self, tmp_path):
        records = [_make_job(affinity=_power_profile_affinity("eco"))]
        trace = self._targeted_trace(tmp_path, records)
        out = sweep.derive_baseline_trace("A", trace, strip_affinity_for_a=False)
        raw = out.read_text()
        assert "joulie.io/power-profile" in raw


# ---------------------------------------------------------------------------
# Negative tests
# ---------------------------------------------------------------------------

class TestNegativeConstraints:

    def _retarget(self, tmp_path, records):
        trace_path = _write_trace(tmp_path, records)
        fake_result = MagicMock()
        fake_result.stdout = _fake_kwok_nodes_heterogeneous()
        with patch.object(sweep, "run", return_value=fake_result):
            return sweep.retarget_trace_for_cluster(trace_path)

    def _load(self, path):
        return [json.loads(l) for l in path.read_text().splitlines() if l.strip()]

    def test_no_node_name_in_any_baseline(self, tmp_path):
        records = [_make_job(affinity=_combined_affinity())]
        targeted = self._retarget(tmp_path, records)
        for bl in ("A", "B", "C"):
            for f in tmp_path.glob("*_baseline_*.jsonl"):
                f.unlink()
            out = sweep.derive_baseline_trace(bl, targeted, strip_affinity_for_a=True)
            raw = out.read_text()
            assert "nodeName" not in raw, f"baseline {bl} contains nodeName"

    def test_no_stale_class_names(self, tmp_path):
        records = [
            _make_job(intent_class="performance"),
            _make_job(intent_class="standard"),
        ]
        targeted = self._retarget(tmp_path, records)
        for rec in self._load(targeted):
            ic = rec.get("intentClass", "")
            assert ic not in ("general", "eco"), f"stale intentClass={ic}"

    def test_no_concrete_node_references(self, tmp_path):
        records = [_make_job(), _make_job(gpu="1", gpu_units=50.0)]
        targeted = self._retarget(tmp_path, records)
        for bl in ("A", "B", "C"):
            for f in tmp_path.glob("*_baseline_*.jsonl"):
                f.unlink()
            out = sweep.derive_baseline_trace(bl, targeted, strip_affinity_for_a=True)
            raw = out.read_text()
            for prefix in ("kwok-node-", "k3s-", "worker-"):
                assert prefix not in raw, f"baseline {bl} contains concrete node ref '{prefix}'"


# ---------------------------------------------------------------------------
# Config parsing tests
# ---------------------------------------------------------------------------

class TestConfigParsing:

    def test_config_loads_valid_yaml(self):
        cfg_path = pathlib.Path(__file__).resolve().parent.parent / "configs" / "benchmark.yaml"
        if not cfg_path.exists():
            pytest.skip("benchmark.yaml not found")
        cfg = sweep.load_config(cfg_path)
        assert isinstance(cfg, dict)

    def test_config_has_required_fields(self):
        cfg_path = pathlib.Path(__file__).resolve().parent.parent / "configs" / "benchmark.yaml"
        if not cfg_path.exists():
            pytest.skip("benchmark.yaml not found")
        cfg = sweep.load_config(cfg_path)
        assert "run" in cfg
        assert "workload" in cfg
        assert "baselines" in cfg["run"]
        assert "seeds" in cfg["run"]
        assert "jobs" in cfg["run"]

    def test_get_cfg_nested(self):
        cfg = {"a": {"b": {"c": 42}}}
        assert sweep.get_cfg(cfg, "a", "b", "c") == 42
        assert sweep.get_cfg(cfg, "a", "x", default="nope") == "nope"

    def test_to_baselines_string(self):
        assert sweep.to_baselines("A,B") == ["A", "B"]
        assert sweep.to_baselines("a, c") == ["A", "C"]

    def test_to_baselines_list(self):
        assert sweep.to_baselines(["A", "B", "C"]) == ["A", "B", "C"]

    def test_to_baselines_none(self):
        assert sweep.to_baselines(None) == ["A", "B", "C"]

    def test_to_baselines_invalid(self):
        with pytest.raises(SystemExit):
            sweep.to_baselines("A,D")


# ---------------------------------------------------------------------------
# Helper function unit tests
# ---------------------------------------------------------------------------

class TestHelpers:

    def test_ensure_required_term_from_none(self):
        aff = sweep.ensure_required_term(None)
        terms = aff["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        assert len(terms) == 1

    def test_add_required_expr_idempotent(self):
        expr = {"key": "joulie.io/hw.kind", "operator": "In", "values": ["cpu-only"]}
        aff = sweep.add_required_expr(None, expr)
        aff = sweep.add_required_expr(aff, expr)
        terms = aff["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        hw_exprs = [e for e in terms[0]["matchExpressions"] if e["key"] == "joulie.io/hw.kind"]
        assert len(hw_exprs) == 1

    def test_strip_power_profile_affinity_removes_power_profile(self):
        aff = {
            "nodeAffinity": {
                "requiredDuringSchedulingIgnoredDuringExecution": {
                    "nodeSelectorTerms": [
                        {
                            "matchExpressions": [
                                {"key": "joulie.io/power-profile", "operator": "In", "values": ["eco"]},
                                {"key": "joulie.io/hw.kind", "operator": "In", "values": ["cpu-only"]},
                            ]
                        }
                    ]
                }
            }
        }
        result = sweep.strip_power_profile_affinity(aff)
        exprs = result["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"][
            "nodeSelectorTerms"
        ][0]["matchExpressions"]
        keys = [e["key"] for e in exprs]
        assert "joulie.io/power-profile" not in keys
        assert "joulie.io/hw.kind" in keys

    def test_strip_power_profile_affinity_returns_none_when_empty(self):
        aff = {
            "nodeAffinity": {
                "requiredDuringSchedulingIgnoredDuringExecution": {
                    "nodeSelectorTerms": [
                        {
                            "matchExpressions": [
                                {"key": "joulie.io/power-profile", "operator": "In", "values": ["eco"]},
                            ]
                        }
                    ]
                }
            }
        }
        result = sweep.strip_power_profile_affinity(aff)
        assert result is None

    def test_build_family_first_pool(self):
        nodes = [
            {"name": "n1", "gpu_product": "A100"},
            {"name": "n2", "gpu_product": "MI300X"},
            {"name": "n3", "gpu_product": "A100"},
        ]
        pool = sweep.build_family_first_pool(nodes, "gpu_product")
        names = [n["name"] for n in pool]
        assert names[0] == "n1"   # A100 sorts before MI300X
        assert names[1] == "n2"   # MI300X representative
        assert names[2] == "n3"   # remaining A100

    def test_rotate_pick(self):
        pool = deque([{"name": "a"}, {"name": "b"}, {"name": "c"}])
        assert sweep.rotate_pick(pool)["name"] == "a"
        assert sweep.rotate_pick(pool)["name"] == "b"
        assert sweep.rotate_pick(pool)["name"] == "c"
        assert sweep.rotate_pick(pool)["name"] == "a"  # wraps


# ---------------------------------------------------------------------------
# Config sanity tests: catch bad workload/cluster combos before cluster setup
# ---------------------------------------------------------------------------

class TestConfigSanity:
    """Validate benchmark configs against cluster inventory.

    These tests load every benchmark-*-prod.yaml (and debug) config, pair it
    with the referenced inventory, and check that the workload parameters will
    produce reasonable utilization and meaningful power-cap headroom.
    """

    CONFIGS_DIR = pathlib.Path(__file__).resolve().parent.parent / "configs"

    @staticmethod
    def _load_inventory(cfg: dict) -> list[dict]:
        inv_path = cfg.get("inventory", {}).get("source")
        if not inv_path:
            pytest.skip("no inventory.source in config")
        inv_path = pathlib.Path(inv_path)
        if not inv_path.is_absolute():
            inv_path = pathlib.Path(__file__).resolve().parents[3] / inv_path
        if not inv_path.exists():
            pytest.skip(f"inventory file not found: {inv_path}")
        data = yaml.safe_load(inv_path.read_text())
        return data.get("nodes", [])

    @staticmethod
    def _total_gpus(nodes: list[dict]) -> int:
        return sum(n.get("replicas", 1) * n.get("gpu_count", 0) for n in nodes)

    @staticmethod
    def _total_gpu_nodes(nodes: list[dict]) -> int:
        return sum(n.get("replicas", 1) for n in nodes if n.get("gpu_count", 0) > 0)

    @staticmethod
    def _total_cpu_nodes(nodes: list[dict]) -> int:
        return sum(n.get("replicas", 1) for n in nodes if n.get("gpu_count", 0) == 0)

    @staticmethod
    def _avg_gpus_per_node(nodes: list[dict]) -> float:
        gpu_nodes = [n for n in nodes if n.get("gpu_count", 0) > 0]
        if not gpu_nodes:
            return 0
        total = sum(n.get("replicas", 1) * n.get("gpu_count", 0) for n in gpu_nodes)
        count = sum(n.get("replicas", 1) for n in gpu_nodes)
        return total / count

    def _prod_configs(self):
        import yaml as _yaml
        for p in sorted(self.CONFIGS_DIR.glob("benchmark-*-prod.yaml")):
            yield p, _yaml.safe_load(p.read_text())
        for p in sorted(self.CONFIGS_DIR.glob("benchmark-*-debug.yaml")):
            yield p, _yaml.safe_load(p.read_text())

    def _prod_only_configs(self):
        """The prod configs, without the debug ones.

        benchmark-5k-debug.yaml is the input of the standalone runs
        (scripts/standalone_sweep.py, no kube-scheduler), so rules written for
        a Kubernetes run are checked on the prod configs only.
        """
        import yaml as _yaml
        for p in sorted(self.CONFIGS_DIR.glob("benchmark-*-prod.yaml")):
            yield p, _yaml.safe_load(p.read_text())

    # -- GPU workload demand vs cluster capacity ------------------------------

    def test_gpu_ratio_nonzero_on_gpu_cluster(self):
        """A heterogeneous cluster with GPUs must have gpu_ratio > 0."""
        for cfg_path, cfg in self._prod_configs():
            nodes = self._load_inventory(cfg)
            total_gpus = self._total_gpus(nodes)
            if total_gpus == 0:
                continue
            gpu_ratio = float(cfg.get("workload", {}).get("gpu_ratio", 0))
            assert gpu_ratio > 0, (
                f"{cfg_path.name}: gpu_ratio=0 but cluster has {total_gpus} GPUs"
            )

    def test_gpu_ratio_sufficient_for_cluster(self):
        """GPU ratio must be high enough that GPU nodes see meaningful work.

        Rule of thumb: with N_gpu GPU nodes and N_total total nodes, at least
        (N_gpu / N_total) × 0.5 of jobs should be GPU jobs to avoid most GPU
        nodes sitting idle.  We use a floor of 0.30 for any GPU cluster.
        """
        for cfg_path, cfg in self._prod_configs():
            nodes = self._load_inventory(cfg)
            n_gpu = self._total_gpu_nodes(nodes)
            n_total = n_gpu + self._total_cpu_nodes(nodes)
            if n_gpu == 0 or n_total == 0:
                continue
            gpu_ratio = float(cfg.get("workload", {}).get("gpu_ratio", 0))
            gpu_frac = n_gpu / n_total
            min_ratio = max(0.30, gpu_frac * 0.5)
            assert gpu_ratio >= min_ratio, (
                f"{cfg_path.name}: gpu_ratio={gpu_ratio} too low for cluster with "
                f"{n_gpu}/{n_total} GPU nodes (min {min_ratio:.2f})"
            )

    def test_allowed_types_include_gpu_workloads(self):
        """If cluster has GPUs, allowed_workload_types must include GPU types."""
        gpu_types = {"debug_eval", "single_gpu_training", "distributed_training",
                     "parameter_server_training", "hpo_experiment"}
        for cfg_path, cfg in self._prod_configs():
            nodes = self._load_inventory(cfg)
            if self._total_gpus(nodes) == 0:
                continue
            allowed = set(cfg.get("workload", {}).get("allowed_workload_types") or [])
            if not allowed:
                continue  # no filter = all types allowed
            overlap = allowed & gpu_types
            assert overlap, (
                f"{cfg_path.name}: no GPU workload types in allowed_workload_types "
                f"{sorted(allowed)}, but cluster has {self._total_gpus(nodes)} GPUs"
            )

    def test_no_gang_scheduling_workload_types(self):
        """Multi-pod workload types must not be enabled: K8s lacks gang scheduling.

        distributed_training, parameter_server_training, and hpo_experiment
        spawn multiple co-dependent pods.  Without gang scheduling, these can
        deadlock the scheduler when cluster resources are tight.
        """
        gang_types = {"distributed_training", "parameter_server_training",
                       "hpo_experiment"}
        for cfg_path, cfg in self._prod_only_configs():
            allowed = set(cfg.get("workload", {}).get("allowed_workload_types") or [])
            if not allowed:
                continue
            overlap = allowed & gang_types
            assert not overlap, (
                f"{cfg_path.name}: gang-scheduling-risky types {overlap} enabled. "
                f"K8s does not support gang scheduling; remove them to avoid deadlocks."
            )

    # -- Simulation coverage --------------------------------------------------

    def test_timeout_covers_diurnal_cycle(self):
        """Sim timeout must cover at least one full day/night cycle (24 sim-hours)."""
        for cfg_path, cfg in self._prod_configs():
            run = cfg.get("run", {})
            time_scale = float(run.get("time_scale", 1))
            timeout = float(run.get("timeout", 0))
            sim_hours = (timeout * time_scale) / 3600
            assert sim_hours >= 12, (
                f"{cfg_path.name}: sim coverage = {sim_hours:.1f} sim-hours "
                f"(timeout={timeout}s × time_scale={time_scale}), need ≥12 for "
                f"meaningful day/night cycle coverage"
            )

    def test_arrival_rate_produces_concurrent_load(self):
        """Mean inter-arrival must produce enough concurrent jobs for the cluster size.

        With N total nodes, we want at least N/10 jobs arriving per sim-hour
        to keep the cluster reasonably loaded.
        """
        for cfg_path, cfg in self._prod_configs():
            run = cfg.get("run", {})
            mia = float(run.get("mean_inter_arrival_sec", 1))
            time_scale = float(run.get("time_scale", 1))
            if mia <= 0:
                continue
            # Jobs per sim-hour = (1/mia) * time_scale * 3600 / time_scale = 3600/mia
            # Actually: jobs arrive at 1/mia per wall-sec, each wall-sec = time_scale sim-sec
            # So jobs per sim-hour = (1/mia) * 3600 / time_scale
            jobs_per_sim_hour = 3600 / (mia * time_scale)
            nodes = self._load_inventory(cfg)
            n_total = self._total_gpu_nodes(nodes) + self._total_cpu_nodes(nodes)
            min_rate = n_total / 10
            assert jobs_per_sim_hour >= min_rate, (
                f"{cfg_path.name}: arrival rate = {jobs_per_sim_hour:.0f} jobs/sim-hr "
                f"but cluster has {n_total} nodes (want ≥{min_rate:.0f})"
            )

    def test_hp_frac_in_reasonable_range(self):
        """High-performance fraction must be above 0 and at most 0.9.

        There is no fixed floor: the fraction is relative to cluster size, and
        at 5000 nodes hp_base_frac=0.05 is 250 performance nodes, a deliberate
        choice for the 5,000-node configs.
        """
        for cfg_path, cfg in self._prod_configs():
            policy = cfg.get("policy", {})
            for key in ("static.hp_frac", "queue_aware.hp_base_frac"):
                parts = key.split(".")
                val = policy
                for p in parts:
                    val = val.get(p, {}) if isinstance(val, dict) else None
                if val is None or not isinstance(val, (int, float)):
                    continue
                assert 0 < float(val) <= 0.9, (
                    f"{cfg_path.name}: {key}={val} outside (0, 0.9]"
                )

    def test_inventory_file_exists(self):
        """The referenced inventory file must exist."""
        for cfg_path, cfg in self._prod_configs():
            inv_path = cfg.get("inventory", {}).get("source")
            assert inv_path, f"{cfg_path.name}: no inventory.source"
            full_path = pathlib.Path(__file__).resolve().parents[3] / inv_path
            assert full_path.exists(), (
                f"{cfg_path.name}: inventory file not found: {inv_path}"
            )

    def test_kind_cluster_config_exists(self):
        """The referenced kind cluster config must exist."""
        for cfg_path, cfg in self._prod_configs():
            kind_path = cfg.get("install", {}).get("kind_cluster_config")
            assert kind_path, f"{cfg_path.name}: no install.kind_cluster_config"
            full_path = pathlib.Path(__file__).resolve().parents[3] / kind_path
            assert full_path.exists(), (
                f"{cfg_path.name}: kind config not found: {kind_path}"
            )

    def test_perf_eco_watts_meaningful_gap(self):
        """Performance watts should be at least 1.5× eco watts for meaningful savings."""
        for cfg_path, cfg in self._prod_configs():
            caps = cfg.get("policy", {}).get("caps", {})
            perf_w = float(caps.get("performance_watts", 0))
            eco_w = float(caps.get("eco_watts", 0))
            if perf_w == 0 or eco_w == 0:
                continue
            ratio = perf_w / eco_w
            assert ratio >= 1.5, (
                f"{cfg_path.name}: performance_watts/eco_watts = {ratio:.2f} "
                f"(need ≥1.5 for meaningful energy savings)"
            )

    # -- Power-cap headroom ---------------------------------------------------

    def test_eco_cap_above_idle_power(self):
        """Eco watts must be meaningfully above a basic floor (≥100W)."""
        for cfg_path, cfg in self._prod_configs():
            eco_w = float(cfg.get("policy", {}).get("caps", {}).get("eco_watts", 0))
            if eco_w == 0:
                continue
            assert eco_w >= 100, (
                f"{cfg_path.name}: eco_watts={eco_w}W is suspiciously low"
            )

    def test_perf_cap_above_eco_cap(self):
        """Performance cap must be strictly above eco cap."""
        for cfg_path, cfg in self._prod_configs():
            caps = cfg.get("policy", {}).get("caps", {})
            perf_w = float(caps.get("performance_watts", 0))
            eco_w = float(caps.get("eco_watts", 0))
            if perf_w == 0 or eco_w == 0:
                continue
            assert perf_w > eco_w, (
                f"{cfg_path.name}: performance_watts={perf_w}W ≤ eco_watts={eco_w}W"
            )

    def test_eco_pct_below_performance_pct(self):
        """Eco CPU/GPU pct must be below performance pct."""
        for cfg_path, cfg in self._prod_configs():
            caps = cfg.get("policy", {}).get("caps", {})
            for resource in ("cpu", "gpu"):
                perf = float(caps.get(f"{resource}_performance_pct_of_max", 100))
                eco = float(caps.get(f"{resource}_eco_pct_of_max", 100))
                if eco == perf == 100:
                    continue
                assert eco < perf, (
                    f"{cfg_path.name}: {resource}_eco_pct={eco} ≥ {resource}_perf_pct={perf}"
                )

    def test_gpu_eco_cap_above_min_cap(self):
        """GPU eco cap must be above the hardware minimum cap watts."""
        for cfg_path, cfg in self._prod_only_configs():
            nodes = self._load_inventory(cfg)
            caps = cfg.get("policy", {}).get("caps", {})
            eco_pct = float(caps.get("gpu_eco_pct_of_max", 100))
            for n in nodes:
                if n.get("gpu_count", 0) == 0:
                    continue
                max_cap = n.get("gpu_max_cap_watts", 0)
                min_cap = n.get("gpu_min_cap_watts", 0)
                if max_cap == 0 or min_cap == 0:
                    continue
                effective_eco = max_cap * eco_pct / 100
                assert effective_eco >= min_cap, (
                    f"{cfg_path.name}: GPU eco cap {effective_eco:.0f}W "
                    f"(= {max_cap}W × {eco_pct}%) < hardware min {min_cap}W "
                    f"for {n.get('node_name_prefix', '?')}"
                )


# ---------------------------------------------------------------------------
# Hardware catalog: the simulator reads the one embedded in it
# ---------------------------------------------------------------------------

REPO_ROOT = pathlib.Path(__file__).resolve().parents[3]


def _load_standalone_sweep():
    spec = importlib.util.spec_from_file_location(
        "standalone_sweep_02", REPO_ROOT / "scripts" / "standalone_sweep.py"
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class TestEmbeddedCatalog:
    """No catalog path reaches the simulator, so it reads its embedded catalog.

    The experiments used to generate a catalog from their inventory and mount
    it over the embedded one. It replaced that catalog whole and held only
    placeholder facts (every CPU 2.0/3.0 GHz and 300 W TDP).
    """

    def test_standalone_sweep_sets_no_catalog_path(self, tmp_path, monkeypatch):
        # A path left exported in the shell must not reach the simulator either.
        monkeypatch.setenv("SIM_HARDWARE_CATALOG_PATH", "/stale/hardware.generated.yaml")
        standalone = _load_standalone_sweep()
        cfg = yaml.safe_load((SCRIPTS_DIR.parent / "configs" / "benchmark.yaml").read_text()) or {}
        seen = {}

        def fake_run(cmd, env=None, **kwargs):
            seen["env"] = env
            return subprocess.CompletedProcess(cmd, 0, stdout="", stderr="")

        monkeypatch.setattr(standalone.subprocess, "run", fake_run)
        ok = standalone.run_standalone(
            simulator_bin="joulie-simulator",
            baseline="B",
            trace_path=tmp_path / "trace.jsonl",
            output_dir=tmp_path / "out",
            inventory_path=str(SCRIPTS_DIR.parent / "configs" / "cluster-nodes.yaml"),
            cfg=cfg,
            time_scale=120.0,
        )
        assert ok
        assert seen["env"]["SIM_HARDWARE_CATALOG_PATH"] == ""

    def test_installer_clears_the_catalog_path(self):
        text = (SCRIPTS_DIR / "03_install_components.sh").read_text()
        # The chart's default path is the ConfigMap's file, and an empty file
        # there loads no catalog at all, so the path has to be cleared.
        assert '--set "env.SIM_HARDWARE_CATALOG_PATH="' in text
        assert '--from-literal=hardware.generated.yaml=""' in text

    def test_no_script_generates_or_passes_a_catalog(self):
        for path in sorted(SCRIPTS_DIR.iterdir()):
            if path.name == pathlib.Path(__file__).name or path.suffix not in {".sh", ".py"}:
                continue
            text = path.read_text()
            for needle in ("--out-catalog", "GENERATED_CATALOG"):
                assert needle not in text, f"{path.name} still uses {needle}"

    def test_out_catalog_flag_writes_nothing(self, tmp_path):
        catalog = tmp_path / "hardware.generated.yaml"
        result = subprocess.run(
            [
                sys.executable,
                str(REPO_ROOT / "scripts" / "generate_heterogeneous_assets.py"),
                "--input", str(SCRIPTS_DIR.parent / "configs" / "cluster-nodes.yaml"),
                "--out-nodes", str(tmp_path / "00-kwok-nodes.yaml"),
                "--out-classes", str(tmp_path / "10-node-classes.yaml"),
                "--out-catalog", str(catalog),
            ],
            capture_output=True,
            text=True,
        )
        assert result.returncode == 0, result.stderr
        assert (tmp_path / "00-kwok-nodes.yaml").exists()
        assert not catalog.exists()
        assert "--out-catalog is deprecated" in result.stderr


# ---------------------------------------------------------------------------
# 04_run_one.py: the simulator's debug artifacts
# ---------------------------------------------------------------------------

def _load_run_one():
    spec = importlib.util.spec_from_file_location("run_one_02", SCRIPTS_DIR / "04_run_one.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


class _FakePortForward:
    """A kubectl port-forward that prints the given stdout and stderr."""

    def __init__(self, out, err=""):
        self.out, self.err, self.args = out, err, None

    def __call__(self, args, **kwargs):
        import io
        self.args = args
        self.stdout = io.StringIO(self.out)
        return self

    def terminate(self):
        pass

    def wait(self, timeout=None):
        return 0

    def kill(self):
        pass

    def communicate(self, timeout=None):
        return self.out, self.err


def test_debug_fetch_uses_the_local_port_kubectl_picks(monkeypatch):
    """The port-forward asks kubectl for a free local port and reads it back:
    the fixed port 18080 failed whenever another process held it, and the
    run's energy then came back empty."""
    import io
    run_one = _load_run_one()
    pf = _FakePortForward("Forwarding from 127.0.0.1:41234 -> 18080\n")
    urls = []
    monkeypatch.setattr(run_one.subprocess, "Popen", pf)
    monkeypatch.setattr(run_one, "urlopen", lambda url, timeout: (urls.append(url), io.BytesIO(b'{"totalJoules": 1}'))[1])
    assert run_one.fetch_via_port_forward("/debug/energy", 5) == '{"totalJoules": 1}'
    assert pf.args[-1] == ":18080"
    assert urls == ["http://127.0.0.1:41234/debug/energy"]


def test_debug_fetch_reports_why_the_port_forward_failed(monkeypatch):
    run_one = _load_run_one()
    monkeypatch.setattr(run_one.subprocess, "Popen", _FakePortForward("", "error: unable to listen on any of the requested ports"))
    with pytest.raises(RuntimeError, match="unable to listen"):
        run_one.fetch_via_port_forward("/debug/energy", 5)


# ---------------------------------------------------------------------------
# Runs that complete: trace shaping, the simulator's settings, the end of a run
# ---------------------------------------------------------------------------

REPO_ROOT = SCRIPTS_DIR.parents[2]


def test_shape_trace_prefills_the_cluster_and_keeps_the_window(tmp_path):
    """A job submitted before prefill_sec is submitted at once, less the work
    it would have done since (cores, or max(0.1, GPUs), x base speed per
    second), or left out when it would have finished; later jobs keep their
    offset less prefill_sec, and jobs after prefill_sec + window are left
    out, with the workload records of every job left out."""
    def job(jid, offset, requests, work):
        return {"type": "job", "jobId": jid, "workloadId": jid, "submitTimeOffsetSec": offset,
                "podTemplate": {"requests": requests}, "work": work}
    recs = [
        {"type": "workload", "workloadId": "a", "submitTimeOffsetSec": 10.0},
        job("a", 10.0, {"cpu": "4"}, {"cpuUnits": 10000.0, "gpuUnits": 0.0}),
        {"type": "workload", "workloadId": "b", "submitTimeOffsetSec": 20.0},
        job("b", 20.0, {"cpu": "1"}, {"cpuUnits": 50.0, "gpuUnits": 0.0}),
        job("c", 150.0, {"cpu": "1"}, {"cpuUnits": 50.0, "gpuUnits": 0.0}),
        job("e", 50.0, {"cpu": "2", "nvidia.com/gpu": "2"}, {"cpuUnits": 10.0, "gpuUnits": 1000.0}),
        job("d", 2000.0, {"cpu": "1"}, {"cpuUnits": 5.0, "gpuUnits": 0.0}),
    ]
    p = tmp_path / "trace.jsonl"
    p.write_text("".join(json.dumps(r) + "\n" for r in recs))
    assert sweep.shape_trace(p, 900.0, 100.0, 2.0) == {"prefilled_jobs": 2, "dropped_jobs": 2}
    out = [json.loads(l) for l in p.read_text().splitlines()]
    assert [(r["type"], r["workloadId"], r["submitTimeOffsetSec"]) for r in out] == [
        ("workload", "a", 0.0), ("job", "a", 0.0), ("job", "c", 50.0), ("job", "e", 0.0)]
    work = {r["jobId"]: r["work"] for r in out if r["type"] == "job"}
    assert work["a"] == {"cpuUnits": 10000.0 - 90 * 2 * 4, "gpuUnits": 0.0}
    assert work["c"] == {"cpuUnits": 50.0, "gpuUnits": 0.0}
    assert work["e"] == {"cpuUnits": 0.0, "gpuUnits": 1000.0 - 50 * 2 * 2}


def test_install_sets_only_simulator_variables_the_simulator_reads():
    """Every env.SIM_* the install script gives the simulator is read by the
    simulator. The ambient model was set as SIM_FACILITY_AMBIENT_BASE_C,
    _AMPLITUDE_C and _PERIOD_SEC, names the simulator never read, so every run
    used its defaults. The stall force-completion is off: it ends a job that
    made no progress, a pending one included, without its work."""
    script = (SCRIPTS_DIR / "03_install_components.sh").read_text()
    names = set(re.findall(r'(?:extraEnv|env)\.(SIM_[A-Z0-9_]+)=', script))
    sources = "".join(p.read_text() for d in ("simulator/cmd/simulator", "simulator/pkg/workload")
                      for p in (REPO_ROOT / d).glob("*.go") if not p.name.endswith("_test.go"))
    assert names, "no simulator variables found"
    assert sorted(n for n in names if f'"{n}"' not in sources) == []
    assert '"env.SIM_WORKLOAD_STALL_FORCE_COMPLETE=-1"' in script


def test_run_window_stops_at_the_window_and_records_work_done(monkeypatch, tmp_path):
    """A run lasts the arrival window and stops before the drain; every poll
    writes the CPU and GPU work done so far, total minus remaining over all
    jobs, and the jobs submitted and completed."""
    run_one = _load_run_one()
    clock = iter([0.0, 0.0, 0.0, 10.0, 10.0, 20.0, 20.0, 20.0])
    monkeypatch.setattr(run_one.time, "time", lambda: next(clock))
    monkeypatch.setattr(run_one.time, "sleep", lambda s: None)
    payload = {"jobs": [
        {"submitted": True, "completed": True, "cpuUnitsTotal": 10, "cpuUnitsRemaining": 0, "gpuUnitsTotal": 4, "gpuUnitsRemaining": 0},
        {"submitted": True, "completed": False, "node": "n1", "cpuUnitsTotal": 10, "cpuUnitsRemaining": 6, "gpuUnitsTotal": 0, "gpuUnitsRemaining": 0},
        {"jobId": "j3", "submitted": True, "completed": False, "node": "", "cpuUnitsTotal": 3, "cpuUnitsRemaining": 3},
        {"submitted": False, "completed": False, "cpuUnitsTotal": 5, "cpuUnitsRemaining": 5},
    ]}
    answers = {"/debug/jobs": json.dumps(payload), "/debug/energy": json.dumps({"totalJoules": 1234.5, "byNodeJoules": {"n1": 1000.0}})}
    monkeypatch.setattr(run_one, "fetch_via_port_forward", lambda endpoint, timeout: answers[endpoint])
    monkeypatch.setattr(run_one, "host_load", lambda: 1.0)
    out = tmp_path / "work_progress.csv"
    assert run_one.run_window(20, 1000, out, {"j3": ("amd", 0.0, True, 8.0)}) == 3
    lines = out.read_text().splitlines()
    assert lines[0] == "elapsed_sec,cpu_units_done,gpu_units_done,cpu_only_units_done,jobs_submitted,jobs_completed,jobs_pending,jobs_queued,pending_nvidia,pending_amd,pending_cpu,pending_perf,pending_8gpu,energy_j,gpu_demand"
    assert lines[1:] == ["0.0,14.000,4.000,4.000,3,1,1,0,0,1,0,1,1,1234.5,0.000", "10.0,14.000,4.000,4.000,3,1,1,0,0,1,0,1,1,1234.5,0.000", "20.0,14.000,4.000,4.000,3,1,1,0,0,1,0,1,1,1234.5,0.000"]
    by_node = [json.loads(l) for l in (tmp_path / "energy_by_node.jsonl").read_text().splitlines()]
    assert by_node[-1] == {"elapsed_sec": 20.0, "byNodeJoules": {"n1": 1000.0}}


def test_benchmark_config_runs_the_arrival_window():
    """A run lasts the arrival window (run.timeout), the report measures from
    run.measure_from_sec inside it, and the sweep runs at least two seeds."""
    cfg = yaml.safe_load((SCRIPTS_DIR.parent / "configs" / "benchmark-40n.yaml").read_text())
    w, run = cfg["workload"], cfg["run"]
    assert run["timeout"] == w["arrival_window_sec"]
    assert 0 < run["measure_from_sec"] < run["timeout"]
    assert run["seeds"] >= 2


def test_run_schedule_interleaves_the_baselines_within_each_seed():
    """Each seed runs every baseline once, and over two seeds each baseline
    holds the same mean position, so no baseline runs as a block or always
    first or last."""
    sched = sweep.run_schedule(["A", "B", "C"], 2)
    assert sched == [(1, "A"), (1, "B"), (1, "C"), (2, "C"), (2, "B"), (2, "A")]
    for seed in (1, 2, 3, 4):
        assert sorted(b for s, b in sweep.run_schedule(["A", "B", "C"], 4) if s == seed) == ["A", "B", "C"]
    assert sweep.run_schedule(["A", "B", "C"], 2, repeat_first=True)[-1] == (1, "A")


def test_shape_trace_raises_gpu_utilization_up_to_one(tmp_path):
    """gpu_util_scale multiplies each job's GPU utilization, never above 1,
    and leaves CPU utilization and jobs without a GPU figure alone."""
    recs = [
        {"type": "job", "jobId": "a", "submitTimeOffsetSec": 1.0, "podTemplate": {"requests": {"cpu": "1"}},
         "work": {"cpuUnits": 1.0, "gpuUnits": 1.0}, "workloadProfile": {"cpuUtilization": 0.5, "gpuUtilization": 0.5}},
        {"type": "job", "jobId": "b", "submitTimeOffsetSec": 2.0, "podTemplate": {"requests": {"cpu": "1"}},
         "work": {"cpuUnits": 1.0, "gpuUnits": 1.0}, "workloadProfile": {"gpuUtilization": 0.9}},
        {"type": "job", "jobId": "c", "submitTimeOffsetSec": 3.0, "podTemplate": {"requests": {"cpu": "1"}},
         "work": {"cpuUnits": 1.0, "gpuUnits": 0.0}, "workloadProfile": {"cpuUtilization": 0.4}},
    ]
    p = tmp_path / "trace.jsonl"
    p.write_text("".join(json.dumps(r) + "\n" for r in recs))
    sweep.shape_trace(p, 0.0, 0.0, 2.0, gpu_util_scale=1.25)
    prof = {r["jobId"]: r["workloadProfile"] for r in map(json.loads, p.read_text().splitlines())}
    assert prof == {"a": {"cpuUtilization": 0.5, "gpuUtilization": 0.625}, "b": {"gpuUtilization": 1.0}, "c": {"cpuUtilization": 0.4}}


def test_control_errors_counts_refused_commands(monkeypatch):
    """A run counts the control commands the simulator refused, from its
    joulie_sim_control_actions_total, and only those."""
    run_one = _load_run_one()
    metrics = "\n".join([
        '# TYPE joulie_sim_control_actions_total counter',
        'joulie_sim_control_actions_total{action="rapl.set_power_cap_watts",node="n1",result="error"} 3',
        'joulie_sim_control_actions_total{action="rapl.set_power_cap_watts",node="n2",result="applied"} 40',
        'joulie_sim_control_actions_total{action="gpu.set_power_cap_watts",node="n3",result="error"} 1',
    ])
    monkeypatch.setattr(run_one, "fetch_via_port_forward", lambda endpoint, timeout: metrics)
    assert run_one.control_errors() == 4


def test_pick_vendor_sends_a_job_where_gpus_are_free():
    """A GPU job goes to the pool whose requested GPUs, the job's included,
    are the lowest share of its GPUs, among pools that can host it; a job
    that has ended frees its GPUs."""
    pools = {"amd": {"gpus": 3, "largest": 3, "active": [], "load": 0.0},
             "nvidia": {"gpus": 8, "largest": 8, "active": [], "load": 0.0}}
    # Shares after the pick: nvidia 1/8 < amd 1/3, 2/8 < 1/3, then amd
    # 1/3 < nvidia 3/8, then nvidia 3/8 < amd 2/3.
    assert [sweep.pick_vendor(pools, 0, 1, 100) for _ in range(4)] == ["nvidia", "nvidia", "amd", "nvidia"]
    # No AMD node holds 4 GPUs.
    assert sweep.pick_vendor(pools, 50, 4, 1) == "nvidia"
    # Every job has ended: nvidia 3/8 < amd 3/3; without freeing, nvidia
    # would be at 10/8.
    assert sweep.pick_vendor(pools, 200, 3, 1) == "nvidia"
    with pytest.raises(SystemExit):
        sweep.pick_vendor(pools, 300, 16, 1)


def test_retarget_keeps_both_vendor_pools_at_the_same_load(tmp_path, monkeypatch):
    """Over the experiment's inventory and jobs of heavy-tailed length, the
    AMD pool (40 of 188 GPUs) and the NVIDIA pool carry about the same share
    of their GPUs over time, and AMD peaks no higher than NVIDIA."""
    import heapq, random
    inv = yaml.safe_load((SCRIPTS_DIR.parent / "configs" / "cluster-nodes.yaml").read_text())
    nodes = []
    for n in inv["nodes"]:
        for i in range(n.get("replicas", 1)):
            g = int(n.get("gpu_count", 0))
            vendor = "amd" if "amd" in str(n.get("vendor", "")).lower() else ("nvidia" if g else "none")
            nodes.append({"name": f"{n['node_name_prefix']}-{i}", "vendor": vendor, "gpu_product": n.get("product", ""),
                          "gpu_count": g, "cpu_model": n.get("cpu", ""), "cpu_only": g <= 0})
    monkeypatch.setattr(sweep, "load_kwok_nodes", lambda: nodes)
    cap = {v: sum(n["gpu_count"] for n in nodes if n["vendor"] == v) for v in ("amd", "nvidia")}
    rng = random.Random(7)
    jobs = []
    for i in range(3000):
        g = rng.choice([1, 1, 1, 1, 2, 4, 8])
        jobs.append({"type": "job", "jobId": f"j{i}", "workloadId": f"j{i}", "submitTimeOffsetSec": i * 0.5,
                     "podTemplate": {"requests": {"cpu": "1", "nvidia.com/gpu": str(g)}},
                     "work": {"cpuUnits": 0.0, "gpuUnits": g * rng.paretovariate(1.5) * 10}})
    p = tmp_path / "trace.jsonl"
    p.write_text("".join(json.dumps(r) + "\n" for r in jobs))
    out = [json.loads(l) for l in sweep.retarget_trace_for_cluster(p).read_text().splitlines()]
    ev = []
    for r in out:
        req = r["podTemplate"]["requests"]
        v = "amd" if "amd.com/gpu" in req else "nvidia"
        g = float(req[f"{v}.com/gpu"])
        ev.append((r["submitTimeOffsetSec"], r["submitTimeOffsetSec"] + r["work"]["gpuUnits"] / g, v, g))
    load = {v: [sum(g for s0, e, vv, g in ev if vv == v and s0 <= t < e) / cap[v] for t in range(300, 1500, 10)] for v in cap}
    gap = sum(abs(a - b) for a, b in zip(load["amd"], load["nvidia"])) / len(load["amd"])
    # The pick in proportion to GPU counts gave a mean gap of 0.26 and an
    # AMD peak of 1.40 against 1.03 on NVIDIA.
    assert gap < 0.1, gap
    assert max(load["amd"]) <= max(load["nvidia"]) + 0.1, (max(load["amd"]), max(load["nvidia"]))


def _write_debug_state(path, nodes, jobs=(), before=None):
    """The simulator's state at the end of a run in path: nodes maps a node
    to its state fields (sim_debug_nodes.json); jobs lists (node, completed)
    as read after the nodes (sim_debug_jobs.json) and before, the same
    jobs as read before them (sim_debug_jobs_before.json), jobs by default."""
    path.joinpath("sim_debug_nodes.json").write_text(json.dumps({"nodes": [{"node": n, "state": st} for n, st in nodes.items()]}))
    for name, js in (("sim_debug_jobs.json", jobs), ("sim_debug_jobs_before.json", jobs if before is None else before)):
        path.joinpath(name).write_text(json.dumps({"jobs": [
            {"jobId": f"j{i}", "node": n, "submitted": True, "completed": c} for i, (n, c) in enumerate(js)]}))


def test_gate_failures_refuse_a_run_outside_the_gates(tmp_path):
    """A run fails the gates when the polls leave the measured window or have
    a gap, when jobs stay queued over the measured window on average, or
    when the binding resource's utilization at regime is below 45 %."""
    run_one = _load_run_one()
    head = "elapsed_sec,cpu_units_done,gpu_units_done,cpu_only_units_done,jobs_submitted,jobs_completed,jobs_pending,jobs_queued,pending_nvidia,pending_amd,pending_cpu,pending_perf,pending_8gpu,energy_j\n"
    (tmp_path / "trace.jsonl").write_text('{"type":"job","jobId":"a","work":{"cpuUnits":1,"gpuUnits":0}}\n')
    _write_debug_state(tmp_path, {"n1": {"gpuUtil": 0, "cpuUtil": 0.3}}, [("n1", False)])
    def write(polls, queued, util):
        (tmp_path / "work_progress.csv").write_text(head + "".join(f"{t},0,0,0,0,0,{queued},{queued},0,0,0,0,0,0\n" for t in polls))
        (tmp_path / "timeseries.csv").write_text("elapsed_sec,cluster_cpu_util,cluster_gpu_util,pods_running\n" + "".join(f"{t},{util},0,100\n" for t in range(0, 901, 10)))
    write(range(0, 901, 10), 0, 0.5)
    assert run_one.gate_failures(tmp_path, 120, 900) == []
    write(range(0, 801, 10), 0, 0.5)
    assert any("cover" in x for x in run_one.gate_failures(tmp_path, 120, 900))
    write([*range(0, 300, 10), *range(400, 901, 10)], 0, 0.5)
    assert any("gap" in x for x in run_one.gate_failures(tmp_path, 120, 900))
    write(range(0, 901, 10), 5, 0.5)
    assert any("queued" in x for x in run_one.gate_failures(tmp_path, 120, 900))
    # One poll at the start of the window does not decide: 5 queued at
    # 120 s only, none after.
    (tmp_path / "work_progress.csv").write_text(head + "".join(f"{t},0,0,0,0,0,{5 if t == 120 else 0},{5 if t == 120 else 0},0,0,0,0,0,0\n" for t in range(0, 901, 10)))
    assert run_one.gate_failures(tmp_path, 120, 900) == []
    write(range(0, 901, 10), 0, 0.4)
    assert any("util" in x for x in run_one.gate_failures(tmp_path, 120, 900))
    # B and C cause their own queueing and utilization: only coverage gates them.
    write(range(0, 901, 10), 5, 0.4)
    assert run_one.gate_failures(tmp_path, 120, 900, "B") == []
    write(range(0, 801, 10), 5, 0.4)
    assert any("cover" in x for x in run_one.gate_failures(tmp_path, 120, 900, "C"))


def test_gate_failures_refuse_a_node_left_with_load(tmp_path):
    """Every run fails when a node with no pod and no job ends at a GPU
    utilization above 0, and a GPU run under A when the simulator's GPU
    utilization at regime is more than 0.03 from the GPU load of its placed
    jobs. Two placed 8-GPU jobs at a GPU utilization of 0.5 on 16 GPUs: the
    polls' gpu_demand is 8, the placed jobs give 0.5."""
    run_one = _load_run_one()
    head = "elapsed_sec,cpu_units_done,gpu_units_done,cpu_only_units_done,jobs_submitted,jobs_completed,jobs_pending,jobs_queued,pending_nvidia,pending_amd,pending_cpu,pending_perf,pending_8gpu,energy_j,gpu_demand\n"
    (tmp_path / "work_progress.csv").write_text(head + "".join(f"{t},0,0,0,0,0,0,0,0,0,0,0,0,0,8.0\n" for t in range(0, 901, 10)))
    (tmp_path / "trace.jsonl").write_text("".join(
        json.dumps({"type": "job", "jobId": j, "submitTimeOffsetSec": 0, "podTemplate": {"requests": {"nvidia.com/gpu": "8"}},
                    "workloadProfile": {"gpuUtilization": 0.5}, "work": {"cpuUnits": 1, "gpuUnits": 1}}, separators=(",", ":")) + "\n" for j in ("g1", "g2")))
    (tmp_path / "nodes.json").write_text(json.dumps({"items": [
        {"status": {"allocatable": {"nvidia.com/gpu": "8"}}}, {"status": {"allocatable": {"nvidia.com/gpu": "8"}}}, {"status": {"allocatable": {"cpu": "4"}}}]}))
    def series(util):
        (tmp_path / "timeseries.csv").write_text("timestamp_utc,elapsed_sec,cluster_cpu_util,cluster_gpu_util,pods_running\n" + "".join(
            f"2026-10-27T00:{t // 60:02d}:{t % 60:02d}Z,{t},0.1,{util},2\n" for t in range(0, 901, 10)))
    series(0.5)
    busy = {"gpuUtil": 0.5, "cpuUtil": 0.1}
    idle = {"gpuUtil": 0, "cpuUtil": 0, "otherPods": 0}
    _write_debug_state(tmp_path, {"n1": busy, "n2": busy, "n3": idle}, [("n1", False), ("n2", False)])
    assert run_one.gate_failures(tmp_path, 120, 900) == []
    # A node whose last job ended before both reads kept its GPU load, or
    # its CPU load with no pod outside the workload.
    _write_debug_state(tmp_path, {"n1": busy, "n2": {"gpuUtil": 0.98, "cpuUtil": 0}}, [("n1", False), ("n2", True)])
    assert any("no job at a load" in x for x in run_one.gate_failures(tmp_path, 120, 900))
    assert any("no job at a load" in x for x in run_one.gate_failures(tmp_path, 120, 900, "B"))
    _write_debug_state(tmp_path, {"n1": busy, "n2": {"gpuUtil": 0, "cpuUtil": 0.24, "otherPods": 0}}, [("n1", False), ("n2", True)])
    assert any("n2 GPU 0.00 CPU 0.24" in x for x in run_one.gate_failures(tmp_path, 120, 900, "B"))
    # A pod outside the workload loads the CPU of a node without jobs.
    _write_debug_state(tmp_path, {"n1": busy, "n2": {"gpuUtil": 0, "cpuUtil": 0.12, "otherPods": 1}}, [("n1", False)])
    assert run_one.gate_failures(tmp_path, 120, 900, "B") == []
    # The job on n2 ended between the two reads of the jobs: the node read
    # may show its load.
    _write_debug_state(tmp_path, {"n1": busy, "n2": busy}, [("n1", False), ("n2", True)], before=[("n1", False), ("n2", False)])
    assert run_one.gate_failures(tmp_path, 120, 900) == []
    # A job placed on n2 and ended between the two reads.
    _write_debug_state(tmp_path, {"n1": busy, "n2": busy}, [("n1", False), ("n2", True)], before=[("n1", False), ("", False)])
    assert run_one.gate_failures(tmp_path, 120, 900) == []
    # The simulator reads 0.6 where the requests give 0.5.
    series(0.6)
    assert any("placed jobs give 0.500" in x for x in run_one.gate_failures(tmp_path, 120, 900))
    assert run_one.gate_failures(tmp_path, 120, 900, "C") == []
    # What the checks read is missing or empty: a failure, never a crash
    # or a pass that looked at nothing.
    (tmp_path / "timeseries.csv").write_text("timestamp_utc,elapsed_sec,cluster_cpu_util,cluster_gpu_util,pods_running\n")
    assert any("< 0.45" in x for x in run_one.gate_failures(tmp_path, 120, 900))
    series(0.5)
    (tmp_path / "work_progress.csv").write_text(head + "".join(f"{t},0,0,0,0,0,0,0,0,0,0,0,0,0,nan\n" for t in range(0, 901, 10)))
    assert any("no utilization to check" in x for x in run_one.gate_failures(tmp_path, 120, 900))
    (tmp_path / "work_progress.csv").write_text(head + "".join(f"{t},0,0,0,0,0,0,0,0,0,0,0,0,0,8.0\n" for t in range(0, 901, 10)))
    series(0.5)
    (tmp_path / "nodes.json").unlink()
    assert any("no utilization to check" in x for x in run_one.gate_failures(tmp_path, 120, 900))
    (tmp_path / "sim_debug_jobs_before.json").unlink()
    assert any("cannot be read" in x for x in run_one.gate_failures(tmp_path, 120, 900, "B"))
    (tmp_path / "sim_debug_nodes.json").write_text("{}")
    (tmp_path / "sim_debug_jobs.json").write_text("{}")
    (tmp_path / "sim_debug_jobs_before.json").write_text("{}")
    assert any("lists no node" in x for x in run_one.gate_failures(tmp_path, 120, 900, "B"))


def test_placed_gpu_demand_counts_the_jobs_the_simulator_counts(tmp_path):
    """The polls sum GPUs x utilization target over the jobs on a node with
    GPU work left, as the simulator's workload tick does: not a waiting job,
    a job past its GPU work, or a completed one. A placed job without a
    target makes the sum NaN, and the trace's targets are clamped to 1."""
    run_one = _load_run_one()
    jobs = [
        {"jobId": "a", "node": "n1", "completed": False, "requestedGpus": 4, "gpuUnitsRemaining": 5},
        {"jobId": "b", "node": "n1", "completed": False, "requestedGpus": 8, "gpuUnitsRemaining": 0},
        {"jobId": "c", "node": "", "completed": False, "requestedGpus": 8, "gpuUnitsRemaining": 9},
        {"jobId": "d", "node": "n2", "completed": True, "requestedGpus": 2, "gpuUnitsRemaining": 1},
    ]
    targets = {"a": 0.5, "b": 1.0, "c": 1.0, "d": 1.0}
    assert run_one.placed_gpu_demand(jobs, targets) == 2.0
    del targets["a"]
    demand = run_one.placed_gpu_demand(jobs, targets)
    assert demand != demand
    p = tmp_path / "trace.jsonl"
    p.write_text("\n".join(json.dumps(r, separators=(",", ":")) for r in [
        {"type": "job", "jobId": "a", "podTemplate": {"requests": {"nvidia.com/gpu": "4"}}, "workloadProfile": {"gpuUtilization": 1.3}},
        {"type": "job", "jobId": "b", "podTemplate": {"requests": {"amd.com/gpu": "1"}}},
        {"type": "job", "jobId": "c", "podTemplate": {"requests": {"cpu": "2"}}, "workloadProfile": {"cpuUtilization": 0.4}},
    ]))
    assert run_one.gpu_util_targets(p) == {"a": 1.0, "b": None}


def test_job_pools_reads_pool_offset_and_class(tmp_path):
    """job_pools gives each job its GPU resource or cpu, its submit offset
    and whether it is a performance job, from the trace's intentClass."""
    run_one = _load_run_one()
    p = tmp_path / "trace.jsonl"
    p.write_text(
        '{"type":"job","jobId":"a","submitTimeOffsetSec":5,"intentClass":"performance","podTemplate":{"requests":{"cpu":"2","amd.com/gpu":"1"}}}\n'
        '{"type":"job","jobId":"b","submitTimeOffsetSec":0,"intentClass":"standard","podTemplate":{"requests":{"cpu":"2"}}}\n'
        '{"type":"meta","jobId":"m"}\n')
    assert run_one.job_pools(p) == {"a": ("amd", 5.0, True, 1.0), "b": ("cpu", 0.0, False, 0.0)}


def test_scheduler_bin_packs_gpus():
    """The cluster's scheduler configuration bin-packs the GPU resources
    (NodeResourcesFit MostAllocated), so whole nodes stay free for 8-GPU
    jobs; with the default LeastAllocated the 1-GPU jobs spread over the
    8-GPU nodes and every 8-GPU job queued (02, seed 1, baseline A)."""
    text = (SCRIPTS_DIR / "01_create_cluster_kwokctl.sh").read_text()
    cfg = yaml.safe_load(text.split("<<SCHED_CFG\n", 1)[1].split("\nSCHED_CFG", 1)[0].replace("${EXTENDER_URL_PREFIX}", "http://x"))
    args = {p["name"]: p["args"] for p in cfg["profiles"][0]["pluginConfig"]}["NodeResourcesFit"]
    assert args["scoringStrategy"]["type"] == "MostAllocated"
    assert {r["name"] for r in args["scoringStrategy"]["resources"]} == {"nvidia.com/gpu", "amd.com/gpu"}


def test_load_failures_refuse_a_loaded_run():
    """A run whose mean 1-minute host load exceeds the host's CPUs fails."""
    run_one = _load_run_one()
    assert run_one.load_failures({"host_load1_mean": 28.0, "host_cpus": 28}) == []
    assert run_one.load_failures({"host_load1_mean": 30.0, "host_cpus": 28}) == ["mean host load 30.0 exceeds the 28 CPUs"]


def test_inventory_gpu_cap_range_is_the_catalogs():
    """Each GPU node pool's cap range is its catalog entry's
    (pkg/hwinv/assets/hardware.yaml, minBoardPowerW and maxBoardPowerW): the
    inventory sets the simulator's class keys, which override the catalog,
    so another value models a board the catalog does not describe. 02 set
    350 W minimums for the H100 SXM and the MI300X, against 300 W and 400 W,
    until 2026-10-27."""
    root = pathlib.Path(__file__).resolve().parents[3]
    catalog = yaml.safe_load((root / "pkg/hwinv/assets/hardware.yaml").read_text())["gpuModels"]
    by_alias = {a: m["official"] for m in catalog.values() for a in m.get("aliases") or []}
    nodes = yaml.safe_load((SCRIPTS_DIR.parent / "configs" / "cluster-nodes.yaml").read_text())["nodes"]
    checked = 0
    for n in nodes:
        if not n.get("gpu_count"):
            continue
        official = by_alias[n["product"]]
        assert (n["gpu_min_cap_watts"], n["gpu_max_cap_watts"]) == (official["minBoardPowerW"], official["maxBoardPowerW"]), n["node_name_prefix"]
        checked += 1
    assert checked



def test_install_gets_the_node_classes_generated_for_the_inventory():
    """The sweep generates the node classes of its inventory
    (00_generate_assets.sh), and the installer must load that file. It
    loaded the kwok-cluster example's classes instead, which hold the H100
    NVL alone, so in experiment 02 every other GPU family ran on the
    simulator defaults (computeGamma 1, idle 30 W)."""
    import re

    generate = (SCRIPTS_DIR / "00_generate_assets.sh").read_text()
    gen_dir = re.search(r'GENERATED_DIR="\$ROOT/([^"]+)"', generate).group(1)
    assert '--out-classes "$GENERATED_DIR/10-node-classes.yaml"' in generate
    install = (SCRIPTS_DIR / "03_install_components.sh").read_text()
    exp_root = re.search(r'EXP_ROOT="\$ROOT/([^"]+)"', install).group(1)
    default = re.search(r'GENERATED_CLASSES=\$\{GENERATED_CLASSES:-([^}]+)\}', install).group(1)
    assert default.replace("$EXP_ROOT", exp_root) == f"{gen_dir}/10-node-classes.yaml"
    assert 'node-classes.yaml "$GENERATED_CLASSES"' in install


def test_gate_refuses_gpu_nodes_off_their_node_class(tmp_path):
    """A GPU node whose model in the simulator differs from the class its
    labels match, or that matches no class, fails the run; the defaults the
    simulator gave every family but the H100 NVL are such a run."""
    run_one = _load_run_one()
    classes = tmp_path / "classes.yaml"
    classes.write_text(
        "classes:\n"
        "  - name: gpu-class-1\n    matchLabels: {joulie.io/gpu.product: NVIDIA-H100-NVL}\n"
        "    model: {gpu: {computeGamma: 0.5, memoryEpsilon: 0.01, idleWattsPerGpu: 58.6}}\n"
        "  - name: gpu-class-2\n    matchLabels: {joulie.io/gpu.product: NVIDIA-L40S}\n"
        "    model: {gpu: {computeGamma: 1.2, idleWattsPerGpu: 36.3}}\n")
    def state(nodes):
        (tmp_path / "nodes.json").write_text(json.dumps({"items": [
            {"metadata": {"name": n, "labels": {"joulie.io/gpu.product": p} if p else {}}} for n, (p, _) in nodes.items()]}))
        (tmp_path / "sim_debug_nodes.json").write_text(json.dumps({"nodes": [
            {"node": n, "model": {"GPU": g}} for n, (_, g) in nodes.items()]}))
    nvl = {"ComputeGamma": 0.5, "MemoryEpsilon": 0.01, "IdleWattsPerGPU": 58.6}
    state({"a": ("NVIDIA-H100-NVL", nvl), "b": ("NVIDIA-L40S", {"ComputeGamma": 1.2, "IdleWattsPerGPU": 36.3}), "c": (None, {})})
    assert run_one.class_failures(tmp_path, classes) == []
    state({"a": ("NVIDIA-H100-NVL", nvl), "b": ("NVIDIA-L40S", {"ComputeGamma": 1, "IdleWattsPerGPU": 30})})
    out = run_one.class_failures(tmp_path, classes)
    assert len(out) == 1 and "b (gpu-class-2)" in out[0] and "computeGamma 1 not 1.2" in out[0], out
    state({"a": ("NVIDIA-H100-NVL", nvl), "d": ("AMD-Radeon-PRO-W7900", {})})
    assert "d matches no GPU class" in run_one.class_failures(tmp_path, classes)[0]
    state({"c": (None, {})})
    assert run_one.class_failures(tmp_path, classes) == ["no GPU node to check against the node classes"]
    assert "cannot be read" in run_one.class_failures(tmp_path, tmp_path / "missing.yaml")[0]


def test_extender_reachability_is_retried_until_a_deadline(tmp_path):
    """The installer probes the scheduler extender from the control plane
    before a run. One probe right after the rollout failed once (02 baseline
    B, 2026-10-27) and ended the sweep; the probe is retried 12 times, 5 s apart,
    and a probe that never succeeds still fails the install."""
    import re
    import subprocess

    script = (SCRIPTS_DIR / "03_install_components.sh").read_text()
    start = script.index('  if docker inspect "$CTRL_CONTAINER" >/dev/null 2>&1; then\n')
    end = script.index('  else\n', start)
    block = script[start:end] + '  fi\n'
    stub = tmp_path / "docker"
    calls = tmp_path / "calls"
    def run(failures):
        # docker inspect finds the control plane; each docker exec is one probe.
        stub.write_text(f"#!/bin/bash\n[ \"$1\" = inspect ] && exit 0\necho x >> {calls}\n[ $(wc -l < {calls}) -gt {failures} ]\n")
        stub.chmod(0o755)
        calls.write_text("")
        body = 'set -e\nCTRL_CONTAINER=c EXTENDER_URL=u\nsleep() { :; }\n' + block + 'echo OK\n'
        return subprocess.run(["bash", "-c", body], env={"PATH": f"{tmp_path}:/usr/bin:/bin"}, capture_output=True, text=True)
    ok = run(2)
    assert ok.returncode == 0 and "OK" in ok.stdout and calls.read_text().count("x") == 3, (ok.stdout, ok.stderr)
    never = run(100)
    assert never.returncode == 1 and "FATAL" in never.stderr and calls.read_text().count("x") == 12, (never.stdout, never.stderr)

