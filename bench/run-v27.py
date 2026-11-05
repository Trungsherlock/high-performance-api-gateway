"""Final separate-process comparison. --complete adds tests, faults and a clean-source check."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

import importlib.util
spec=importlib.util.spec_from_file_location("faults",Path(__file__).with_name("run-v26.py"))
faults=importlib.util.module_from_spec(spec);spec.loader.exec_module(faults)
from process_usage import process_usage
ROOT=Path(__file__).resolve().parent.parent
SUFFIX=".exe" if os.name=="nt" else ""
MODES=("local-one","local-two","shared","leased")

def identity():
    marker=ROOT/"clean-source.json"
    if marker.exists():return json.loads(marker.read_text())
    return {"kind":"working_tree","commit":subprocess.check_output(["git","rev-parse","HEAD"],cwd=ROOT,text=True).strip(),"dirty":bool(subprocess.check_output(["git","status","--porcelain"],cwd=ROOT,text=True))}

def info_fields(raw):
    return {k:v for line in raw.splitlines() if not line.startswith("#") and ":" in line for k,v in [line.split(":",1)]}

class Topology(faults.Topology):
    def __init__(self,folder,label,args,mode,delay):
        super().__init__(folder,label,args,"protection-first")
        self.mode=mode;self.delay=delay;self.roles={};self.front=faults.port();self.front_admin=faults.port()
        if mode=="local-one":self.data=self.data[:1];self.admin=self.admin[:1]
    def launch(self,command,name):
        process=super().launch(command,name);self.roles[process.pid]=name;return process
    def gateway(self,i):
        nodes=len(self.data)
        command=[ROOT/"bin"/("gateway"+SUFFIX),"-addr",f"127.0.0.1:{self.data[i]}","-admin-addr",f"127.0.0.1:{self.admin[i]}",
                 "-upstream",f"http://127.0.0.1:{self.backend}","-cache-mode","layered" if self.mode in ("shared","leased") else "local",
                 "-redis-addr",f"127.0.0.1:{self.redis_port}","-redis-timeout","50ms","-instance",f"v27-{i}","-request-log-every","1000",
                 "-max-concurrent",32//nodes,"-max-waiters",32//nodes,"-max-cache-flights",64//nodes,"-max-cache-waiters",1024//nodes]
        if self.mode=="leased":command+=["-leases"]
        process=self.launch(command,f"gateway-{i}");self.ready(self.admin[i],process);return process
    def open(self):
        self.redis_info=self.start_redis() if self.mode in ("shared","leased") else None
        backend=self.launch([ROOT/"bin"/("mockbackend"+SUFFIX),"-addr",f"127.0.0.1:{self.backend}","-delay",f"{self.delay}ms"],"backend");self.ready(self.backend,backend)
        self.gateways=[self.gateway(i) for i in range(len(self.data))]
        front=self.launch([ROOT/"bin"/("frontproxy"+SUFFIX),"-addr",f"127.0.0.1:{self.front}","-admin-addr",f"127.0.0.1:{self.front_admin}",
                           "-targets",",".join(f"http://127.0.0.1:{p}" for p in self.data),"-readiness",",".join(f"http://127.0.0.1:{p}/readyz" for p in self.admin),
                           "-probe-timeout","250ms","-timeout","12s"],"front")
        self.ready(self.front_admin,front)
    def sample(self):
        processes={}
        for process in self.children:
            role=self.roles[process.pid]
            if role=="redis" or process.poll() is not None:continue
            try:processes[role]={"pid":process.pid,**process_usage(process.pid)}
            except OSError as error:processes[role]={"pid":process.pid,"error":str(error)}
        cache=None
        if self.redis_process is not None:
            cpu=info_fields(faults.redis(self.redis_port,"INFO","cpu"));memory=info_fields(faults.redis(self.redis_port,"INFO","memory"));stats=info_fields(faults.redis(self.redis_port,"INFO","stats"))
            cache={"cpu_seconds":float(cpu["used_cpu_sys"])+float(cpu["used_cpu_user"]),"rss_bytes":int(memory["used_memory_rss"]),"used_memory":int(memory["used_memory"]),"commands":int(stats["total_commands_processed"]),"evicted_keys":int(stats["evicted_keys"])}
            processes["redis"]=cache
        return {"time":datetime.now(timezone.utc).isoformat(),"epoch":time.time(),"processes":processes,"gateways":self.views(),"front":faults.snapshot(self.front_admin),"backend":faults.snapshot(self.backend,"/stats")}
    def generate(self,kind,seconds,warmup,seed,resources):
        if kind=="cold":
            command=[ROOT/"bin"/("burst"+SUFFIX),"-targets",f"http://127.0.0.1:{self.front}","-requests","100"]
        else:
            command=[ROOT/"bin"/("loadgen"+SUFFIX),"-target",f"http://127.0.0.1:{self.front}","-admins",",".join(f"http://127.0.0.1:{p}" for p in self.admin),
                     "-front-admin",f"http://127.0.0.1:{self.front_admin}","-backend",f"http://127.0.0.1:{self.backend}","-mode","scheduled","-workload","mixed",
                     "-duration",f"{seconds}s","-warmup",f"{warmup}s","-rate",self.args.rate,"-concurrency","256","-seed",seed]
        process=self.launch(command,"generator");began=time.monotonic();next_sample=0
        with resources.open("w") as out:
            while process.poll() is None:
                if time.monotonic()-began>seconds+warmup+20:raise RuntimeError("generator watchdog expired")
                if time.monotonic()>=next_sample:
                    out.write(json.dumps(self.sample())+"\n");out.flush();next_sample=time.monotonic()+1
                time.sleep(.05)
        assert process.returncode==0,("generator exit",process.returncode)
        return [json.loads(line) for line in Path(self.logs[-1].name).read_text().splitlines() if line.strip()]

def run_trial(folder,args,mode,kind,trial):
    topology=Topology(folder,f"{mode}-{kind}-{trial}"+(f"-attempt-{args.attempt}" if args.attempt else ""),args,mode,500 if kind=="cold" else 50)
    try:
        topology.open();before={"gateways":topology.views(),"backend":faults.snapshot(topology.backend,"/stats"),"front":faults.snapshot(topology.front_admin)}
        seconds=args.soak if kind=="soak" else args.seconds
        resources=folder/f"{topology.label}-resources.jsonl"
        output=topology.generate(kind,seconds,0 if kind=="cold" else args.warmup,42+trial,resources)
        after={"gateways":topology.views(),"backend":faults.snapshot(topology.backend,"/stats"),"front":faults.snapshot(topology.front_admin)}
        assert all(all(v==0 for v in n["gauges"].values()) for n in after["gateways"])
        assert after["backend"]["active"]==0 and after["backend"]["peak"]<=32
        return {"mode":mode,"kind":kind,"trial":trial,"commands":topology.commands,"before":before,"after":after,"output":output,"resources":resources.name,"redis_info":topology.redis_info}
    finally:topology.close()

def build(folder):
    for name in ("gateway","mockbackend","frontproxy","loadgen","burst"):
        command=["go","build","-o",str(ROOT/"bin"/(name+SUFFIX)),f"./cmd/{name}"]
        subprocess.run(command,cwd=ROOT,check=True)

def verify(folder,args):
    topology=faults.Topology(folder,"checks",args,"protection-first")
    try:
        topology.start_redis();environment={**os.environ,"REDIS_TEST_ADDR":f"127.0.0.1:{topology.redis_port}"}
        if os.name=="nt" and Path("C:/msys64/mingw64/bin/gcc.exe").exists():environment["PATH"]="C:/msys64/mingw64/bin;"+environment["PATH"]
        for name,command in [("tests",["go","test","./...","-count=1","-timeout","60s"]),("race",["go","test","-race","./...","-count=1","-timeout","60s"]),("vet",["go","vet","./..."]),("build",["go","build","./..."])]:
            with (folder/f"{name}.txt").open("w") as out:subprocess.run(command,cwd=ROOT,env=environment,stdout=out,stderr=subprocess.STDOUT,check=True)
    finally:topology.close()

def clean_check(folder,args):
    destination=ROOT/"bin"/("v27-clean-"+datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S"));destination.mkdir(parents=True)
    for name in ("cmd","internal"):shutil.copytree(ROOT/name,destination/name)
    for name in ("go.mod","go.sum"):shutil.copy2(ROOT/name,destination/name)
    (destination/"bench").mkdir()
    for source in (ROOT/"bench").glob("*.py"):shutil.copy2(source,destination/"bench"/source.name)
    files={str(p.relative_to(destination)):hashlib.sha256(p.read_bytes()).hexdigest() for p in destination.rglob("*") if p.is_file()}
    (destination/"clean-source.json").write_text(json.dumps({"kind":"source_only_snapshot","parent":identity(),"files_sha256":files,"note":"No prebuilt Go binaries or historical results copied; normal content-addressed Go build cache allowed."},indent=2))
    server=args.redis_server
    if not Path(server).is_absolute():
        server=(ROOT/server).as_posix()
        if os.name=="nt":server="/mnt/"+server[0].lower()+server[2:]
    command=[sys.executable,str(destination/"bench/run-v27.py"),"--quick","--redis-server",server]
    if args.wsl:command += ["--wsl",args.wsl]
    with (folder/"clean-source.log").open("w") as out:subprocess.run(command,cwd=destination,stdout=out,stderr=subprocess.STDOUT,check=True)
    runs=sorted((destination/"bench/results/v2").glob("*-v27"));assert len(runs)==1 and (runs[0]/"SUCCESS").exists()
    shutil.copytree(runs[0],folder/"clean-source-results")
    shutil.copy2(destination/"clean-source.json",folder/"clean-source.json")

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--redis-server",default="bin/redis-build/redis-server");parser.add_argument("--wsl",default="Ubuntu" if os.name=="nt" else None)
    parser.add_argument("--seconds",type=int,default=30);parser.add_argument("--warmup",type=int,default=5);parser.add_argument("--trials",type=int,default=3)
    parser.add_argument("--cold-trials",type=int,default=3);parser.add_argument("--soak",type=int,default=300);parser.add_argument("--rate",type=int,default=500)
    parser.add_argument("--quick",action="store_true");parser.add_argument("--skip-build",action="store_true");parser.add_argument("--complete",action="store_true")
    parser.add_argument("--resume",type=Path,help="resume an interrupted artifact directory without repeating completed trials")
    args=parser.parse_args();args.race=False;args.attempt=0
    if args.quick:args.seconds=2;args.warmup=1;args.trials=1;args.cold_trials=1;args.soak=0
    if args.seconds<1 or args.warmup<0 or not 1<=args.trials<=20 or not 0<=args.cold_trials<=20 or args.soak<0 or args.rate<1:parser.error("invalid workload settings")
    folder=args.resume.resolve() if args.resume else ROOT/"bench/results/v2"/datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S-v27")
    if not folder.is_relative_to(ROOT/"bench/results/v2"):parser.error("artifact directory must be inside this checkout's bench/results/v2")
    if not args.resume:folder.mkdir(parents=True)
    completed=set()
    if args.resume:
        original=json.loads((folder/"manifest.json").read_text())
        for key in ("seconds","warmup","trials","cold_trials","soak","rate","redis_server","wsl","complete"):
            if original["settings"][key]!=getattr(args,key):parser.error(f"resume setting differs: {key}")
        for name,digest in original["hashes"].items():
            if name.endswith((".go",".exe")) or name in ("go.mod","go.sum"):
                if hashlib.sha256((ROOT/name).read_bytes()).hexdigest()!=digest:parser.error(f"resume application/binary hash differs: {name}")
        entries=[json.loads(line) for line in (folder/"trials.jsonl").read_text().splitlines()]
        completed={(r["mode"],r["kind"],r["trial"]) for r in entries}
        if len(completed)!=len(entries):parser.error("duplicate recorded trial")
        args.attempt=len(list(folder.glob("resume-*.json")))+1
        (folder/f"resume-{args.attempt}.json").write_text(json.dumps({"time":datetime.now(timezone.utc).isoformat(),"completed_trials_preserved":len(completed),"note":"Readiness HTTP parsing retry and distinct fixture ports; no application/binary changes. Original FAILED marker/logs retained.","harness_hashes":{str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in (ROOT/"bench").glob("*.py")}},indent=2))
    try:
        if not args.skip_build:build(folder)
        sources=list((ROOT/"cmd").rglob("*.go"))+list((ROOT/"internal").rglob("*.go"))+list((ROOT/"bench").glob("*.py"))+[ROOT/"go.mod",ROOT/"go.sum"]
        binaries=[ROOT/"bin"/(name+SUFFIX) for name in ("gateway","mockbackend","frontproxy","loadgen","burst")]
        manifest={"source":identity(),"time":datetime.now(timezone.utc).isoformat(),"platform":sys.platform,"host":__import__("platform").platform(),"python":sys.version,"cpu_count":os.cpu_count(),"go":subprocess.check_output(["go","version"],text=True).strip(),"settings":{k:str(v) if isinstance(v,Path) else v for k,v in vars(args).items()},
                  "aggregate_permits":32,"aggregate_permit_queue":32,"aggregate_cache_flights":64,"aggregate_cache_waiters":1024,"redis_version":"7.4.2","redis_maxmemory":67108864,"redis_policy":"noeviction","hashes":{str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sources+binaries}}
        if not args.resume:(folder/"manifest.json").write_text(json.dumps(manifest,indent=2))
        with (folder/"trials.jsonl").open("a" if args.resume else "w") as out:
            for trial in range(1,args.trials+1):
                # Rotate order while preserving the same seed/offered workload within each block.
                modes=MODES[(trial-1)%4:]+MODES[:(trial-1)%4]
                for mode in modes:
                    if (mode,"steady",trial) in completed:continue
                    row=run_trial(folder,args,mode,"steady",trial);out.write(json.dumps(row)+"\n");out.flush();print(mode,"steady",trial,flush=True)
            for trial in range(1,args.cold_trials+1):
                for mode in MODES:
                    if (mode,"cold",trial) in completed:continue
                    row=run_trial(folder,args,mode,"cold",trial);out.write(json.dumps(row)+"\n");out.flush();print(mode,"cold",trial,flush=True)
            if args.soak and ("leased","soak",1) not in completed:
                row=run_trial(folder,args,"leased","soak",1);out.write(json.dumps(row)+"\n");out.flush();print("leased soak complete",flush=True)
        subprocess.run([sys.executable,str(ROOT/"bench/summarize-v27.py"),str(folder)],cwd=ROOT,check=True)
        if args.complete:
            verify(folder,args)
            command=[sys.executable,str(ROOT/"bench/run-v26.py"),"--redis-server",args.redis_server]
            if args.wsl:command += ["--wsl",args.wsl]
            with (folder/"fault-reproduction.log").open("w") as out:subprocess.run(command,cwd=ROOT,stdout=out,stderr=subprocess.STDOUT,check=True)
            clean_check(folder,args)
        (folder/"SUCCESS").write_text("Comparison assertions and requested checks passed.\n");print(folder,flush=True)
    except BaseException as error:
        (folder/"FAILED").write_text(str(error)+"\n");raise

if __name__=="__main__":main()
