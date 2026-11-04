"""Regression checks for Step 31 load evidence calculations."""
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).parent))
import load_verdict


class LoadVerdictTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def records(self, name, latencies):
        path = self.root / name
        with path.open("w") as file:
            for index, latency in enumerate(latencies):
                file.write(json.dumps({"event_id": f"ld-{name}-{index:06d}",
                                       "status": 202, "lat_ms": latency}) + "\n")
        return path

    def test_paired_delta_uses_event_distribution(self):
        proxy = self.records("proxy", [10, 12, 13, 14])
        direct = self.records("direct", [2, 3, 4, 5])
        self.assertEqual(load_verdict.paired_proxy_delta_p99(proxy, direct), 9)

    def test_missing_pair_cannot_pass(self):
        proxy = self.records("proxy", [10, 12])
        direct = self.records("direct", [2])
        self.assertIsNone(load_verdict.paired_proxy_delta_p99(proxy, direct))

    def test_integer_gc_trace_is_a_sample(self):
        log = self.root / "bridge.log"
        log.write_text("gc 1 @0s: 3->4->1 MB, 4 MB goal\n")
        self.assertEqual(load_verdict.heap_peak_mb(log), (4.0, 1))


if __name__ == "__main__":
    unittest.main()
