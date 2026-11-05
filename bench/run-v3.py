"""V3 separate-process comparison. Modes, workload, offered rates, backend timing and
gateway count are parameters; --complete adds tests, the V2.6 faults and a clean-source check.
local-two-aa repeats local-two in every block as an A/A noise control, and --affinity can pin the
generator, gateways and backend+front to disjoint logical-CPU sets. Each run first writes
predictions.json from the generator's planned key stream (predict-v3.py); --restart-trials adds
runs in which gateway 1 stops mid-measurement and returns with an empty L1."""
import argparse
from datetime import datetime, timezone
import hashlib
import http.client
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

# The frozen V2.7 harness supplies builds, verification and source identity; sharing
# its fault-fixture module keeps one distinct-port allocator for the whole run.
spec=importlib.util.spec_from_file_location("final",Path(__file__).with_name("run-v27.py"))
final=importlib.util.module_from_spec(spec);spec.loader.exec_module(final);faults=final.faults
spec=importlib.util.spec_from_file_location("predict",Path(__file__).with_name("predict-v3.py"))
predict=importlib.util.module_from_spec(spec);spec.loader.exec_module(predict)
from process_usage import process_usage
import host
import pack_logs
ROOT=Path(__file__).resolve().parent.parent
SUFFIX=".exe" if os.name=="nt" else ""
MODES=("local-one","local-two","shared","leased","leased-v2","local-two-aa")
REDIS_MODES=("shared","leased","leased-v2")
# leased keeps V2.7's protocol; leased-v2 runs the V3.2 read-or-acquire protocol in the same blocks.
LEASE_PROTOCOL={"leased":"v1","leased-v2":"v2"}
WORKLOADS=("hot","mixed","unique","zipf","hotset")
# Restart trials need a second gateway and skip the A/A copy.
RESTART_MODES=("local-two","shared","leased","leased-v2")
# Aggregate budgets are split evenly across gateways, matching V2.7.
BUDGET={"aggregate_permits":32,"aggregate_permit_queue":32,"aggregate_cache_flights":64,"aggregate_cache_waiters":1024,
        "cache_entries_per_node":1000,"cache_payload_bytes_per_node":16*2**20,"redis_timeout_ms":50,"redis_maxmemory":64*2**20,"redis_policy":"noeviction"}
BINARIES=("gateway","mockbackend","frontproxy","loadgen","burst")
SETTINGS=("seconds","warmup","trials","cold_trials","soak","soak_mode","rates","modes","workload","gateways","delay","cold_delay","max_age","concurrency","redis_server","wsl","complete","affinity","keys","zipf_s","hot_keys","cache_entries","restart_trials")

class Topology(faults.Topology):
    def __init__(self,folder,label,args,mode,delay):
        super().__init__(folder,label,args,"protection-first")
        self.mode=mode;self.delay=delay;self.roles={};self.front=faults.port();self.front_admin=faults.port()
        nodes=1 if mode=="local-one" else args.gateways
        self.data=[faults.port() for _ in range(nodes)];self.admin=[faults.port() for _ in range(nodes)]
        self.restarting=False;self.events=[]
    def views(self):
        out=[]
        for port in self.admin:
            try:out.append(faults.snapshot(port))
            except (OSError,EOFError,http.client.HTTPException):
                # Only a restart trial may find a gateway down.
                if not self.restarting:raise
                out.append({"error":"unreachable"})
        return out
    def restart(self,action):
        """One step of the restart scenario: gateway 1 stops, then returns with an empty L1."""
        event={"event":action,"epoch":time.time()}
        if action=="stop":
            event["backend_calls"]=faults.snapshot(self.backend,"/stats")["calls"]
            self.gateways[1].kill();self.gateways[1].wait(timeout=5);event["stopped_epoch"]=time.time()
        elif action=="start":
            self.gateways[1]=self.gateway(1);event["epoch"]=time.time()
            event["backend_calls"]=faults.snapshot(self.backend,"/stats")["calls"];event["cache_entries"]=faults.snapshot(self.admin[1])["cache_entries"]
        else:event["backend_calls"]=faults.snapshot(self.backend,"/stats")["calls"]
        return event
    def launch(self,command,name):
        # V2.6 launch plus CPU pinning: Redis runs in the WSL2 VM and cannot be pinned this way.
        group="generator" if name=="generator" else "gateways" if name.startswith("gateway") else "support" if name in ("backend","front") else None
        cpus=self.args.layout["groups"][group] if self.args.layout and group else None
        env={**os.environ,"CACHE_ENABLED":"true","GORACE":"halt_on_error=1"}
        # Go sizes GOMAXPROCS before affinity is applied on Windows, so set it explicitly.
        if cpus:env["GOMAXPROCS"]=str(len(cpus))
        log=open(self.folder/f"{self.label}-{name}-{len(self.children)}.log","w");self.logs.append(log)
        process=subprocess.Popen(list(map(str,command)),cwd=ROOT,stdout=log,stderr=log,env=env,**host.launch_options(cpus))
        self.children.append(process);self.roles[process.pid]=name
        entry={"pid":process.pid,"argv":list(map(str,command))}
        if cpus:
            entry.update(group=group,cpus=host.pin(process.pid,cpus),gomaxprocs=len(cpus));assert entry["cpus"]==cpus,(name,entry["cpus"],cpus)
        self.commands.append(entry);return process
    def gateway(self,i):
        nodes=len(self.data);budget=self.args.budget
        command=[ROOT/"bin"/("gateway"+SUFFIX),"-addr",f"127.0.0.1:{self.data[i]}","-admin-addr",f"127.0.0.1:{self.admin[i]}",
                 "-upstream",f"http://127.0.0.1:{self.backend}","-cache-mode","layered" if self.mode in REDIS_MODES else "local",
                 "-redis-addr",f"127.0.0.1:{self.redis_port}","-redis-timeout",f"{budget['redis_timeout_ms']}ms","-instance",f"v3-{i}","-request-log-every","1000",
                 "-max-concurrent",budget["aggregate_permits"]//nodes,"-max-waiters",budget["aggregate_permit_queue"]//nodes,
                 "-max-cache-flights",budget["aggregate_cache_flights"]//nodes,"-max-cache-waiters",budget["aggregate_cache_waiters"]//nodes,
                 "-cache-entries",budget["cache_entries_per_node"],"-cache-bytes",budget["cache_payload_bytes_per_node"]]
        if self.mode in LEASE_PROTOCOL:command+=["-leases","-lease-protocol",LEASE_PROTOCOL[self.mode]]
        process=self.launch(command,f"gateway-{i}");self.ready(self.admin[i],process);return process
    def open(self):
        self.redis_info=self.start_redis() if self.mode in REDIS_MODES else None
        backend=self.launch([ROOT/"bin"/("mockbackend"+SUFFIX),"-addr",f"127.0.0.1:{self.backend}","-delay",f"{self.delay}ms","-max-age",self.args.max_age],"backend");self.ready(self.backend,backend)
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
        if self.redis_process is not None:
            cpu=final.info_fields(faults.redis(self.redis_port,"INFO","cpu"));memory=final.info_fields(faults.redis(self.redis_port,"INFO","memory"));stats=final.info_fields(faults.redis(self.redis_port,"INFO","stats"))
            processes["redis"]={"cpu_seconds":float(cpu["used_cpu_sys"])+float(cpu["used_cpu_user"]),"rss_bytes":int(memory["used_memory_rss"]),"used_memory":int(memory["used_memory"]),"commands":int(stats["total_commands_processed"]),"evicted_keys":int(stats["evicted_keys"])}
        return {"time":datetime.now(timezone.utc).isoformat(),"epoch":time.time(),"processes":processes,"gateways":self.views(),"front":faults.snapshot(self.front_admin),"backend":faults.snapshot(self.backend,"/stats")}
    def generate(self,kind,seconds,warmup,seed,rate,resources):
        if kind=="cold":
            command=[ROOT/"bin"/("burst"+SUFFIX),"-targets",f"http://127.0.0.1:{self.front}","-requests","100"]
        else:
            command=[ROOT/"bin"/("loadgen"+SUFFIX),"-target",f"http://127.0.0.1:{self.front}","-admins",",".join(f"http://127.0.0.1:{p}" for p in self.admin),
                     "-front-admin",f"http://127.0.0.1:{self.front_admin}","-backend",f"http://127.0.0.1:{self.backend}","-mode","scheduled","-workload",self.args.workload,
                     "-keys",self.args.keys,"-zipf-s",self.args.zipf_s,"-hot-keys",self.args.hot_keys,
                     "-duration",f"{seconds}s","-warmup",f"{warmup}s","-rate",rate,"-concurrency",self.args.concurrency,"-seed",seed]
        # A restart launches a gateway after the generator, so keep the generator's own log.
        process=self.launch(command,"generator");log=self.logs[-1];began=time.monotonic();next_sample=0
        # Offsets from the generator's start; the summary aligns recorded epochs to the measurement start.
        schedule=[(warmup+5,"pre"),(warmup+predict.RESTART["stop"],"stop"),(warmup+predict.RESTART["start"],"start")] if kind=="restart" else []
        with resources.open("w") as out:
            while process.poll() is None:
                if time.monotonic()-began>seconds+warmup+20:raise RuntimeError("generator watchdog expired")
                if schedule and time.monotonic()-began>=schedule[0][0]:
                    _,action=schedule.pop(0);self.events.append(self.restart(action))
                    if action=="start":
                        ready=time.monotonic()-began;schedule+=[(ready+2,"early"),(ready+10,"post")]
                if time.monotonic()>=next_sample:
                    out.write(json.dumps(self.sample())+"\n");out.flush();next_sample=time.monotonic()+1
                time.sleep(.05)
        assert process.returncode==0,("generator exit",process.returncode)
        assert not schedule,("restart steps never ran",schedule)
        return [json.loads(line) for line in Path(log.name).read_text().splitlines() if line.strip()]

def run_trial(folder,args,mode,kind,rate,trial):
    label=f"{mode}-cold-{trial}" if kind=="cold" else f"{mode}-{kind}-r{rate}-{trial}"
    if args.attempt:label+=f"-attempt-{args.attempt}"
    topology=Topology(folder,label,args,mode,args.cold_delay if kind=="cold" else args.delay);topology.restarting=kind=="restart"
    try:
        power=host.power();topology.open();before={"gateways":topology.views(),"backend":faults.snapshot(topology.backend,"/stats"),"front":faults.snapshot(topology.front_admin)}
        seconds=args.soak if kind=="soak" else predict.RESTART["seconds"] if kind=="restart" else args.seconds
        resources=folder/f"{topology.label}-resources.jsonl"
        output=topology.generate(kind,seconds,0 if kind=="cold" else args.warmup,42+trial,rate,resources)
        after={"gateways":topology.views(),"backend":faults.snapshot(topology.backend,"/stats"),"front":faults.snapshot(topology.front_admin)}
        assert all(all(v==0 for v in n["gauges"].values()) for n in after["gateways"])
        assert after["backend"]["active"]==0 and after["backend"]["peak"]<=args.budget["aggregate_permits"]
        return {"mode":mode,"kind":kind,"rate":rate,"trial":trial,"gateways":len(topology.data),"commands":topology.commands,"before":before,"after":after,"output":output,"resources":resources.name,"redis_info":topology.redis_info,"power":power,"events":topology.events}
    finally:topology.close()

def fault_runs(folder,args):
    """The ten V2.6 fault cases: protocol v1 as the regression reference, then v2 with
    ordinary and race-detector gateways."""
    environment=dict(os.environ)
    if os.name=="nt" and Path("C:/msys64/mingw64/bin/gcc.exe").exists():environment["PATH"]="C:/msys64/mingw64/bin;"+environment["PATH"]
    with (folder/"race-build.txt").open("w") as out:
        subprocess.run(["go","build","-race","-o",str(ROOT/"bin"/("gateway-race"+SUFFIX)),"./cmd/gateway"],cwd=ROOT,env=environment,stdout=out,stderr=subprocess.STDOUT,check=True)
    for protocol,race in (("v1",False),("v2",False),("v2",True)):
        name=protocol+("-race" if race else "")
        command=[sys.executable,str(ROOT/"bench/run-v26.py"),"--redis-server",args.redis_server,"--results",str((folder/"faults"/name).relative_to(ROOT)),"--lease-protocol",protocol]
        if race:command+=["--race"]
        if args.wsl:command+=["--wsl",args.wsl]
        with (folder/f"fault-reproduction-{name}.log").open("w") as out:subprocess.run(command,cwd=ROOT,stdout=out,stderr=subprocess.STDOUT,check=True)

def clean_check(folder,args):
    destination=ROOT/"bin"/("v3-clean-"+datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S"));destination.mkdir(parents=True)
    for name in ("cmd","internal"):shutil.copytree(ROOT/name,destination/name)
    for name in ("go.mod","go.sum"):shutil.copy2(ROOT/name,destination/name)
    (destination/"bench").mkdir()
    for source in (ROOT/"bench").glob("*.py"):shutil.copy2(source,destination/"bench"/source.name)
    files={str(p.relative_to(destination)):hashlib.sha256(p.read_bytes()).hexdigest() for p in destination.rglob("*") if p.is_file()}
    (destination/"clean-source.json").write_text(json.dumps({"kind":"source_only_snapshot","parent":final.identity(),"files_sha256":files,"note":"No prebuilt Go binaries or historical results copied; normal content-addressed Go build cache allowed."},indent=2))
    server=args.redis_server
    if not Path(server).is_absolute():
        server=(ROOT/server).as_posix()
        if os.name=="nt":server="/mnt/"+server[0].lower()+server[2:]
    # Same topology and workload as the parent run, at smoke-test duration.
    command=[sys.executable,str(destination/"bench/run-v3.py"),"--quick","--redis-server",server,"--modes",",".join(args.modes),"--workload",args.workload,
             "--rates",",".join(map(str,args.rates)),"--gateways",str(args.gateways),"--delay",str(args.delay),"--cold-delay",str(args.cold_delay),"--max-age",str(args.max_age),"--concurrency",str(args.concurrency),"--affinity",args.affinity,
             "--keys",str(args.keys),"--zipf-s",str(args.zipf_s),"--hot-keys",str(args.hot_keys),"--cache-entries",str(args.cache_entries)]
    if args.wsl:command+=["--wsl",args.wsl]
    with (folder/"clean-source.log").open("w") as out:subprocess.run(command,cwd=destination,stdout=out,stderr=subprocess.STDOUT,check=True)
    runs=sorted((destination/"bench/results/v3").glob("*-v3"));assert len(runs)==1 and (runs[0]/"SUCCESS").exists()
    shutil.copytree(runs[0],folder/"clean-source-results")
    shutil.copy2(destination/"clean-source.json",folder/"clean-source.json")

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--redis-server",default="bin/redis-build/redis-server");parser.add_argument("--wsl",default="Ubuntu" if os.name=="nt" else None)
    parser.add_argument("--modes",default=",".join(MODES),help="comma-separated subset of "+", ".join(MODES)+"; local-two-aa needs local-two")
    parser.add_argument("--workload",default="mixed",choices=WORKLOADS)
    parser.add_argument("--rates",default="500",help="comma-separated scheduled arrivals/sec; each rate gets its own trial blocks")
    parser.add_argument("--gateways",type=int,default=2,choices=(2,4,8),help="gateway count for every mode except local-one")
    parser.add_argument("--delay",type=int,default=50,help="steady backend delay, ms");parser.add_argument("--cold-delay",type=int,default=500,help="cold-burst backend delay, ms")
    parser.add_argument("--max-age",type=int,default=10,help="backend max-age, seconds; gateways cap cached lifetimes at 10")
    parser.add_argument("--concurrency",type=int,default=256,help="generator in-flight request and connection limit")
    parser.add_argument("--keys",type=int,default=10000,help="zipf key-space size");parser.add_argument("--zipf-s",type=float,default=1.1,help="zipf exponent, above 1")
    parser.add_argument("--hot-keys",type=int,default=100,help="hotset size");parser.add_argument("--cache-entries",type=int,default=1000,help="L1 entries per gateway")
    parser.add_argument("--restart-trials",type=int,default=0,help="restart trials per two-gateway mode: gateway 1 stops at 15s and returns empty at 20s")
    parser.add_argument("--seconds",type=int,default=30);parser.add_argument("--warmup",type=int,default=5);parser.add_argument("--trials",type=int,default=5)
    parser.add_argument("--cold-trials",type=int,default=3);parser.add_argument("--soak",type=int,default=300);parser.add_argument("--soak-mode",default="leased-v2",choices=MODES)
    # V3.1 measured auto pinning as noisier than none on this laptop (docs/v3/noise-floor.md), so it is opt-in.
    parser.add_argument("--affinity",default="off",help="'off', 'auto', or explicit sets such as 'generator=0-3;gateways=4-9;support=10-11'")
    parser.add_argument("--quick",action="store_true");parser.add_argument("--skip-build",action="store_true");parser.add_argument("--complete",action="store_true")
    parser.add_argument("--resume",type=Path,help="resume an interrupted artifact directory without repeating completed trials")
    args=parser.parse_args();args.race=False;args.attempt=0
    # V3.3: an idle laptop entered Modern Standby mid-trial, so runs now hold the system awake.
    awake=host.keep_awake()
    args.modes=[m for m in args.modes.split(",") if m]
    try:args.rates=[int(v) for v in args.rates.split(",") if v]
    except ValueError:parser.error("rates must be integers")
    if not args.modes or len(set(args.modes))!=len(args.modes) or any(m not in MODES for m in args.modes):parser.error("modes must be distinct values from "+", ".join(MODES))
    if "local-two-aa" in args.modes and "local-two" not in args.modes:parser.error("local-two-aa is an A/A copy and needs local-two")
    if not args.rates or len(set(args.rates))!=len(args.rates) or min(args.rates)<1:parser.error("rates must be distinct positive integers")
    if args.quick:args.seconds=2;args.warmup=1;args.trials=1;args.cold_trials=1;args.soak=0
    try:args.layout=host.layout(args.affinity)
    except (ValueError,OSError) as error:parser.error(f"affinity: {error}")
    if args.seconds<1 or args.warmup<0 or not 1<=args.trials<=20 or not 0<=args.cold_trials<=20 or args.soak<0 or args.delay<0 or args.cold_delay<0 or args.max_age<0 or args.concurrency<1:parser.error("invalid workload settings")
    if args.keys<1 or not args.zipf_s>1 or args.hot_keys<1 or args.cache_entries<1 or not 0<=args.restart_trials<=20:parser.error("invalid key-space, cache or restart settings")
    args.budget={**BUDGET,"cache_entries_per_node":args.cache_entries}
    folder=args.resume.resolve() if args.resume else ROOT/"bench/results/v3"/datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S-v3")
    if not folder.is_relative_to(ROOT/"bench/results/v3"):parser.error("artifact directory must be inside this checkout's bench/results/v3")
    if not args.resume:folder.mkdir(parents=True)
    completed=set()
    if args.resume:
        original=json.loads((folder/"manifest.json").read_text())
        for key in SETTINGS:
            if original["settings"].get(key)!=getattr(args,key):parser.error(f"resume setting differs: {key}")
        if (original.get("affinity") or {}).get("groups")!=(args.layout or {}).get("groups"):parser.error("resume CPU sets differ")
        for name,digest in original["hashes"].items():
            if name.endswith((".go",".exe")) or name in ("go.mod","go.sum"):
                if hashlib.sha256((ROOT/name).read_bytes()).hexdigest()!=digest:parser.error(f"resume application/binary hash differs: {name}")
        entries=[json.loads(line) for line in (folder/"trials.jsonl").read_text().splitlines()]
        completed={(r["mode"],r["kind"],r["rate"],r["trial"]) for r in entries}
        if len(completed)!=len(entries):parser.error("duplicate recorded trial")
        args.attempt=len(list(folder.glob("resume-*.json")))+1
        (folder/f"resume-{args.attempt}.json").write_text(json.dumps({"time":datetime.now(timezone.utc).isoformat(),"completed_trials_preserved":len(completed),"kept_awake":awake,"note":"Identical settings and application/binary hashes; completed trials kept. Original FAILED marker/logs retained.","harness_hashes":{str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in (ROOT/"bench").glob("*.py")}},indent=2))
    try:
        if not args.skip_build:final.build(folder)
        sources=list((ROOT/"cmd").rglob("*.go"))+list((ROOT/"internal").rglob("*.go"))+list((ROOT/"bench").glob("*.py"))+[ROOT/"go.mod",ROOT/"go.sum"]
        binaries=[ROOT/"bin"/(name+SUFFIX) for name in BINARIES]
        manifest={"source":final.identity(),"time":datetime.now(timezone.utc).isoformat(),"platform":sys.platform,"host":__import__("platform").platform(),"python":sys.version,"cpu_count":os.cpu_count(),"go":subprocess.check_output(["go","version"],text=True).strip(),
                  "settings":{k:str(v) if isinstance(v,Path) else v for k,v in vars(args).items() if k not in ("layout","budget")},"budget":args.budget,
                  "affinity":args.layout and {**args.layout,"unpinned":"Redis (inside the WSL2 VM on Windows), the Python harness and the OS are not pinned."},"power":host.power(),"kept_awake":awake,"redis_version":"7.4.2",
                  "hashes":{str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sources+binaries}}
        if not args.resume:(folder/"manifest.json").write_text(json.dumps(manifest,indent=2))
        if not (folder/"predictions.json").exists():
            # Written before any trial: the same seeded streams the blocks will offer.
            forecasts={}
            for rate in args.rates:
                options=argparse.Namespace(workload=args.workload,keys=args.keys,zipf_s=args.zipf_s,hot_keys=args.hot_keys,rate=rate,seconds=args.seconds,warmup=args.warmup,trials=args.trials,
                                           restart_trials=args.restart_trials,max_age=args.max_age,delay=args.delay,cache_entries=args.cache_entries,gateways=args.gateways)
                forecasts[str(rate)]=predict.predict(options)
            (folder/"predictions.json").write_text(json.dumps({"rates":forecasts},indent=2))
        with (folder/"trials.jsonl").open("a" if args.resume else "w") as out:
            def record(mode,kind,rate,trial):
                if (mode,kind,rate,trial) in completed:return
                row=run_trial(folder,args,mode,kind,rate,trial);out.write(json.dumps(row)+"\n");out.flush();print(mode,kind,rate,trial,flush=True)
            for rate in args.rates:
                for trial in range(1,args.trials+1):
                    # Rotate order while preserving the same seed/offered workload within each block;
                    # with as many trials as modes, every mode runs once in every position.
                    shift=(trial-1)%len(args.modes)
                    for mode in args.modes[shift:]+args.modes[:shift]:record(mode,"steady",rate,trial)
            for trial in range(1,args.cold_trials+1):
                for mode in args.modes:record(mode,"cold",None,trial)
            if args.soak:record(args.soak_mode,"soak",args.rates[0],1)
            restartable=[m for m in args.modes if m in RESTART_MODES]
            for trial in range(1,args.restart_trials+1):
                shift=(trial-1)%max(1,len(restartable))
                for mode in restartable[shift:]+restartable[:shift]:record(mode,"restart",args.rates[0],trial)
        subprocess.run([sys.executable,str(ROOT/"bench/summarize-v3.py"),str(folder)],cwd=ROOT,check=True)
        if args.complete:
            final.verify(folder,args)
            fault_runs(folder,args)
            clean_check(folder,args)
        # Process logs are only read during their trial; one archive keeps the run folder small.
        pack_logs.pack(folder)
        (folder/"SUCCESS").write_text("Comparison assertions and requested checks passed.\n");print(folder,flush=True)
    except BaseException as error:
        (folder/"FAILED").write_text(str(error)+"\n")
        try:pack_logs.pack(folder)
        except Exception as packing:print("log packing failed; logs left in place:",packing,file=sys.stderr)
        raise

if __name__=="__main__":main()
