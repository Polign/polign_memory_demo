"""Run Recall against a temporary Polign database, kill it, and recover.

Usage: python3 tests/restart.py /absolute/path/to/polign-server
No model API calls, cloud buckets, or existing demo data are used.
"""
from pathlib import Path
import os
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
SERVER = str(Path(sys.argv[1]).resolve())

with tempfile.TemporaryDirectory(prefix="recall-demo-restart-") as tmp:
    temp = Path(tmp)
    testbin = temp / "demo.test"
    subprocess.run(["go", "test", "-c", "-o", str(testbin), "."], cwd=ROOT, check=True)
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    url = f"http://127.0.0.1:{port}"
    env = {k: v for k, v in os.environ.items() if not k.startswith("POLIGN_")}
    env.update(POLIGN_MEMORY_DEMO_URL=url, POLIGN_MEMORY_TEST_COLLECTION="it_" + uuid.uuid4().hex)
    child = None
    with (temp / "server.log").open("w+") as log:
        def start():
            process = subprocess.Popen([SERVER, "-store", "fs:" + str(temp / "data"), "-http", f"127.0.0.1:{port}",
                                        "-grpc", "127.0.0.1:0", "-telemetry=false"], env=env, stdout=log, stderr=log)
            try:
                for _ in range(160):
                    if process.poll() is not None:
                        raise RuntimeError("test server exited during startup")
                    try:
                        with urllib.request.urlopen(url + "/healthz", timeout=1) as response:
                            if response.status == 200:
                                return process
                    except OSError:
                        pass
                    time.sleep(0.25)
                raise RuntimeError("test server did not become healthy")
            except BaseException:
                process.kill()
                process.wait()
                raise

        def phase(pattern):
            subprocess.run([str(testbin), "-test.v", "-test.run", pattern, "-test.timeout", "90s"], env=env, cwd=ROOT, check=True, timeout=100)

        try:
            child = start()
            phase("^TestIntegrationWrite$")
            child.kill()
            child.wait()
            child = start()
            phase("^TestIntegration(RecallAfterRestart|WriteAfterRestart)$")
            print("PASS: Recall writes, corrections, typed values, client restart, server SIGKILL and cold recovery")
        except BaseException:
            log.flush()
            log.seek(0)
            print(log.read()[-12000:], file=sys.stderr)
            raise
        finally:
            if child is not None and child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=35)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
