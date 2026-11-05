import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("publish-models.py").resolve()
SNAPSHOT = SCRIPT.parent.parent / "data/models.snapshot.json"
REF = "refs/heads/models-data"


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo, self.remote = self.root / "work", self.root / "remote.git"
        self.repo.mkdir()
        subprocess.run(["git", "init", "--bare", str(self.remote)], check=True, capture_output=True)
        self.git("init")
        self.git("config", "user.name", "Fixture")
        self.git("config", "user.email", "fixture@example.invalid")
        (self.repo / "code.txt").write_text("keep source tree and index unchanged\n")
        self.git("add", "code.txt")
        self.git("commit", "-m", "source")
        self.source_head = self.git("rev-parse", "HEAD")
        self.source_tree = self.git("write-tree")

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.repo, stderr=subprocess.PIPE).decode().strip()

    def run_script(self, action, directory, *, remote=None, success=True):
        result = subprocess.run([sys.executable, str(SCRIPT), action,
                                 "--directory", str(directory), "--remote", str(remote or self.remote),
                                 "--bootstrap", str(SNAPSHOT)], cwd=self.repo, capture_output=True, text=True)
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout)
        return result

    def prepare(self, name="attempt"):
        directory = self.root / name
        self.run_script("prepare", directory)
        (directory / "candidate.json").write_bytes((directory / "previous.json").read_bytes())
        return directory

    def edit(self, directory, *, meaningful=True):
        path = directory / "candidate.json"
        doc = json.loads(path.read_bytes())
        if meaningful:
            doc["models"][0]["description"] += " Updated reference."
        else:
            doc["source_sha256"] = "a" * 64
            doc["models_dev_sha256"] = "b" * 64
            doc["fetched_at"] = "2099-01-01T00:00:00Z"
            doc["counts"]["catalog"] += 1
        path.write_text(json.dumps(doc) + "\n")

    def head(self):
        return self.git("--git-dir=" + str(self.remote), "rev-parse", REF)

    def test_first_publish_and_semantic_noop_do_not_touch_code(self):
        first = self.prepare()
        self.run_script("publish", first)
        head = self.head()
        self.assertEqual(self.git("--git-dir=" + str(self.remote), "ls-tree", "--name-only", head), "models.json")
        self.assertEqual(self.git("--git-dir=" + str(self.remote), "rev-list", "--count", REF), "1")
        again = self.prepare("again")
        self.edit(again, meaningful=False)
        result = self.run_script("publish", again)
        self.assertIn("no commit or push", result.stdout)
        self.assertEqual(self.head(), head)
        self.assertEqual(self.git("rev-parse", "HEAD"), self.source_head)
        self.assertEqual(self.git("write-tree"), self.source_tree)
        self.assertEqual(self.git("status", "--porcelain"), "")

    def test_changed_snapshots_have_one_reachable_commit(self):
        self.run_script("publish", self.prepare())
        first = self.head()
        second = self.prepare("second")
        self.edit(second)
        self.run_script("publish", second)
        self.assertNotEqual(self.head(), first)
        self.assertEqual(self.git("--git-dir=" + str(self.remote), "rev-list", "--count", REF), "1")
        commit = self.git("--git-dir=" + str(self.remote), "cat-file", "-p", self.head())
        self.assertNotIn("\nparent ", "\n" + commit)

    def test_first_publish_race_is_rejected(self):
        stale, winner = self.prepare("stale"), self.prepare("winner")
        self.edit(winner)
        self.run_script("publish", winner)
        head = self.head()
        result = self.run_script("publish", stale, success=False)
        self.assertIn("rejected", result.stderr)
        self.assertEqual(self.head(), head)

    def test_existing_branch_race_is_rejected(self):
        self.run_script("publish", self.prepare())
        stale, winner = self.prepare("stale"), self.prepare("winner")
        self.edit(winner)
        self.run_script("publish", winner)
        head = self.head()
        self.edit(stale)
        self.edit(stale)
        result = self.run_script("publish", stale, success=False)
        self.assertIn("rejected", result.stderr)
        self.assertEqual(self.head(), head)

    def test_failed_prepare_does_not_bootstrap_or_reuse_old_candidate(self):
        directory = self.prepare()
        self.run_script("prepare", directory, remote=self.root / "missing", success=False)
        self.assertFalse((directory / "state.json").exists())
        self.assertFalse((directory / "candidate.json").exists())

    def test_invalid_candidate_and_tampered_previous_never_publish(self):
        self.run_script("publish", self.prepare())
        head = self.head()
        bad = self.prepare("bad")
        (bad / "candidate.json").write_text('{"schema_version":999}')
        self.run_script("publish", bad, success=False)
        self.assertEqual(self.head(), head)
        tampered = self.prepare("tampered")
        (tampered / "previous.json").write_text("{}")
        self.run_script("publish", tampered, success=False)
        self.assertEqual(self.head(), head)

    def test_ref_with_code_tree_is_not_overwritten(self):
        self.git("push", str(self.remote), "HEAD:" + REF)
        result = self.run_script("prepare", self.root / "bad-tree", success=False)
        self.assertIn("only models.json", result.stderr)
        self.assertEqual(self.head(), self.source_head)

    def test_oversized_and_short_name_collision_candidates_are_rejected(self):
        self.run_script("publish", self.prepare())
        head = self.head()
        large = self.prepare("large")
        (large / "candidate.json").write_bytes(b" " * ((4 << 20) + 1))
        self.run_script("publish", large, success=False)
        self.assertEqual(self.head(), head)
        collision = self.prepare("collision")
        path = collision / "candidate.json"
        doc = json.loads(path.read_bytes())
        doc["models"][1]["id"] = "different/" + doc["models"][0]["id"].rsplit("/", 1)[-1]
        path.write_text(json.dumps(doc))
        self.run_script("publish", collision, success=False)
        self.assertEqual(self.head(), head)

    def test_existing_data_history_is_rejected_even_if_content_is_unchanged(self):
        self.run_script("publish", self.prepare())
        head = self.head()
        tree = self.git("rev-parse", head + "^{tree}")
        child = self.git("commit-tree", tree, "-p", head, "-m", "manual history")
        self.git("push", str(self.remote), child + ":" + REF)
        result = self.run_script("prepare", self.root / "has-parent", success=False)
        self.assertIn("parentless snapshot", result.stderr)
        self.assertEqual(self.head(), child)


if __name__ == "__main__":
    unittest.main()
