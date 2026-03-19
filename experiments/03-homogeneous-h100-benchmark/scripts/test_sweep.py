"""Unit tests for experiment 03 (homogeneous H100 benchmark) sweep logic.

These tests exercise the pure-Python trace-manipulation functions without
requiring a Kubernetes cluster.  All kubectl / subprocess calls are mocked.
"""

import importlib
import json
import pathlib
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

_spec = importlib.util.spec_from_file_location("sweep_03", SCRIPTS_DIR / "05_sweep.py")
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


def _fake_kwok_nodes_h100():
    """Return kubectl JSON for a homogeneous H100 cluster with a few CPU-only nodes."""
    cpu_nodes = [
        {
            "metadata": {
                "name": "kwok-cpu-0",
                "labels": {
                    "type": "kwok",
                    "joulie.io/hw.cpu-model": "xeon-8480",
                },
            },
            "status": {"allocatable": {"cpu": "96", "memory": "256Gi"}},
        },
    ]
    gpu_nodes = [
        {
            "metadata": {
                "name": f"kwok-gpu-h100-{i}",
                "labels": {
                    "type": "kwok",
                    "joulie.io/hw.cpu-model": "xeon-8480",
                    "joulie.io/gpu.product": "H100-SXM",
                    "joulie.io/hw.gpu-count": "8",
                },
            },
            "status": {"allocatable": {"cpu": "96", "memory": "1Ti", "nvidia.com/gpu": "8"}},
        }
        for i in range(3)
    ]
    return json.dumps({"items": cpu_nodes + gpu_nodes})


# ---------------------------------------------------------------------------
# Trace retargeting tests
# ---------------------------------------------------------------------------

class TestRetarget:

    def _retarget(self, tmp_path, records):
        trace_path = _write_trace(tmp_path, records)
        fake_result = MagicMock()
        fake_result.stdout = _fake_kwok_nodes_h100()
        with patch.object(sweep, "run", return_value=fake_result):
            return sweep.retarget_trace_for_cluster(trace_path)

    def _load(self, path):
        return [json.loads(l) for l in path.read_text().splitlines() if l.strip()]

    def test_retarget_adds_hw_kind_affinity_for_cpu_jobs(self, tmp_path):
        records = [_make_job()]
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

    def test_retarget_gpu_job_gets_nvidia_key_on_h100_cluster(self, tmp_path):
        """On a homogeneous H100 (NVIDIA) cluster, GPU jobs should get nvidia.com/gpu."""
        records = [_make_job(gpu="1", gpu_key="gpu", gpu_units=100.0)]
        out = self._retarget(tmp_path, records)
        jobs = self._load(out)
        req = jobs[0]["podTemplate"]["requests"]
        assert "gpu" not in req
        assert "nvidia.com/gpu" in req

    def test_retarget_passes_through_metadata_records(self, tmp_path):
        records = [_make_metadata_record(), _make_job()]
        out = self._retarget(tmp_path, records)
        jobs = self._load(out)
        assert jobs[0]["type"] == "metadata"

    def test_retarget_gpu_job_preserves_gpu_request_quantity(self, tmp_path):
        records = [_make_job(gpu="4", gpu_key="nvidia.com/gpu", gpu_units=400.0)]
        out = self._retarget(tmp_path, records)
        jobs = self._load(out)
        req = jobs[0]["podTemplate"]["requests"]
        assert req["nvidia.com/gpu"] == "4"


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

    def test_baseline_a_strips_draining_affinity(self, tmp_path):
        """Exp 03 strip_power_profile_affinity also removes joulie.io/draining."""
        aff = {
            "nodeAffinity": {
                "requiredDuringSchedulingIgnoredDuringExecution": {
                    "nodeSelectorTerms": [
                        {
                            "matchExpressions": [
                                {"key": "joulie.io/power-profile", "operator": "In", "values": ["eco"]},
                                {"key": "joulie.io/draining", "operator": "In", "values": ["false"]},
                                {"key": "joulie.io/hw.kind", "operator": "In", "values": ["cpu-only"]},
                            ]
                        }
                    ]
                }
            }
        }
        records = [_make_job(affinity=aff)]
        trace = self._targeted_trace(tmp_path, records)
        out = sweep.derive_baseline_trace("A", trace, strip_affinity_for_a=True)
        jobs = self._load(out)
        remaining_aff = jobs[0]["podTemplate"].get("affinity", {})
        for term in (
            remaining_aff.get("nodeAffinity", {})
            .get("requiredDuringSchedulingIgnoredDuringExecution", {})
            .get("nodeSelectorTerms", [])
        ):
            for expr in term.get("matchExpressions", []):
                assert expr["key"] not in ("joulie.io/power-profile", "joulie.io/draining")


# ---------------------------------------------------------------------------
# Negative tests
# ---------------------------------------------------------------------------

class TestNegativeConstraints:

    def _retarget(self, tmp_path, records):
        trace_path = _write_trace(tmp_path, records)
        fake_result = MagicMock()
        fake_result.stdout = _fake_kwok_nodes_h100()
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

    def test_strip_power_profile_affinity_removes_power_and_draining(self):
        aff = {
            "nodeAffinity": {
                "requiredDuringSchedulingIgnoredDuringExecution": {
                    "nodeSelectorTerms": [
                        {
                            "matchExpressions": [
                                {"key": "joulie.io/power-profile", "operator": "In", "values": ["eco"]},
                                {"key": "joulie.io/draining", "operator": "In", "values": ["false"]},
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
        assert "joulie.io/draining" not in keys
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

    def test_build_family_first_pool_homogeneous(self):
        """All nodes share the same gpu_product -- pool should contain all of them."""
        nodes = [
            {"name": f"h100-{i}", "gpu_product": "H100-SXM"}
            for i in range(4)
        ]
        pool = sweep.build_family_first_pool(nodes, "gpu_product")
        assert len(pool) == 4
        # First in pool is the family representative
        assert pool[0]["name"] == "h100-0"

    def test_rotate_pick(self):
        pool = deque([{"name": "a"}, {"name": "b"}, {"name": "c"}])
        assert sweep.rotate_pick(pool)["name"] == "a"
        assert sweep.rotate_pick(pool)["name"] == "b"
        assert sweep.rotate_pick(pool)["name"] == "c"
        assert sweep.rotate_pick(pool)["name"] == "a"  # wraps


# ---------------------------------------------------------------------------
# Config sanity tests — catch bad workload/cluster combos before cluster setup
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

    # -- GPU workload demand vs cluster capacity ------------------------------

    def test_gpu_ratio_nonzero_on_gpu_cluster(self):
        """A cluster with GPUs must have gpu_ratio > 0."""
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
        """Multi-pod workload types must not be enabled — K8s lacks gang scheduling.

        distributed_training, parameter_server_training, and hpo_experiment
        spawn multiple co-dependent pods.  Without gang scheduling, these can
        deadlock the scheduler when cluster resources are tight.
        """
        gang_types = {"distributed_training", "parameter_server_training",
                       "hpo_experiment"}
        for cfg_path, cfg in self._prod_configs():
            allowed = set(cfg.get("workload", {}).get("allowed_workload_types") or [])
            if not allowed:
                continue
            overlap = allowed & gang_types
            assert not overlap, (
                f"{cfg_path.name}: gang-scheduling-risky types {overlap} enabled. "
                f"K8s does not support gang scheduling — remove them to avoid deadlocks."
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
        """Mean inter-arrival must produce enough concurrent jobs for the cluster size."""
        for cfg_path, cfg in self._prod_configs():
            run = cfg.get("run", {})
            mia = float(run.get("mean_inter_arrival_sec", 1))
            time_scale = float(run.get("time_scale", 1))
            if mia <= 0:
                continue
            jobs_per_sim_hour = 3600 / (mia * time_scale)
            nodes = self._load_inventory(cfg)
            n_total = self._total_gpu_nodes(nodes) + self._total_cpu_nodes(nodes)
            min_rate = n_total / 10
            assert jobs_per_sim_hour >= min_rate, (
                f"{cfg_path.name}: arrival rate = {jobs_per_sim_hour:.0f} jobs/sim-hr "
                f"but cluster has {n_total} nodes (want ≥{min_rate:.0f})"
            )

    def test_hp_frac_in_reasonable_range(self):
        """High-performance fraction must be between 0.1 and 0.9."""
        for cfg_path, cfg in self._prod_configs():
            policy = cfg.get("policy", {})
            for key in ("static.hp_frac", "queue_aware.hp_base_frac"):
                parts = key.split(".")
                val = policy
                for p in parts:
                    val = val.get(p, {}) if isinstance(val, dict) else None
                if val is None or not isinstance(val, (int, float)):
                    continue
                assert 0.1 <= float(val) <= 0.9, (
                    f"{cfg_path.name}: {key}={val} outside [0.1, 0.9]"
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
        for cfg_path, cfg in self._prod_configs():
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
