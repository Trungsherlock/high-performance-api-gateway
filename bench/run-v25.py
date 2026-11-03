"""Cold burst and killed-owner experiments using isolated gateway/backend processes.
Requires an isolated running Redis 7.4.2 and built gateway, mockbackend, burst binaries.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import http.client
import json
import os
from pathlib import Path
import platform
import socket
import subprocess
import time

ROOT=Path(__file__).resolve().parent.parent
SUFFIX=".exe" if os.name=="nt" else ""

def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1",0))
        return s.getsockname()[1]

def request(port,path="/healthz"):
    connection=http.client.HTTPConnection("127.0.0.1",port,timeout=6)
    try:
        connection.request("GET",path)
        response=connection.getresponse()
        return response.status,response.read().decode()
    finally:
        connection.close()

def stats(port,path="/metrics"):
    status,body=request(port,path)
    assert status==200,(status,body)
    return json.loads(body)

def wait_until(fn,seconds=5):
    deadline=time.monotonic()+seconds
    while time.monotonic()<deadline:
        if fn():
            return
        time.sleep(.01)
    raise RuntimeError("condition deadline")

class Topology:
    def __init__(self,folder,scenario,trial,args):
        self.children=[];self.logs=[];self.commands=[]
        self.folder=folder;self.label=f"{scenario}-{trial}";self.args=args
    def start(self,name,*flags):
        command=[str(ROOT/"bin"/(name+SUFFIX)),*map(str,flags)]
        log=open(self.folder/f"{self.label}-{name}-{len(self.children)}.log","w")
        self.logs.append(log)
        process=subprocess.Popen(command,stdout=log,stderr=log,cwd=ROOT,env={**os.environ,"CACHE_ENABLED":"true"})
        self.children.append(process);self.commands.append({"pid":process.pid,"argv":command})
        return process
    def ready(self,port,process):
        def check():
            if process.poll() is not None:
                raise RuntimeError("child exited before readiness")
            try:
                return request(port)[0]==200
            except OSError:
                return False
        wait_until(check,10)
    def open(self,leases):
        self.backend=port();backend=self.start("mockbackend","-addr",f"127.0.0.1:{self.backend}","-delay","500ms")
        self.ready(self.backend,backend)
        self.data=[];self.admin=[];self.gateways=[]
        for i in range(2):
            data,admin=port(),port();self.data.append(data);self.admin.append(admin)
            flags=["-addr",f"127.0.0.1:{data}","-admin-addr",f"127.0.0.1:{admin}","-upstream",f"http://127.0.0.1:{self.backend}",
                   "-cache-mode","layered","-redis-addr",self.args.redis_addr,"-redis-timeout","100ms",
                   "-instance",f"v25-{i}","-request-log-every","0","-max-concurrent","16","-max-waiters","16",
                   "-upstream-timeout","5s"]
            if leases:
                flags += ["-leases","-lease-duration","1s","-lease-fill-timeout","600ms","-lease-wait","2s","-lease-poll","10ms"]
            process=self.start("gateway",*flags);self.gateways.append(process);self.ready(admin,process)
        return self
    def close(self):
        for process in reversed(self.children):
            if process.poll() is None:
                process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill();process.wait(timeout=5)
        for log in self.logs:
            log.close()

def drained(snapshot):
    return all(value==0 for value in snapshot["gauges"].values())

def burst(topology,leases):
    command=[str(ROOT/"bin"/("burst"+SUFFIX)),"-targets",",".join(f"http://127.0.0.1:{p}" for p in topology.data),"-requests","100"]
    raw=subprocess.check_output(command,cwd=ROOT,text=True,timeout=10)
    result=json.loads(raw)
    snapshots=[stats(p) for p in topology.admin]
    backend=stats(topology.backend,"/stats")
    assert result["StatusCounts"]=={"200":100} and result["TransportErrors"]==0 and result["BodiesConsistent"],result
    expected=1 if leases else 2
    assert backend["calls"]==expected,backend
    assert sum(s["counters"]["backend_attempts"] for s in snapshots)==expected
    assert all(s["counters"]["incoming"]==s["counters"]["completed"]==s["counters"]["status_2xx"]==50 for s in snapshots)
    assert all(s["counters"]["dependency_errors"]==0 and drained(s) for s in snapshots)
    if leases:
        assert sum(s["counters"]["lease_publications"] for s in snapshots)==1
        assert sum(s["counters"]["distributed_followers"] for s in snapshots)>=1
    return {"generator_command":command,"burst":result,"gateways":snapshots,"backend":backend}

def crash(topology):
    def owner_request():
        try:
            status,body=request(topology.data[0],"/api/public/item?id=42")
            return {"status":status,"body":body}
        except OSError as err:
            return {"transport_error":type(err).__name__}
    with ThreadPoolExecutor(max_workers=1) as executor:
        owner=executor.submit(owner_request)
        wait_until(lambda:stats(topology.backend,"/stats")["calls"]==1)
        at_kill=stats(topology.admin[0])
        assert at_kill["counters"]["lease_acquired"]==1 and at_kill["counters"]["lease_publications"]==0
        topology.gateways[0].kill();topology.gateways[0].wait(timeout=5)
        started=time.monotonic()
        status,body=request(topology.data[1],"/api/public/item?id=42")
        elapsed=time.monotonic()-started
        owner_result=owner.result(timeout=5)
    assert status==200 and elapsed<3.5,(status,body,elapsed)
    survivor=stats(topology.admin[1]);backend=stats(topology.backend,"/stats")
    assert "transport_error" in owner_result,owner_result
    assert backend["calls"]==2 and backend["active"]==0,backend
    assert survivor["counters"]["lease_acquired"]==survivor["counters"]["lease_publications"]==1
    assert survivor["counters"]["distributed_followers"]==1 and survivor["counters"]["dependency_errors"]==0
    assert drained(survivor),survivor
    return {"owner_at_kill":at_kill,"owner_result":owner_result,"survivor":survivor,"backend":backend,"survivor_seconds_after_kill":elapsed}

def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument("--redis-addr",default="127.0.0.1:6379");parser.add_argument("--trials",type=int,default=3)
    args=parser.parse_args()
    if not 1<=args.trials<=100:
        parser.error("trials must be 1..100")
    folder=ROOT/"bench/results/v2"/datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S-v25");folder.mkdir(parents=True)
    sources=list((ROOT/"internal").rglob("*.go"))+list((ROOT/"cmd").rglob("*.go"))+[ROOT/"go.mod",ROOT/"go.sum",Path(__file__)]
    binaries=[ROOT/"bin"/(n+SUFFIX) for n in ("gateway","mockbackend","burst")]
    manifest={"time":datetime.now(timezone.utc).isoformat(),"platform":platform.platform(),"python":platform.python_version(),"go":subprocess.check_output(["go","version"],text=True).strip(),
              "commit":subprocess.check_output(["git","rev-parse","HEAD"],cwd=ROOT,text=True).strip(),"dirty":bool(subprocess.check_output(["git","status","--porcelain"],cwd=ROOT,text=True)),
              "redis_addr":args.redis_addr,"trials":args.trials,"backend_delay_ms":500,"redis_operation_ms":100,"lease_ms":1000,"owner_fill_ms":600,"wait_ms":2000,"poll_base_ms":10,
              "aggregate_permits":32,"aggregate_queue_slots":32,"hashes":{str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sources+binaries}}
    (folder/"manifest.json").write_text(json.dumps(manifest,indent=2))
    results=[]
    with (folder/"trials.jsonl").open("w") as out:
        for scenario in ("shared-burst","leased-burst","owner-crash"):
            for trial in range(1,args.trials+1):
                topology=Topology(folder,scenario,trial,args)
                try:
                    topology.open(scenario!="shared-burst")
                    result=crash(topology) if scenario=="owner-crash" else burst(topology,scenario=="leased-burst")
                    row={"scenario":scenario,"trial":trial,"commands":topology.commands,**result}
                    results.append(row);out.write(json.dumps(row)+"\n");out.flush()
                    print(scenario,trial,"backend calls",result["backend"]["calls"],flush=True)
                except BaseException as error:
                    (folder/"FAILED").write_text(f"{scenario} trial {trial}: {error}\n")
                    raise
                finally:
                    topology.close()
    summary={scenario:[{"backend_calls":r["backend"]["calls"],"seconds":r.get("survivor_seconds_after_kill",r.get("burst",{}).get("ElapsedSeconds"))} for r in results if r["scenario"]==scenario] for scenario in ("shared-burst","leased-burst","owner-crash")}
    (folder/"summary.json").write_text(json.dumps(summary,indent=2));(folder/"SUCCESS").write_text("All burst and killed-owner assertions passed.\n")
    print(folder)

if __name__=="__main__":
    main()
