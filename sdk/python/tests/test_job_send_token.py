import os
import unittest
from unittest import mock

from toft.env import app_or_job_token


class JobSendToken(unittest.TestCase):
    """Inside a job, which holds no app token, the SDK sends with the run's own
    credential — the platform accepts it for sending and nothing else."""

    def test_app_token_wins(self):
        with mock.patch.dict(os.environ, {"TOFT_APP_TOKEN": "tofta_x", "TOFT_RUN_ID": "r",
                                          "TOFT_JOB_TOKEN": "jwt"}, clear=True):
            self.assertEqual(app_or_job_token(), "tofta_x")

    def test_a_job_uses_its_run_token(self):
        with mock.patch.dict(os.environ, {"TOFT_RUN_ID": "r", "TOFT_JOB_TOKEN": "jwt"}, clear=True):
            self.assertEqual(app_or_job_token(), "jwt")

    def test_neither_explains(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(Exception):
                app_or_job_token()


if __name__ == "__main__":
    unittest.main()
