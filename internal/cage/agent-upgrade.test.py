"""Exercise the public guest upgrade path using real processes and a private npm prefix."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

SOURCE = Path(__file__).with_name("agent-runtime.py")


class UpgradeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.home = Path(self.tmp.name)
        env = mock.patch.dict(os.environ, {"HOME": str(self.home)})
        env.start()
        self.addCleanup(env.stop)
        module = importlib.util.spec_from_file_location("guest_runtime", SOURCE)
        self.rt = importlib.util.module_from_spec(module)
        module.loader.exec_module(self.rt)
        self.bin = self.home / ".local/bin"
        self.bin.mkdir(parents=True)
        self.package = self.home / ".local/lib/node_modules/@openai/codex"
        (self.package / "bin").mkdir(parents=True)
        (self.package / "package.json").write_text('{"name":"@openai/codex","version":"1.0.0"}')
        old = self.package / "bin/codex.js"
        old.write_text("#!/bin/sh\necho codex-cli 1.0.0\n")
        old.chmod(0o755)
        (self.bin / "codex").symlink_to(old)
        npm = self.bin / "npm"
        npm.write_text("""#!/bin/sh
set -eu
if [ "$1" = prefix ]; then echo "$HOME/.local"; exit; fi
case " $* " in *' --include=optional '*) ;; *) exit 8 ;; esac
while [ "$1" != --prefix ]; do shift; done
prefix=$2
p=$prefix/lib/node_modules/@openai/codex
mkdir -p "$prefix/bin" "$p/bin"
echo '{"name":"@openai/codex","version":"2.0.0"}' > "$p/package.json"
printf '#!/bin/sh\\n%s\\n' "$FAKE_VERSION_CMD" > "$p/bin/codex.js"
chmod +x "$p/bin/codex.js"
ln -s ../lib/node_modules/@openai/codex/bin/codex.js "$prefix/bin/codex"
""")
        npm.chmod(0o755)
        self.catalog = [{"id":"codex", "name":"OpenAI Codex", "binary":"codex",
                         "channels":[{"kind":"npm","package":"@openai/codex","label":"npm"}]}]

    def test_menu_and_atomic_upgrade(self):
        with mock.patch("builtins.input", return_value="1"), mock.patch.dict(
                os.environ, {"FAKE_VERSION_CMD": "echo codex-cli 2.0.0"}), contextlib.redirect_stdout(io.StringIO()) as menu:
            self.assertEqual(self.rt.upgrade_agent([], self.catalog), 0)
        self.assertIn("OpenAI Codex (codex)", menu.getvalue())
        self.assertEqual(subprocess.check_output([self.bin / "codex", "--version"], text=True).strip(), "codex-cli 2.0.0")
        self.assertEqual(json.loads((self.package / "package.json").read_text())["version"], "2.0.0")

    def test_missing_platform_or_wrong_version_keeps_working_binary(self):
        for script, reason in [("echo 'Missing optional dependency @openai/codex-linux-x64'; exit 1", "缺少平台包"),
                               ("echo codex-cli 1.0.0", "版本不符")]:
            with self.subTest(script=script), mock.patch.dict(os.environ, {"FAKE_VERSION_CMD": script}), self.assertRaisesRegex(ValueError, reason):
                self.rt.upgrade_agent(["codex"], self.catalog)
            self.assertEqual(subprocess.check_output([self.bin / "codex", "--version"], text=True).strip(), "codex-cli 1.0.0")

    def test_empty_or_invalid_selection_does_not_install(self):
        with mock.patch("builtins.input", side_effect=EOFError), mock.patch.object(self.rt, "install_agent") as install:
            self.assertEqual(self.rt.upgrade_agent([], self.catalog), 0)
            with self.assertRaises(ValueError):
                self.rt.upgrade_agent(["absent"], self.catalog)
            install.assert_not_called()


if __name__ == "__main__":
    unittest.main()
