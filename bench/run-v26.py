"""Own an isolated Redis and exercise both gateway degradation policies.
Windows: --wsl Ubuntu --redis-server bin/redis-build/redis-server (Linux binary).
Linux: --redis-server /path/to/redis-server. Requires Redis 7.4.2 and Go binaries.
"""
import argparse
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
import uuid

ROOT=Path(__file__).resolve().parent.parent
SUFFIX=".exe" if os.name=="nt" else ""

class RedisError(RuntimeError):
    pass

def read_resp(stream):
    line=stream.readline(65536)
    if not line:
        raise EOFError("Redis closed connection")
    kind,payload=line[:1],line[1:-2]
    if kind==b"+":return payload.decode()
    if kind==b"-":raise RedisError(payload.decode())
    if kind==b":":return int(payload)
    if kind==b"$":
        length=int(payload)
        if length<0:return None
        if length>8*1024*1024:raise RuntimeError("control reply too large")
        value=stream.read(length)
        if len(value)!=length or stream.read(2)!=b"\r\n":raise EOFError("partial Redis reply")
        return value.decode()
    if kind==b"*":
        count=int(payload)
        if not 0<=count<=1024:raise RuntimeError("invalid array size")
        return [read_resp(stream) for _ in range(count)]
    raise RuntimeError("unexpected Redis response")

def redis(port,*args):
    encoded=[str(a).encode() for a in args]
    command=b"*"+str(len(encoded)).encode()+b"\r\n"+b"".join(b"$"+str(len(a)).encode()+b"\r\n"+a+b"\r\n" for a in encoded)
    with socket.create_connection(("127.0.0.1",port),timeout=3) as connection:
        connection.sendall(command)
        with connection.makefile("rb") as stream:
            return read_resp(stream)

def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1",0))
        return s.getsockname()[1]

def request(port,path="/healthz"):
    connection=http.client.HTTPConnection("127.0.0.1",port,timeout=5)
    started=time.monotonic()
    try:
        connection.request("GET",path)
        response=connection.getresponse()
        body=response.read().decode()
        return {"status":response.status,"body":body,"degraded":response.getheader("X-Gateway-Degraded", ""),"rejection":response.getheader("X-Gateway-Rejection", ""),"seconds":time.monotonic()-started}
    finally:
        connection.close()

def wait_until(fn,seconds=8):
    end=time.monotonic()+seconds
    while time.monotonic()<end:
        if fn():return
        time.sleep(.02)
    raise RuntimeError("condition deadline")

def snapshot(port,path="/metrics"):
    result=request(port,path);assert result["status"]==200,result
    return json.loads(result["body"])

class Topology:
    def __init__(self,folder,label,args,policy):
        self.folder=folder;self.label=label;self.args=args;self.policy=policy
        self.children=[];self.logs=[];self.commands=[];self.redis_process=None
        self.redis_port=port();self.backend=port();self.data=[port(),port()];self.admin=[port(),port()]
    def launch(self,command,name):
        log=open(self.folder/f"{self.label}-{name}-{len(self.children)}.log","w");self.logs.append(log)
        process=subprocess.Popen(list(map(str,command)),cwd=ROOT,stdout=log,stderr=log,env={**os.environ,"CACHE_ENABLED":"true","GORACE":"halt_on_error=1"})
        self.children.append(process);self.commands.append({"pid":process.pid,"argv":list(map(str,command))})
        return process
    def start_redis(self):
        config=self.folder/f"{self.label}-redis.conf"
        config.write_text(f'bind 127.0.0.1\nport {self.redis_port}\nprotected-mode yes\ndaemonize no\nsave ""\nappendonly no\nmaxmemory 64mb\nmaxmemory-policy noeviction\n',newline="\n")
        command=[self.args.redis_server,config.relative_to(ROOT).as_posix()]
        if self.args.wsl:
            command=["wsl","-d",self.args.wsl,"--",*command]
        self.redis_process=self.launch(command,"redis")
        def check():
            if self.redis_process.poll() is not None:raise RuntimeError("owned Redis exited")
            try:return redis(self.redis_port,"PING")=="PONG"
            except (OSError,EOFError):return False
        wait_until(check,15)
        info=redis(self.redis_port,"INFO","server")
        assert "redis_version:7.4.2" in info,info
        return info
    def stop_redis(self):
        if self.redis_process is None or self.redis_process.poll() is not None:return
        # WSL loopback forwarding may close a socket while reconnecting a newly
        # restarted listener. EOF alone does not prove this server shut down.
        deadline=time.monotonic()+10
        while self.redis_process.poll() is None and time.monotonic()<deadline:
            try:redis(self.redis_port,"SHUTDOWN","NOSAVE")
            except (EOFError,OSError):pass
            time.sleep(.05)
        self.redis_process.wait(timeout=1)
    def ready(self,port,process):
        def check():
            if process.poll() is not None:raise RuntimeError("child exited before readiness")
            try:return request(port)["status"]==200
            except (OSError,EOFError):return False
        wait_until(check)
    def gateway(self,i):
        command=[ROOT/"bin"/(("gateway-race" if self.args.race else "gateway")+SUFFIX),"-addr",f"127.0.0.1:{self.data[i]}","-admin-addr",f"127.0.0.1:{self.admin[i]}",
                 "-upstream",f"http://127.0.0.1:{self.backend}","-cache-mode","layered","-leases","-redis-addr",f"127.0.0.1:{self.redis_port}",
                 "-redis-timeout","50ms","-degradation",self.policy,"-instance",f"v26-{i}","-request-log-every","1",
                 "-lease-duration","1s","-lease-fill-timeout","600ms","-lease-wait","500ms","-lease-poll","10ms",
                 "-max-concurrent","16","-max-waiters","16","-max-cache-flights","16","-max-cache-waiters","128"]
        process=self.launch(command,f"gateway-{i}");self.ready(self.admin[i],process);return process
    def open(self):
        self.redis_info=self.start_redis()
        backend=self.launch([ROOT/"bin"/("mockbackend"+SUFFIX),"-addr",f"127.0.0.1:{self.backend}","-delay","50ms"],"backend");self.ready(self.backend,backend)
        self.gateways=[self.gateway(i) for i in range(2)]
        for p in self.data:assert request(p,"/api/public/item?id=42")["status"]==200
        assert snapshot(self.backend,"/stats")["calls"]==1
    def views(self):return [snapshot(p) for p in self.admin]
    def warm(self):
        before=self.views()
        results=[request(p,"/api/public/item?id=42") for p in self.data]
        after=self.views()
        assert all(r["status"]==200 and not r["degraded"] for r in results),results
        assert all(a["counters"]["dependency_errors"]==b["counters"]["dependency_errors"] for a,b in zip(after,before))
        return results
    def burst(self,key):
        command=[ROOT/"bin"/("burst"+SUFFIX),"-targets",",".join(f"http://127.0.0.1:{p}" for p in self.data),"-id",str(key),"-requests","100"]
        before=self.views();backend_before=snapshot(self.backend,"/stats")
        result=json.loads(subprocess.check_output(list(map(str,command)),cwd=ROOT,text=True,timeout=10))
        after=self.views();backend_after=snapshot(self.backend,"/stats")
        expected_status="200" if self.policy=="available-local" else "503"
        assert result["TransportErrors"]==0 and result["StatusCounts"]=={expected_status:100},result
        expected_calls=2 if self.policy=="available-local" else 0
        assert backend_after["calls"]-backend_before["calls"]==expected_calls,(backend_before,backend_after)
        for a,b in zip(after,before):
            assert a["counters"]["incoming"]-b["counters"]["incoming"]==50
            assert a["counters"]["completed"]-b["counters"]["completed"]==50
            assert all(v==0 for v in a["gauges"].values()),a
        if self.policy=="available-local":
            assert result["BodiesConsistent"] and result["DegradedReasons"],result
            assert sum(a["counters"]["degraded_flights"]-b["counters"]["degraded_flights"] for a,b in zip(after,before))==2
        else:assert result["RejectionReasons"],result
        return {"generator":result,"before":before,"after":after,"backend_before":backend_before,"backend_after":backend_after}
    def recover(self,key=44):
        before=snapshot(self.backend,"/stats")["calls"]
        attempts=[]
        # A closed pre-restart pooled connection may fail its first operation:
        # Redis retries are deliberately disabled. Retain every external probe.
        for data in self.data:
            node=[];deadline=time.monotonic()+2
            while time.monotonic()<deadline:
                result=request(data,f"/api/public/item?id={key}");node.append(result)
                if result["status"]==200:break
                assert result["status"]==503,result
                time.sleep(.05)
            assert node[-1]["status"]==200,node
            attempts.append(node)
        delta=snapshot(self.backend,"/stats")["calls"]-before
        assert 1<=delta<=(2 if self.policy=="available-local" else 1),delta
        # A fresh representation now proves both nodes have resumed shared reuse,
        # rather than merely serving L1 entries created during fallback.
        before=snapshot(self.backend,"/stats")["calls"]
        verified=[request(p,f"/api/public/item?id={key+1000}") for p in self.data]
        assert all(r["status"]==200 and not r["degraded"] for r in verified),verified
        assert snapshot(self.backend,"/stats")["calls"]==before+1
        return {"attempts":attempts,"initial_backend_calls":delta,"fresh_shared_verification":verified}
    def close(self):
        # Stop gateways before their externally owned dependency. All processes here
        # are owned by this topology; control commands never touch an existing Redis.
        for process in reversed(self.children):
            if process is self.redis_process:continue
            if process.poll() is None:process.terminate()
            try:process.wait(timeout=5)
            except subprocess.TimeoutExpired:process.kill();process.wait(timeout=5)
        try:self.stop_redis()
        finally:
            for process in self.children:
                if process.poll() is None:process.terminate();process.wait(timeout=5)
            for log in self.logs:log.close()

def experiment(topology,scenario):
    result={"warm_before":topology.views()}
    if scenario=="outage":
        topology.stop_redis();result["warm_during"]=topology.warm();result["fault"]=topology.burst(43)
        topology.start_redis();result["recovery"]=topology.recover()
    elif scenario=="slow":
        redis(topology.redis_port,"CLIENT","PAUSE",350,"ALL")
        result["warm_during"]=topology.warm();result["fault"]=topology.burst(43)
        assert redis(topology.redis_port,"PING")=="PONG";result["recovery"]=topology.recover()
    elif scenario=="memory":
        redis(topology.redis_port,"CONFIG","SET","maxmemory",2*1024*1024)
        keys=[];oom=False
        for i in range(128):
            key=f"v26:pressure:{i}"
            try:redis(topology.redis_port,"SET",key,"x"*32768,"PX",5000);keys.append(key)
            except RedisError as error:
                assert "OOM" in str(error),str(error);oom=True;break
        assert oom,"failed to reach Redis memory pressure"
        result["pressure_keys"]=len(keys);result["redis_memory"]=redis(topology.redis_port,"INFO","memory")
        # Saturate metadata headroom too, making an actual OOM admission failure
        # deterministic instead of depending on exact key/token allocation sizes.
        redis(topology.redis_port,"CONFIG","SET","maxmemory",1)
        result["warm_during"]=topology.warm();result["fault"]=topology.burst(43)
        redis(topology.redis_port,"CONFIG","SET","maxmemory",64*1024*1024)
        if keys:redis(topology.redis_port,"DEL",*keys)
        result["recovery"]=topology.recover()
    elif scenario=="restart":
        before_id=next(line for line in redis(topology.redis_port,"INFO","server").splitlines() if line.startswith("run_id:"))
        topology.stop_redis();after_info=topology.start_redis();after_id=next(line for line in after_info.splitlines() if line.startswith("run_id:"));assert before_id!=after_id
        result["warm_during"]=topology.warm()
        topology.gateways[0].kill();topology.gateways[0].wait(timeout=5)
        topology.gateways[0]=topology.gateway(0)
        result["empty_instance"]=snapshot(topology.admin[0]);assert result["empty_instance"]["cache_entries"]==0
        before=snapshot(topology.backend,"/stats")["calls"]
        response=request(topology.data[0],"/api/public/item?id=42");assert response["status"]==200 and not response["degraded"],response
        assert snapshot(topology.backend,"/stats")["calls"]==before+1
        result["restarted_response"]=response;result["recovery"]=topology.recover()
    elif scenario=="corrupt":
        raw=f'http://127.0.0.1:{topology.backend}/api/public/item?id=43|""|"identity"|""'
        key="gateway:response:v1:"+hashlib.sha256(raw.encode()).hexdigest()
        redis(topology.redis_port,"SET",key,"{corrupt", "PX",10000)
        before=topology.views();result["recovery"]=topology.recover(43);after=topology.views()
        assert sum(a["counters"]["shared_misses"]-b["counters"]["shared_misses"] for a,b in zip(after,before))>=1
        assert all(a["counters"]["dependency_errors"]==b["counters"]["dependency_errors"] for a,b in zip(after,before))
        assert redis(topology.redis_port,"GET",key)!="{corrupt","corrupt key not exercised"
    result["final"]=topology.views();result["backend"]=snapshot(topology.backend,"/stats")
    assert all(all(v==0 for v in s["gauges"].values()) for s in result["final"])
    assert result["backend"]["active"]==0 and result["backend"]["peak"]<=32
    result["redis_leases"]=redis(topology.redis_port,"KEYS","gateway:lease:v1:*")
    assert result["redis_leases"]==[],result["redis_leases"]
    return result

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--redis-server",default="bin/redis-build/redis-server")
    parser.add_argument("--wsl",default="Ubuntu" if os.name=="nt" else None)
    parser.add_argument("--trials",type=int,default=1)
    parser.add_argument("--race",action="store_true",help="use prebuilt bin/gateway-race[.exe] with halt_on_error")
    args=parser.parse_args()
    if not 1<=args.trials<=20:parser.error("trials must be 1..20")
    folder=ROOT/"bench/results/v2"/datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S-v26");folder.mkdir(parents=True)
    sources=list((ROOT/"internal").rglob("*.go"))+list((ROOT/"cmd").rglob("*.go"))+[ROOT/"go.mod",ROOT/"go.sum",Path(__file__)]
    binaries=[ROOT/"bin"/(n+SUFFIX) for n in (("gateway-race" if args.race else "gateway"),"mockbackend","burst")]
    manifest={"time":datetime.now(timezone.utc).isoformat(),"platform":platform.platform(),"python":platform.python_version(),"go":subprocess.check_output(["go","version"],text=True).strip(),
              "commit":subprocess.check_output(["git","rev-parse","HEAD"],cwd=ROOT,text=True).strip(),"dirty":bool(subprocess.check_output(["git","status","--porcelain"],cwd=ROOT,text=True)),
              "gateway_race":args.race,"gateway_build_info":subprocess.check_output(["go","version","-m",str(binaries[0])],text=True),"redis_server":args.redis_server,"wsl":args.wsl,"trials":args.trials,"redis_version":"7.4.2","redis_policy":"noeviction","redis_memory":64*1024*1024,
              "workload":"two gateways, 16 permits and queue slots each, 16 cache flights and 128 cache-miss clients each; 50ms backend; dependency timeout 50ms",
              "hashes":{str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sources+binaries}}
    (folder/"manifest.json").write_text(json.dumps(manifest,indent=2))
    summaries=[]
    with (folder/"trials.jsonl").open("w") as out:
        for policy in ("protection-first","available-local"):
            for scenario in ("outage","slow","memory","restart","corrupt"):
                for trial in range(1,args.trials+1):
                    topology=Topology(folder,f"{policy}-{scenario}-{trial}",args,policy)
                    try:
                        topology.open();result=experiment(topology,scenario)
                        row={"policy":policy,"scenario":scenario,"trial":trial,"commands":topology.commands,"redis_info":topology.redis_info,**result}
                        out.write(json.dumps(row)+"\n");out.flush()
                        summary={"policy":policy,"scenario":scenario,"trial":trial,"backend_calls":result["backend"]["calls"]}
                        if "fault" in result:
                            summary.update({"fault_status":result["fault"]["generator"]["StatusCounts"],"fault_seconds":result["fault"]["generator"]["ElapsedSeconds"],"fault_backend_calls":result["fault"]["backend_after"]["calls"]-result["fault"]["backend_before"]["calls"]})
                        summaries.append(summary);print(summary,flush=True)
                    except BaseException as error:
                        (folder/"FAILED").write_text(f"{policy}/{scenario}/{trial}: {error}\n");raise
                    finally:topology.close()
    (folder/"summary.json").write_text(json.dumps(summaries,indent=2));(folder/"SUCCESS").write_text("All isolated dependency fault assertions passed.\n")
    print(folder)

if __name__=="__main__":main()
