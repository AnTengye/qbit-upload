import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch, MagicMock

spec = importlib.util.spec_from_file_location("rescue_hook", Path(__file__).with_name("qbit-rescue-hook.py"))
hook = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hook)


class CompletionHookTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.infohash = "a" * 40
        self.guard = Path(self.directory.name) / (self.infohash + ".json")

    def run_hook(self, guard, torrent, failure=None):
        if guard is not None:
            self.guard.write_text(json.dumps(guard))
        response = MagicMock()
        response.__enter__.return_value = response
        response.status = 200
        response.headers = {"Content-Type": "application/json"}
        response.read.return_value = json.dumps([torrent]).encode()
        opener = MagicMock()
        opener.open.return_value = response
        if failure:
            opener.open.side_effect = failure
        with patch.dict(os.environ, {"QBIT_RESCUE_GUARDS": self.directory.name}), patch.object(sys, "argv", ["hook", "/downloads/movie", "Movie", self.infohash]), patch.object(hook.urllib.request, "build_opener", return_value=opener), patch.object(hook.os, "execv") as execute, patch("sys.stdout", new=io.StringIO()):
            if failure:
                with self.assertRaises(type(failure)):
                    hook.main()
            else:
                hook.main()
            return execute.call_args

    def test_matching_instance_is_deferred_to_worker(self):
        self.assertIsNone(self.run_hook({"hash": self.infohash, "added_on": 123, "original": True}, {"hash": self.infohash, "added_on": 123}))

    def test_pending_candidate_needs_ownership_tag(self):
        self.assertIsNone(self.run_hook({"hash": self.infohash, "added_on": 0, "tag": "qbit-rescue"}, {"hash": self.infohash, "added_on": 123, "tags": "qbit-rescue"}))

    def test_readded_torrent_keeps_legacy_callback(self):
        call = self.run_hook({"hash": self.infohash, "added_on": 123}, {"hash": self.infohash, "added_on": 124})
        self.assertEqual(call.args, ("/bin/bash", ["/bin/bash", "/config/qbit-move.sh", "/downloads/movie", "Movie"]))

    def test_unmanaged_torrent_keeps_legacy_callback(self):
        self.assertIsNotNone(self.run_hook(None, {"hash": self.infohash}))

    def test_api_failure_keeps_managed_data_for_recovery(self):
        self.assertIsNone(self.run_hook({"hash": self.infohash, "added_on": 123}, {}, TimeoutError()))


if __name__ == "__main__":
    unittest.main()
