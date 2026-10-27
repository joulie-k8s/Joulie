"""Tests of report_figures.py on synthetic run directories laid out as
04_run_one.py writes them."""
import datetime as dt
import importlib.util
import json
import math
import pathlib
import sys

import pytest

pytest.importorskip("matplotlib")

HERE = pathlib.Path(__file__).parent
_spec = importlib.util.spec_from_file_location("report_figures", HERE / "report_figures.py")
rf = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(rf)

START = dt.datetime(2026, 10, 27, 1, 0, 0)
TS = ("timestamp_utc,elapsed_sec,it_power_w,cpu_power_w,gpu_power_w,pue,cooling_power_w,facility_power_w,"
      "ambient_temp_c,cluster_cpu_util,cluster_gpu_util,nodes_active,pods_running,energy_cumulative_j\n")
WK = ("elapsed_sec,cpu_units_done,gpu_units_done,cpu_only_units_done,jobs_submitted,jobs_completed,jobs_pending,"
      "jobs_queued,pending_nvidia,pending_amd,pending_cpu,pending_perf,pending_8gpu,energy_j\n")


def plan_line(sec, profiles):
    stamp = (START + dt.timedelta(seconds=sec)).strftime("%Y/%m/%d %H:%M:%S")
    plan = ",".join(f"n{i}={p}(cpu=100.0%)" for i, p in enumerate(profiles))
    return f"{stamp} controller: assigned profiles policy=queue_aware_v1 interval=2.5s nodes={len(profiles)} plan={plan}\n"


def make_run(results, baseline, seed, watts, log="", gate_failures=(), gpu_watts=0, stamp="20261027T010000Z"):
    """A run of constant power and pace: watts on CPU-only node c1 and, with
    gpu_watts, a GPU node g1; 10 work units per second of the binding kind
    and 5 CPU-only units."""
    d = results / f"{stamp}_b{baseline}_s{seed}"
    d.mkdir(parents=True)
    (d / "completion.json").write_text(json.dumps({"control_errors": 0, "gate_failures": list(gate_failures),
                                                   "host_load1_mean": 10.0, "host_load1_max": 12.0, "host_cpus": 28}))
    (d / "metadata.json").write_text(json.dumps({"timeScale": 120}))
    gpu = 1e9 if gpu_watts else 0
    (d / "trace.jsonl").write_text(f'{{"type":"job","jobId":"j","submitTimeOffsetSec":0,"work":{{"cpuUnits":1e9,"gpuUnits":{gpu}}}}}\n')
    (d / "sim_debug_nodes.json").write_text('[{"node": "c1", "model": {"GPU": {"Count": 0}}}, {"node": "g1", "model": {"GPU": {"Count": 8}}}]')
    it = watts + gpu_watts
    ts, wk, en = [TS], [WK], []
    for t in range(0, 901, 10):
        when = (START + dt.timedelta(seconds=t)).strftime("%Y-%m-%dT%H:%M:%SZ")
        ts.append(f"{when},{t},{it},{watts},{gpu_watts},1.2,100,{it + 100},22,0.5,{0.5 if gpu_watts else 0},2,100,{it * t}\n")
        wk.append(f"{t},{t * 10.0},{t * 10.0 if gpu_watts else 0},{t * 5.0},1,0,2,0,0,0,2,1,0,{it * t}\n")
        en.append(json.dumps({"elapsed_sec": t, "byNodeJoules": {"c1": watts * t, "g1": gpu_watts * t}}) + "\n")
    (d / "timeseries.csv").write_text("".join(ts))
    (d / "work_progress.csv").write_text("".join(wk))
    (d / "energy_by_node.jsonl").write_text("".join(en))
    if log:
        (d / "controller-manager.log").write_text(log)
    (d / "simulator.log").write_text("2026/10/27 01:00:05 [joulie-simulator] job completed id=j node=c1 class=standard elapsed=2.0s\n"
                                     + "2026/10/27 01:00:06 [joulie-simulator] job completed id=x node=g1 class=standard elapsed=2.0s\n")
    return d


def test_plans_and_their_held_mean(tmp_path):
    """Each logged plan gives its performance node count at its time on the
    run's clock, held until the next plan."""
    d = make_run(tmp_path, "C", 1, 1000, plan_line(100, ["performance", "eco", "performance"])
                 + "2026/10/27 01:03:00 controller: an unrelated line\n"
                 + plan_line(500, ["performance"] * 4))
    events = rf.plans(d)
    assert events == [(100.0, 2), (500.0, 4)]
    assert rf.held_mean(events, 120, 900) == pytest.approx((2 * 380 + 4 * 400) / 780)
    assert math.isnan(rf.held_mean(events, 50, 900))
    assert rf.plans(make_run(tmp_path, "A", 1, 1000)) == []


def test_report_runs_to_the_end(tmp_path, monkeypatch, capsys):
    """The report prints every table and draws every figure; the per-baseline
    table once failed on a text column."""
    results, out = tmp_path / "res", tmp_path / "img"
    for seed in (1, 2):
        make_run(results, "A", seed, 1000)
        make_run(results, "B", seed, 900, plan_line(0, ["performance", "eco", "eco"]))
        make_run(results, "C", seed, 800, plan_line(0, ["performance", "performance", "eco"]))
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("run:\n  measure_from_sec: 120\n  timeout: 900\n")
    monkeypatch.setattr(sys, "argv", ["report_figures.py", str(results), str(out), "x", str(cfg)])
    rf.main()
    text = capsys.readouterr().out
    summary = text.split("Per baseline, mean over seeds:")[1].split("Change against A")[0]
    rows = {line.split("|")[1].strip()[0]: [c.strip() for c in line.split("|")[2:-1]] for line in summary.splitlines() if line.startswith("| ") and "Baseline" not in line}
    assert rows["A"][6:8] == ["none", "1.0"]
    assert rows["B"][6] == "1.0" and rows["C"][6] == "2.0"
    assert "C (queue-aware) | -20.0 / -20.0" in text.split("Change against A")[1]
    # The backlog is in simulated units like the work: (1e9 submitted - 9,000
    # done at 900 s) x the time scale of 120.
    assert "119,998,920,000" in text.split("Per baseline")[0]
    assert sorted(p.name for p in out.glob("*.png")) == ["cumulative_energy.png", "energy_comparison.png", "facility_power_breakdown.png",
                                                          "pue_analysis.png", "timeseries.png", "utilization_summary.png"]


def test_report_refuses_a_run_that_failed_the_gates(tmp_path):
    make_run(tmp_path, "A", 1, 1000, gate_failures=["cluster_cpu_util at regime 0.400 < 0.45"])
    with pytest.raises(SystemExit, match="failed the gates"):
        rf.load(tmp_path)


def table(text, title):
    """The rows of the markdown table after title, as {first two cells: {column: cell}}."""
    lines = [l for l in text.split(title, 1)[1].split("\n\n", 2)[1].splitlines() if l.startswith("|")]
    head = [c.strip() for c in lines[0].split("|")[1:-1]]
    rows = {}
    for line in lines[2:]:
        cells = [c.strip() for c in line.split("|")[1:-1]]
        rows[" ".join(cells[:2])] = dict(zip(head, cells))
    return rows


def test_gpu_report_measures_energy_per_unit_on_each_kind_of_node(tmp_path, monkeypatch, capsys):
    """For a GPU cluster the decisive metric is GPU-node energy per GPU unit,
    beside CPU-only-node energy per CPU-only unit; a second run of A seed 1
    gives the run-to-run noise."""
    results, out = tmp_path / "res", tmp_path / "img"
    for seed in (1, 2):
        make_run(results, "A", seed, 100, gpu_watts=1000)
        make_run(results, "B", seed, 100, gpu_watts=900)
        make_run(results, "C", seed, 100, gpu_watts=800)
    make_run(results, "A", 1, 100, gpu_watts=1005, stamp="20261027T020000Z")
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("run:\n  measure_from_sec: 120\n  timeout: 900\n")
    monkeypatch.setattr(sys, "argv", ["report_figures.py", str(results), str(out), "x", str(cfg)])
    rf.main()
    text = capsys.readouterr().out
    runs = table(text, "per run (2 seeds):")
    assert runs["A (no mgmt) 1"]["J per GPU unit"] == "100.000"
    # Job x has no trace record, so no GPU request, and ran on the GPU node g1.
    assert runs["A (no mgmt) 1"]["Jobs finished / of them without GPUs on GPU nodes"] == "2 / 1"
    change = table(text, "Change against A, paired by seed")
    c = next(v for k, v in change.items() if k.startswith("C (queue-aware)"))
    assert c["GPU-node energy per GPU unit (%)"] == "-20.0 / -20.0"
    assert c["CPU-only-node energy per CPU-only unit (%)"] == "+0.0 / +0.0"
    noise = table(text, "Run-to-run noise")
    assert noise["A (no mgmt) 1"]["GPU-node energy per GPU unit (%)"] == "+0.5"
    assert noise["A (no mgmt) 1"]["Host load mean, first / second (28 CPUs)"] == "10.0 / 10.0"


def test_report_refuses_a_run_on_a_loaded_host(tmp_path):
    """A run whose mean host load exceeds the host's CPUs is run again."""
    d = make_run(tmp_path, "A", 1, 1000)
    done = json.loads((d / "completion.json").read_text())
    done["host_load1_mean"] = 30.0
    (d / "completion.json").write_text(json.dumps(done))
    with pytest.raises(SystemExit, match="exceeds the 28 CPUs"):
        rf.load(tmp_path)


def test_report_leaves_out_a_repeat_above_the_load_limit(tmp_path, monkeypatch, capsys):
    """A repeat only estimates noise: one that failed only the load gate is
    left out of the noise table with the reason, and the report runs."""
    results, out = tmp_path / "res", tmp_path / "img"
    for seed in (1, 2):
        make_run(results, "A", seed, 1000)
        make_run(results, "B", seed, 900)
    d = make_run(results, "A", 1, 1000, gate_failures=["mean host load 30.0 exceeds the 28 CPUs"], stamp="20261027T020000Z")
    done = json.loads((d / "completion.json").read_text())
    done["host_load1_mean"] = 30.0
    (d / "completion.json").write_text(json.dumps(done))
    rf.SKIPPED.clear()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("run:\n  measure_from_sec: 120\n  timeout: 900\n")
    monkeypatch.setattr(sys, "argv", ["report_figures.py", str(results), str(out), "x", str(cfg)])
    rf.main()
    text = capsys.readouterr().out
    assert "Run-to-run noise: the repeat of A (no mgmt) seed 1" in text and "above the 28 CPUs, and is left out" in text
    assert "second run against the first" not in text
