"""Tests of summarize.py (standard library only).

Run: python3 -B -m unittest experiments/05-governor-exploration/analysis/test_summarize.py
"""
import contextlib
import csv
import importlib.util
import io
import os
import pathlib
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True
_spec = importlib.util.spec_from_file_location(
    "summarize_05", pathlib.Path(__file__).resolve().parent / "summarize.py")
summarize = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(summarize)

GPU_COLUMNS = ["cell", "seed", "profile", "sens", "arm", "preset", "ceiling_pct", "cap_pct", "gate", "result",
               "native", "writes", "status", "load", "class", "gpus", "transfer", "transfer_intensity", "cap_w",
               "time_s", "host_j", "gpu_j", "cpu_j", "mean_host_w", "mean_compute_mhz", "mean_mem_mhz",
               "min_gpu_throughput", "min_peer_mul", "peer_rx_bytes", "peer_gbps", "edp_js"]


def gpu_row(preset, host_j, time_s, ceiling=""):
    r = dict.fromkeys(GPU_COLUMNS, "")
    r.update(cell=f"gpu_p_{preset}{ceiling}", seed="1", profile="p", arm=preset, preset=preset,
             ceiling_pct=ceiling, cap_pct="100", gate="profile", status="ok", load="job",
             **{"class": "gpu.compute_bound"}, gpus="1", transfer="none",
             time_s=str(time_s), host_j=str(host_j))
    return r


class AcceptanceTest(unittest.TestCase):
    def acceptance_rows(self, rows):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "t-gpu.csv"), "w", newline="") as f:
                w = csv.DictWriter(f, fieldnames=GPU_COLUMNS)
                w.writeheader()
                w.writerows(rows)
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                self.assertEqual(self.run_main(d), 0)
        table = out.getvalue().split("## Acceptance rule")[1].split("| candidate | profiles where")[0]
        return {cells[1].strip(): cells[-2].strip() for cells in
                (line.split("|") for line in table.splitlines() if line.startswith("| GPU"))}

    @staticmethod
    def run_main(d):
        argv = sys.argv
        sys.argv = ["summarize.py", d]
        try:
            return summarize.main()
        finally:
            sys.argv = argv

    def test_a_candidate_that_changes_nothing_fails(self):
        # sustained gives the baseline's energy and time exactly: it lowers
        # no energy, so it fails the rule; only the baseline arm itself is
        # labelled baseline.
        got = self.acceptance_rows([gpu_row("auto", 1000, 60), gpu_row("sustained", 1000, 60),
                                    gpu_row("efficient", 900, 62, "80")])
        self.assertEqual(got.get("GPU sustained"), "fail", got)
        self.assertEqual(got.get("GPU efficient 80 %"), "pass", got)

    def acceptance_table(self, rows):
        """The per-profile rows, by (candidate, profile): cells, timed out,
        energy, time p95, worst time, idle energy, burst, verdict; and the
        pass counts."""
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "t-gpu.csv"), "w", newline="") as f:
                w = csv.DictWriter(f, fieldnames=GPU_COLUMNS)
                w.writeheader()
                w.writerows(rows)
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                self.assertEqual(self.run_main(d), 0)
        text = out.getvalue().split("## Acceptance rule")[1]
        table, counts = text.split("| candidate | profiles where")
        rows = {(c[1].strip(), c[3].strip()): [x.strip() for x in c[4:12]] for c in
                (line.split("|") for line in table.splitlines() if line.startswith("| GPU"))}
        return rows, {c[1].strip(): c[2].strip() for c in (line.split("|") for line in counts.splitlines() if line.startswith("| GPU"))}

    def test_acceptance_judges_each_profile_on_its_job_cells_once_each(self):
        # The audit found the rule pooled the profiles and the idle cells,
        # counted each seed of a deterministic cell as a sample and dropped
        # timed-out cells; Matteo chose to judge per profile, idle apart.
        def row(preset, host_j, time_s, ceiling="", **kw):
            r = gpu_row(preset, host_j, time_s, ceiling)
            r.update(kw)
            return r
        rows = []
        for seed in ("1", "2"):
            rows += [row("auto", 1000, 60, seed=seed), row("efficient", 900, 62, "80", seed=seed)]
        # On profile q the same candidate slows the job by 30 %: it fails there.
        rows += [row("auto", 1000, 60, profile="q"), row("efficient", 950, 78, "80", profile="q")]
        # Idle cells on p: twice the energy of auto, which pooled into the
        # median would fail the candidate; they are reported apart.
        rows += [row("auto", 100, 60, load="idle"), row("efficient", 200, 60, "80", load="idle")]
        got, counts = self.acceptance_table(rows)
        cells, timed_out, e, t, worst, idle, burst, verdict = got[("GPU efficient 80 %", "p")]
        self.assertEqual((cells, verdict, idle), ("1", "pass", "+100.0"), got)  # one cell, its two seeds averaged
        self.assertEqual(got[("GPU efficient 80 %", "q")][-1], "fail", got)
        # The worst cell is shown beside the p95, which can hide it.
        self.assertEqual(got[("GPU efficient 80 %", "q")][4], "+30.0", got)
        self.assertEqual(counts["GPU efficient 80 %"], "1 of 2", counts)
        # A timed-out cell keeps its slowdown at the limit, a lower bound: a
        # pass is then not established on that profile.
        rows += [row("auto", 1000, 60, profile="t"), row("efficient", 900, 61, "80", profile="t", status="timeout")]
        got, _ = self.acceptance_table(rows)
        self.assertEqual(got[("GPU efficient 80 %", "t")][1], "1", got)
        self.assertEqual(got[("GPU efficient 80 %", "t")][-1], "undecided (timed-out cells)", got)

    def test_q5_flags_a_value_that_flips_the_energy_change_or_breaks_the_limit(self):
        def row(preset, host_j, time_s, ceiling="", **kw):
            r = gpu_row(preset, host_j, time_s, ceiling)
            r.update(kw)
            return r
        rows = []
        for sens, e, t in (("clockBeta=1.0", 1010, 62), ("clockBeta=2.5", 900, 62), ("dataPathHoldW=1", 950, 62),
                           ("dataPathHoldW=50", 960, 62), ("memoryKneeFrac=0.87", 900, 75)):
            rows += [row("auto", 1000, 60, sens=sens), row("efficient", e, t, "80", sens=sens)]
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "t-gpu.csv"), "w", newline="") as f:
                w = csv.DictWriter(f, fieldnames=GPU_COLUMNS)
                w.writeheader()
                w.writerows(rows)
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                self.assertEqual(self.run_main(d), 0)
        q5 = out.getvalue().split("## Q5: the acceptance at every value")[1]
        line = next(l for l in q5.splitlines() if l.startswith("| GPU efficient 80 %"))
        cells = [c.strip() for c in line.split("|")]
        self.assertEqual(cells[3], "clockBeta=1.0", line)
        self.assertEqual(cells[4], "+1.0 at clockBeta=1.0", line)
        # The slowdown limit is part of the rule: +25 % is over the eco 20 %.
        self.assertEqual(cells[5], "memoryKneeFrac=0.87", line)
        self.assertEqual(cells[6], "+25.0 at memoryKneeFrac=0.87", line)
        self.assertEqual(cells[7], "+25.0 at memoryKneeFrac=0.87", line)
        # Q5 holds only when both stay within the rule at every value.
        self.assertEqual(cells[9], "fails", line)
        # A candidate whose arm the grid does not run is named, not left out:
        # a Tier A pass without a Q5 row would read as both halves passed.
        self.assertIn("GPU efficient 75 % with a 60 % cap", q5.split("Not run in Q5")[1])


    def test_q3_compares_the_gate_on_against_off_and_sustained_against_auto(self):
        def row(preset, host_j, time_s, gate):
            r = gpu_row(preset, host_j, time_s)
            r.update(gate=gate, sens="slowFraction=0.5", gpus="4", transfer="gather", transfer_intensity="0.3")
            return r
        rows = [row("auto", 1000, 60, "profile"), row("auto", 1100, 66, "on"), row("sustained", 1000, 60, "on")]
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "t-gpu.csv"), "w", newline="") as f:
                w = csv.DictWriter(f, fieldnames=GPU_COLUMNS)
                w.writeheader()
                w.writerows(rows)
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                self.assertEqual(self.run_main(d), 0)
        q3 = out.getvalue().split("## Q3: the receiver gate")[1]
        line = next(l for l in q3.splitlines() if l.startswith("| 0.5 |"))
        self.assertEqual([c.strip() for c in line.split("|")][2:5], ["p", "+10.0, +10.0, +0.1", "-9.1, -9.1, -0.1"], line)


    def test_q5_fails_on_the_slowdown_alone_and_holds_within_both(self):
        # efficient 80: the energy change is negative at every value, and
        # only the p95 at memoryKneeFrac=0.87 (+25 %) is over the eco 20 %.
        # efficient 90: negative and within the limit everywhere.
        def row(preset, host_j, time_s, ceiling="", **kw):
            r = gpu_row(preset, host_j, time_s, ceiling)
            r.update(kw)
            return r
        rows = []
        for sens, t80 in (("clockBeta=1.0", 62), ("clockBeta=2.5", 62), ("memoryKneeFrac=0.87", 75)):
            rows += [row("auto", 1000, 60, sens=sens), row("efficient", 900, t80, "80", sens=sens),
                     row("efficient", 950, 62, "90", sens=sens)]
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "t-gpu.csv"), "w", newline="") as f:
                w = csv.DictWriter(f, fieldnames=GPU_COLUMNS)
                w.writeheader()
                w.writerows(rows)
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                self.assertEqual(self.run_main(d), 0)
        q5 = out.getvalue().split("## Q5: the acceptance at every value")[1]
        verdict = {c[1].strip(): (c[3].strip(), c[9].strip()) for c in
                   (l.split("|") for l in q5.splitlines() if l.startswith("| GPU efficient"))}
        self.assertEqual(verdict["GPU efficient 80 %"], ("none", "fails"), verdict)
        self.assertEqual(verdict["GPU efficient 90 %"], ("none", "holds"), verdict)


if __name__ == "__main__":
    unittest.main()
