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
