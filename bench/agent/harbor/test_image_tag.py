import importlib.util
import subprocess
import tempfile
import unittest
import unittest.mock
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("image_tag", HERE / "image-tag.py")
image_tag = importlib.util.module_from_spec(spec)
spec.loader.exec_module(image_tag)

DOCKERFILE = """\
ARG TASK=bench/agent/tasks/t
FROM golang:1.23 AS tools
COPY go.mod go.sum ./
COPY cmd cmd
FROM debian AS base
FROM base AS control
ARG TASK
COPY --chown=agent $TASK/task.md /tmp/task.md
COPY --from=tools /out /opt/kavach
RUN echo 'unbalanced "quote'
FROM base AS grader
ARG TASK
COPY ${TASK}/hidden /task/hidden
"""


class ImageTag(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        task = self.root / "bench/agent/tasks/t"
        for name, text in {
            "bench/agent/tasks/t/Dockerfile": DOCKERFILE,
            "bench/agent/tasks/t/task.md": "task",
            "bench/agent/tasks/t/hidden/test.patch": "patch",
            "bench/agent/tasks/t/notcopied.md": "x",
            "go.mod": "m", "go.sum": "s", "cmd/main.go": "package main",
            "unrelated.txt": "u",
        }.items():
            (self.root / name).parent.mkdir(parents=True, exist_ok=True)
            (self.root / name).write_text(text)
        subprocess.run(["git", "init", "-q"], cwd=self.root, check=True)
        subprocess.run(["git", "add", "-A"], cwd=self.root, check=True)
        self.patches = [
            unittest.mock.patch.object(image_tag, "ROOT", self.root),
            unittest.mock.patch.object(image_tag, "TASKS", self.root / "bench/agent/tasks"),
        ]
        for p in self.patches:
            p.start()
            self.addCleanup(p.stop)
        self.dockerfile = task / "Dockerfile"

    def test_copy_sources_follow_args_and_skip_stage_copies(self):
        self.assertEqual(image_tag.copy_sources(self.dockerfile), [
            "bench/agent/tasks/t/hidden", "bench/agent/tasks/t/task.md", "cmd", "go.mod", "go.sum",
        ])

    def test_targets_and_ref(self):
        self.assertEqual(image_tag.targets("t"), ["tools", "base", "control", "grader"])
        self.assertRegex(image_tag.ref("t", "grader"), r"^ghcr\.io/kavachlabs/bench-t:grader-[0-9a-f]{12}$")
        with self.assertRaises(SystemExit):
            image_tag.ref("t", "nope")

    def test_key_follows_copied_files_only(self):
        before = image_tag.content_key("t")
        self.assertEqual(before, image_tag.content_key("t"))
        (self.root / "unrelated.txt").write_text("changed")
        (self.root / "bench/agent/tasks/t/notcopied.md").write_text("changed")
        self.assertEqual(before, image_tag.content_key("t"))
        for name in ["cmd/main.go", "bench/agent/tasks/t/hidden/test.patch", "bench/agent/tasks/t/Dockerfile"]:
            path = self.root / name
            old = path.read_text()
            path.write_text(old + "\n# edit\n")
            self.assertNotEqual(before, image_tag.content_key("t"), name)
            path.write_text(old)
            self.assertEqual(before, image_tag.content_key("t"), name)

    def test_mode_change_changes_key(self):
        before = image_tag.content_key("t")
        (self.root / "cmd/main.go").chmod(0o755)
        self.assertNotEqual(before, image_tag.content_key("t"))

    def test_dockerignore_files_are_inputs(self):
        before = image_tag.content_key("t")
        (self.root / ".dockerignore").write_text("cmd/*.go\n")
        subprocess.run(["git", "add", "-A"], cwd=self.root, check=True)
        with_root = image_tag.content_key("t")
        self.assertNotEqual(before, with_root)
        (self.root / ".dockerignore").write_text("cmd/*.txt\n")
        self.assertNotEqual(with_root, image_tag.content_key("t"))
        (self.root / "bench/agent/tasks/t/Dockerfile.dockerignore").write_text("x\n")
        self.assertNotEqual(image_tag.content_key("t"), with_root)

    def test_args_resolve_per_stage(self):
        self.dockerfile.write_text("""\
ARG D=glob
FROM debian AS a
ARG D=one
COPY $D/f /x
FROM debian AS b
ARG D=two
COPY ${D}/f /x
FROM debian AS c
COPY x/f /x
""")
        self.assertEqual(image_tag.copy_sources(self.dockerfile), ["one/f", "two/f", "x/f"])
        self.dockerfile.write_text("""\
ARG D=glob
FROM debian AS a
ARG D
COPY $D/f /x
FROM debian AS b
COPY $D/f /x
""")
        with self.assertRaises(SystemExit):
            image_tag.copy_sources(self.dockerfile)
        self.dockerfile.write_text(self.dockerfile.read_text().replace("FROM debian AS b\n", "FROM debian AS b\nARG D\n"))
        self.assertEqual(image_tag.copy_sources(self.dockerfile), ["glob/f"])

    def test_dirty_files(self):
        subprocess.run(["git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x"], cwd=self.root, check=True)
        self.assertEqual(image_tag.dirty_files("t"), [])
        (self.root / "cmd/new.go").write_text("x")
        (self.root / "unrelated2.txt").write_text("x")
        self.assertEqual(image_tag.dirty_files("t"), ["?? cmd/new.go"])
        (self.root / ".dockerignore").write_text("cmd/new.go\n")
        self.assertEqual(image_tag.dirty_files("t"), [])
        (self.root / "cmd/main.go").write_text("changed")
        self.assertEqual(image_tag.dirty_files("t"), [" M cmd/main.go"])

    def test_amd64_pin_drops_arm64(self):
        self.assertEqual(image_tag.platforms("t"), ["linux/amd64", "linux/arm64"])
        self.dockerfile.write_text(DOCKERFILE.replace("FROM debian", "FROM --platform=linux/amd64 debian"))
        self.assertEqual(image_tag.platforms("t"), ["linux/amd64"])

    def test_real_dockerfiles_resolve(self):
        for p in self.patches:
            p.stop()
        self.addCleanup(lambda: [p.start() for p in self.patches])
        for task in image_tag.task_names():
            for target in image_tag.harbor_targets(task):
                self.assertTrue(image_tag.ref(task, target))


if __name__ == "__main__":
    unittest.main()
