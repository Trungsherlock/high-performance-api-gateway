"""Short two-process-gateway tier comparison; requires an isolated running Redis.
Build bin/gateway[.exe] and bin/mockbackend[.exe] first. No Redis keys are flushed.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import socket
import statistics
import subprocess
import time
import http.client
from urllib.parse import urlsplit
from datetime import datetime, timezone

ROOT = Path(__file__).resolve().parent.parent

def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]

CONNECTIONS = {}

def get(url):
    parsed = urlsplit(url)
    connection = CONNECTIONS.setdefault(parsed.netloc, http.client.HTTPConnection(parsed.netloc, timeout=3))
    connection.request("GET", parsed.path + ("?" + parsed.query if parsed.query else ""))
    response = connection.getresponse()
    body = response.read()
    if response.status != 200:
        raise RuntimeError(f"HTTP {response.status} from {parsed.netloc}")
    return body

def snapshot(url):
    return json.loads(get(url))

def ready(url, process):
    end = time.monotonic() + 10
    while time.monotonic() < end:
        if process.poll() is not None:
            raise RuntimeError("child exited before readiness")
        try:
            get(url)
            return
        except OSError:
            time.sleep(.03)
    raise RuntimeError("readiness deadline")

def run(mode, trial, args, folder):
    children, logs, commands = [], [], []
    suffix = ".exe" if os.name == "nt" else ""
    def start(name, *flags):
        command = [str(ROOT / "bin" / (name + suffix)), *map(str, flags)]
        log = open(folder / f"{mode}-{trial}-{name}-{len(children)}.log", "w")
        logs.append(log)
        process = subprocess.Popen(command, stdout=log, stderr=log, cwd=ROOT,
                                   env={**os.environ, "CACHE_ENABLED": "true"})
        children.append(process)
        commands.append({"pid": process.pid, "argv": command})
        return process
    try:
        backend_port = port()
        backend_url = f"http://127.0.0.1:{backend_port}"
        backend = start("mockbackend", "-addr", f"127.0.0.1:{backend_port}", "-delay", "50ms")
        ready(backend_url + "/healthz", backend)
        urls, admins = [], []
        for index in range(2):
            data, admin = port(), port()
            process = start("gateway", "-addr", f"127.0.0.1:{data}", "-admin-addr", f"127.0.0.1:{admin}",
                            "-upstream", backend_url, "-cache-mode", mode,
                            "-redis-addr", args.redis_addr, "-redis-timeout", "50ms",
                            "-instance", f"v24-{index}", "-request-log-every", "0",
                            "-max-concurrent", "16", "-max-waiters", "16")
            urls.append(f"http://127.0.0.1:{data}/api/public/item?id=42")
            admins.append(f"http://127.0.0.1:{admin}/metrics")
            ready(f"http://127.0.0.1:{admin}/readyz", process)
        before = [snapshot(url) for url in admins]
        a = get(urls[0])
        b = get(urls[1])
        assert a == b
        primed = [snapshot(url) for url in admins]
        samples = []
        for i in range(100):
            started = time.perf_counter()
            assert get(urls[i % 2]) == a
            samples.append((time.perf_counter() - started) * 1000)
        after = [snapshot(url) for url in admins]
        backend_stats = snapshot(backend_url + "/stats")
        expected = 2 if mode == "local" else 1
        assert backend_stats["calls"] == expected, backend_stats
        delta = lambda name: sum(n["counters"][name] - o["counters"][name] for n,o in zip(after,before))
        assert delta("incoming") == delta("completed") == delta("status_2xx") == 102
        assert delta("backend_attempts") == expected
        assert delta("dependency_errors") == 0
        assert delta("shared_hits") == ({"local": 0, "shared": 101, "layered": 1}[mode])
        assert delta("cache_hits") == (0 if mode == "shared" else 100)
        assert all(all(v == 0 for v in n["gauges"].values()) for n in after)
        return {"mode":mode, "trial":trial, "commands":commands, "before":before,
                "primed":primed, "after":after, "backend":backend_stats,
                "warm_requests":100, "warm_median_ms":statistics.median(samples),
                "warm_p95_ms":sorted(samples)[94], "samples_ms":samples}
    finally:
        for connection in CONNECTIONS.values():
            connection.close()
        CONNECTIONS.clear()
        for process in reversed(children):
            if process.poll() is None:
                process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        for log in logs:
            log.close()

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--redis-addr",default="127.0.0.1:6379")
    parser.add_argument("--trials",type=int,default=3)
    args=parser.parse_args()
    if not 1 <= args.trials <= 100:
        parser.error("trials must be 1..100")
    folder=ROOT/"bench/results/v2"/datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S-v24")
    folder.mkdir(parents=True)
    sources=list((ROOT/"internal").rglob("*.go"))+list((ROOT/"cmd").rglob("*.go"))+[ROOT/"go.mod",ROOT/"go.sum",Path(__file__)]
    manifest={"time":datetime.now(timezone.utc).isoformat(),"platform":platform.platform(),
              "python":platform.python_version(),"go":subprocess.check_output(["go","version"],text=True).strip(),
              "commit":subprocess.check_output(["git","rev-parse","HEAD"],cwd=ROOT,text=True).strip(),
              "dirty":bool(subprocess.check_output(["git","status","--porcelain"],cwd=ROOT,text=True)),
              "redis_addr":args.redis_addr,"trials":args.trials,
              "hashes":{str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sources},
              "binary_hashes":{str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in (ROOT/"bin").glob("*.exe") if p.stem in ("gateway","mockbackend")},
              "workload":"A fill, B reuse, 100 sequential alternating warm requests; fresh gateway/backend processes per trial"}
    (folder/"manifest.json").write_text(json.dumps(manifest,indent=2))
    with (folder/"trials.jsonl").open("w") as out:
        for mode in ("local","shared","layered"):
            for trial in range(1,args.trials+1):
                result=run(mode,trial,args,folder)
                out.write(json.dumps(result)+"\n")
                out.flush()
                print(mode,trial,"backend calls",result["backend"]["calls"],"median ms",round(result["warm_median_ms"],3),flush=True)
    (folder/"SUCCESS").write_text("All tier/counter assertions passed.\n")
    print(folder)

if __name__=="__main__":
    main()
