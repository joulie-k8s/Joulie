"""Unit tests for experiment 01 (CPU-only benchmark) sweep logic.

These tests exercise the pure-Python trace-manipulation functions without
requiring a Kubernetes cluster.  All kubectl / subprocess calls are mocked.
"""

import importlib
import json
import pathlib
import sys
import types
from unittest.mock import MagicMock, patch

import pytest

# ---------------------------------------------------------------------------
# Import the sweep module via importlib (filename starts with a digit).
# ---------------------------------------------------------------------------
SCRIPTS_DIR = pathlib.Path(__file__).resolve().parent
if str(SCRIPTS_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_DIR))

_spec = importlib.util.spec_from_file_location("sweep_01", SCRIPTS_DIR / "05_sweep.py")
sweep = importlib.util.module_from_spec(_spec)
# Patch subprocess.run globally before executing the module body so that
# module-level code never hits a real shell.
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
    affinity=None,
    cpu_units=100.0,
    gpu_units=0.0,
    workload_type="cpu_preprocess",
):
    """Return a single trace job record as a dict."""
    requests = {"cpu": cpu, "memory": memory}
    if gpu is not None:
        requests["nvidia.com/gpu"] = gpu
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
    """Build a nodeAffinity with a joulie.io/power-profile expression."""
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
    """Write records as JSONL and return the path."""
    p = tmp_path / "trace.jsonl"
    p.write_text("\n".join(json.dumps(r, separators=(",", ":")) for r in records) + "\n")
    return p


def _fake_kwok_nodes():
    """Return kubectl JSON output for a small CPU-only KWOK cluster."""
    return json.dumps(
        {
            "items": [
                {
                    "metadata": {
                        "name": f"kwok-cpu-{i}",
                        "labels": {
                            "type": "kwok",
                            "joulie.io/hw.cpu-model": model,
                        },
                    }
                }
                for i, model in enumerate(["xeon-8280", "xeon-8280", "epyc-7763"])
            ]
        }
    )


# ---------------------------------------------------------------------------
# Trace retargeting tests
# ---------------------------------------------------------------------------

class TestRetarget:
    """Tests for retarget_trace_for_cluster."""

    def _retarget(self, tmp_path, records):
        trace_path = _write_trace(tmp_path, records)
        fake_result = MagicMock()
        fake_result.stdout = _fake_kwok_nodes()
        with patch.object(sweep, "run", return_value=fake_result):
            return sweep.retarget_trace_for_cluster(trace_path)

    def _load_jobs(self, path):
        return [json.loads(l) for l in path.read_text().splitlines() if l.strip()]

    def test_retarget_adds_hw_kind_affinity(self, tmp_path):
        records = [_make_job(), _make_job(intent_class="performance")]
        out = self._retarget(tmp_path, records)
        for rec in self._load_jobs(out):
            if rec.get("type", "job") != "job":
                continue
            aff = rec["podTemplate"]["affinity"]
            exprs = aff["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"][
                "nodeSelectorTerms"
            ][0]["matchExpressions"]
            keys = [e["key"] for e in exprs]
            assert "joulie.io/hw.kind" in keys

    def test_retarget_never_adds_node_name(self, tmp_path):
        records = [_make_job(), _make_job(intent_class="performance")]
        out = self._retarget(tmp_path, records)
        raw = out.read_text()
        assert "nodeName" not in raw
        for rec in self._load_jobs(out):
            if rec.get("type", "job") != "job":
                continue
            aff = rec.get("podTemplate", {}).get("affinity", {})
            # No expression should reference a concrete node name key
            for term in (
                aff.get("nodeAffinity", {})
                .get("requiredDuringSchedulingIgnoredDuringExecution", {})
                .get("nodeSelectorTerms", [])
            ):
                for expr in term.get("matchExpressions", []):
                    assert expr.get("key") != "kubernetes.io/hostname"

    def test_retarget_preserves_intent_class(self, tmp_path):
        records = [_make_job(intent_class="performance"), _make_job(intent_class="standard")]
        out = self._retarget(tmp_path, records)
        jobs = self._load_jobs(out)
        assert jobs[0]["intentClass"] == "performance"
        assert jobs[1]["intentClass"] == "standard"

    def test_retarget_preserves_workload_class(self, tmp_path):
        records = [_make_job(workload_class="cpu_analytics")]
        out = self._retarget(tmp_path, records)
        jobs = self._load_jobs(out)
        assert jobs[0]["workloadClass"] == "cpu_analytics"

    def test_retarget_preserves_work_units(self, tmp_path):
        records = [_make_job(cpu_units=42.5, gpu_units=7.0)]
        out = self._retarget(tmp_path, records)
        jobs = self._load_jobs(out)
        assert jobs[0]["work"]["cpuUnits"] == 42.5
        assert jobs[0]["work"]["gpuUnits"] == 7.0

    def test_retarget_preserves_pod_template_requests(self, tmp_path):
        records = [_make_job(cpu="2", memory="1Gi")]
        out = self._retarget(tmp_path, records)
        jobs = self._load_jobs(out)
        req = jobs[0]["podTemplate"]["requests"]
        assert req["cpu"] == "2"
        assert req["memory"] == "1Gi"

    def test_retarget_passes_through_metadata_records(self, tmp_path):
        records = [_make_metadata_record(), _make_job()]
        out = self._retarget(tmp_path, records)
        jobs = self._load_jobs(out)
        assert jobs[0]["type"] == "metadata"
        assert jobs[0].get("info") == "test"


# ---------------------------------------------------------------------------
# Baseline derivation tests
# ---------------------------------------------------------------------------

class TestDeriveBaseline:

    def _targeted_trace(self, tmp_path, records):
        """Write a _targeted.jsonl trace (simulating the output of retarget)."""
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
        # power-profile should be gone
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
            # Remove previously generated output so derive_baseline_trace regenerates
            for f in tmp_path.glob("*_baseline_*.jsonl"):
                f.unlink()
            out = sweep.derive_baseline_trace(bl, trace, strip_affinity_for_a=True)
            jobs = self._load(out)
            assert jobs[0]["intentClass"] == "performance", f"baseline {bl} lost intentClass"

    def test_baseline_a_with_strip_disabled(self, tmp_path):
        """When strip_affinity_for_a is False, baseline A keeps power-profile."""
        records = [_make_job(affinity=_power_profile_affinity("eco"))]
        trace = self._targeted_trace(tmp_path, records)
        out = sweep.derive_baseline_trace("A", trace, strip_affinity_for_a=False)
        jobs = self._load(out)
        raw = out.read_text()
        assert "joulie.io/power-profile" in raw


# ---------------------------------------------------------------------------
# Negative tests
# ---------------------------------------------------------------------------

class TestNegativeConstraints:

    def _retarget(self, tmp_path, records):
        trace_path = _write_trace(tmp_path, records)
        fake_result = MagicMock()
        fake_result.stdout = _fake_kwok_nodes()
        with patch.object(sweep, "run", return_value=fake_result):
            return sweep.retarget_trace_for_cluster(trace_path)

    def _load(self, path):
        return [json.loads(l) for l in path.read_text().splitlines() if l.strip()]

    def test_no_node_name_in_any_baseline(self, tmp_path):
        records = [_make_job(affinity=_combined_affinity())]
        targeted = self._retarget(tmp_path, records)
        for bl in ("A", "B", "C"):
            # Clean up previous baselines
            for f in tmp_path.glob("*_baseline_*.jsonl"):
                f.unlink()
            out = sweep.derive_baseline_trace(bl, targeted, strip_affinity_for_a=True)
            raw = out.read_text()
            assert "nodeName" not in raw, f"baseline {bl} contains nodeName"

    def test_no_stale_class_names(self, tmp_path):
        """intentClass must never be 'general' or 'eco' (those are stale names)."""
        records = [
            _make_job(intent_class="performance"),
            _make_job(intent_class="standard"),
        ]
        targeted = self._retarget(tmp_path, records)
        for rec in self._load(targeted):
            ic = rec.get("intentClass", "")
            assert ic not in ("general", "eco"), f"stale intentClass={ic}"

    def test_no_concrete_node_references(self, tmp_path):
        records = [_make_job()]
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
        assert len(hw_exprs) == 1, "add_required_expr should be idempotent for same key"

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

    def test_build_family_first_pool(self):
        nodes = [
            {"name": "n1", "cpu_model": "xeon"},
            {"name": "n2", "cpu_model": "epyc"},
            {"name": "n3", "cpu_model": "xeon"},
        ]
        pool = sweep.build_family_first_pool(nodes, "cpu_model")
        names = [n["name"] for n in pool]
        # First member of each family comes first (sorted alphabetically)
        assert names[0] == "n2"  # epyc sorts before xeon
        assert names[1] == "n1"  # xeon representative
        assert names[2] == "n3"  # remaining xeon

    def test_rotate_pick(self):
        from collections import deque
        pool = deque([{"name": "a"}, {"name": "b"}, {"name": "c"}])
        first = sweep.rotate_pick(pool)
        assert first["name"] == "a"
        second = sweep.rotate_pick(pool)
        assert second["name"] == "b"
        third = sweep.rotate_pick(pool)
        assert third["name"] == "c"
        fourth = sweep.rotate_pick(pool)
        assert fourth["name"] == "a"  # wraps around
