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
        table = out.getvalue().split("## Acceptance rule")[1]
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


if __name__ == "__main__":
    unittest.main()
