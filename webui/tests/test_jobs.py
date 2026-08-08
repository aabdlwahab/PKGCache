"""Behavior tests for the single-runner background job service."""
import threading
import unittest

from app.errors import OpError
from app.services.jobs import Jobs


class _BlockingOperations:
    def __init__(self):
        self.release = threading.Event()

    def build(self, action, params):
        def run():
            self.release.wait(timeout=5)
            yield "done\n"

        return run()


class JobsTests(unittest.TestCase):
    def test_second_job_is_rejected_as_a_conflict(self):
        operations = _BlockingOperations()
        jobs = Jobs(operations)
        jobs.start("checkpoint", {})
        try:
            with self.assertRaises(OpError) as caught:
                jobs.start("export", {})
            self.assertEqual(caught.exception.status, 409)
        finally:
            operations.release.set()


if __name__ == "__main__":
    unittest.main()
