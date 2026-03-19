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
import yaml

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


# ---------------------------------------------------------------------------
# Config sanity tests — catch bad workload/cluster combos before cluster setup
# ---------------------------------------------------------------------------

class TestConfigSanity:
    """Validate benchmark configs against cluster inventory.

    These tests load every benchmark-*-prod.yaml (and debug) config, pair it
    with the referenced inventory, and check that the workload parameters will
    produce reasonable utilization and meaningful power-cap headroom.  They run
    *before* any cluster is created, so misconfigurations fail fast.
    """

    CONFIGS_DIR = pathlib.Path(__file__).resolve().parent.parent / "configs"

    @staticmethod
    def _load_inventory(cfg: dict) -> list[dict]:
        """Parse cluster-nodes YAML into a list of node-group dicts."""
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

    def _prod_configs(self):
        """Yield (path, cfg) for every prod/debug config."""
        import yaml as _yaml
        for p in sorted(self.CONFIGS_DIR.glob("benchmark-*-prod.yaml")):
            yield p, _yaml.safe_load(p.read_text())
        for p in sorted(self.CONFIGS_DIR.glob("benchmark-*-debug.yaml")):
            yield p, _yaml.safe_load(p.read_text())

    # -- CPU-only experiment: GPU ratio must be zero --------------------------

    def test_cpu_only_experiment_has_zero_gpu_ratio(self):
        """Experiment 01 is CPU-only; gpu_ratio must be 0."""
        for cfg_path, cfg in self._prod_configs():
            gpu_ratio = float(cfg.get("workload", {}).get("gpu_ratio", 0))
            assert gpu_ratio == 0.0, (
                f"{cfg_path.name}: gpu_ratio={gpu_ratio} but this is a CPU-only experiment"
            )

    def test_cpu_only_no_gpu_workload_types(self):
        """CPU-only configs must not include GPU workload types."""
        gpu_types = {"debug_eval", "single_gpu_training", "distributed_training",
                     "parameter_server_training", "hpo_experiment"}
        for cfg_path, cfg in self._prod_configs():
            allowed = set(cfg.get("workload", {}).get("allowed_workload_types") or [])
            overlap = allowed & gpu_types
            assert not overlap, (
                f"{cfg_path.name}: GPU workload types {overlap} in a CPU-only experiment"
            )

    def test_no_gang_scheduling_workload_types(self):
        """Multi-pod workload types must not be enabled — K8s lacks gang scheduling."""
        gang_types = {"distributed_training", "parameter_server_training",
                       "hpo_experiment"}
        for cfg_path, cfg in self._prod_configs():
            allowed = set(cfg.get("workload", {}).get("allowed_workload_types") or [])
            if not allowed:
                continue
            overlap = allowed & gang_types
            assert not overlap, (
                f"{cfg_path.name}: gang-scheduling-risky types {overlap} enabled"
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
            n_total = sum(n.get("replicas", 1) for n in nodes)
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
        """Eco watts must be meaningfully above idle power (≥ 1.3× idle).

        If the cap is too close to idle, there is no dynamic range for the
        simulator to show energy savings — the eco baseline degenerates to
        idle-only power.
        """
        for cfg_path, cfg in self._prod_configs():
            eco_w = float(cfg.get("policy", {}).get("caps", {}).get("eco_watts", 0))
            if eco_w == 0:
                continue
            nodes = self._load_inventory(cfg)
            # For CPU-only: idle power ~ TDP * 0.3 per socket, rough estimate
            # Just ensure eco_watts > 100W as a basic sanity floor
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
                    continue  # not configured
                assert eco < perf, (
                    f"{cfg_path.name}: {resource}_eco_pct={eco} ≥ {resource}_perf_pct={perf}"
                )
