"""Provider credentials stay confined to stdin and the intended local context."""

import json
import subprocess
import unittest
from unittest.mock import patch

from . import platform_credentials


class CredentialBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.enterContext(patch("shutil.which", return_value="/tools/kubectl"))

    def test_missing_key_does_not_contact_kubernetes(self):
        with patch.dict("os.environ", {}, clear=True), patch("subprocess.check_output") as context:
            with self.assertRaisesRegex(SystemExit, "Set GOOGLE_API_KEY"):
                platform_credentials.main()
            context.assert_not_called()

    def test_wrong_context_does_not_apply_secret(self):
        with (
            patch.dict("os.environ", {"GOOGLE_API_KEY": "synthetic-test-key"}),
            patch("subprocess.check_output", return_value="unrelated-cluster\n"),
            patch("subprocess.run") as apply,
        ):
            with self.assertRaisesRegex(SystemExit, "k3d-local"):
                platform_credentials.main()
            apply.assert_not_called()

    def test_key_is_only_in_stdin_and_error_cannot_echo_it(self):
        key = "synthetic-test-key"
        with (
            patch.dict("os.environ", {"GOOGLE_API_KEY": key}),
            patch("subprocess.check_output", return_value="k3d-local\n"),
            patch("subprocess.run", return_value=subprocess.CompletedProcess([], 1, key, key)) as apply,
        ):
            with self.assertRaisesRegex(SystemExit, "not applied") as error:
                platform_credentials.main()
            self.assertNotIn(key, str(error.exception))
            argv = apply.call_args.args[0]
            self.assertNotIn(key, " ".join(argv))
            self.assertEqual(argv[1:3], ["--context", "k3d-local"])
            self.assertIn("--server-side", argv)
            submitted = json.loads(apply.call_args.kwargs["input"])
            self.assertEqual(submitted["metadata"]["namespace"], "agentops")
            self.assertEqual(submitted["stringData"]["GOOGLE_API_KEY"], key)

    def test_success_contains_no_key(self):
        with (
            patch.dict("os.environ", {"GOOGLE_API_KEY": "synthetic-test-key"}),
            patch("subprocess.check_output", return_value="k3d-local\n"),
            patch("subprocess.run", return_value=subprocess.CompletedProcess([], 0)),
            patch("builtins.print") as output,
        ):
            platform_credentials.main()
            self.assertNotIn("synthetic-test-key", str(output.call_args))
