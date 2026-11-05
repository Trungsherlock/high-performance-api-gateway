"""V3.4 capacity at a fixed SLO. Each mode climbs a shared geometric rate ladder, one trial per
rung, interleaved with the other modes in rotating order. A failed rung is retried once; a second
failure ends the climb, and the highest passing rung then needs --confirm more passing trials.
SLO per measured window (after warmup): at least 99.9% of responses succeed, scheduled p99 is at
most 3x the backend delay, and the generator drops no arrivals.

Run the host-ceiling control first (--workload hot --modes local-two), then pass its folder to the
backend-bound ladders with --host-run so ceilings above half the host ceiling are labelled
host-bound. Before the first trial, predictions.json holds predict-v3.py's ceiling per mode from
the same seeded streams; each mode's climb starts --start-fraction below its predicted ceiling.
Trials reuse run-v3.py's topology, reconciliation and budget (split evenly across gateways)."""
import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

def load(name,file):
    spec=importlib.util.spec_from_file_location(name,Path(__file__).with_name(file));module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
v3=load("run_v3","run-v3.py")
logic=load("capacity_summary","summarize-capacity-v3.py")
predict,final,host,pack_logs=v3.predict,v3.final,v3.host,v3.pack_logs
ROOT=v3.ROOT
SETTINGS=("seconds","warmup","modes","workload","gateways","delay","max_age","concurrency","redis_server","wsl","affinity","keys","zipf_s","hot_keys","cache_entries",
          "start","step","rungs","max_rate","start_fraction","confirm","complete","slo_p99_ms")

def host_ceiling(path):
    """The host-ceiling control's confirmed local-two ceiling on the hot workload."""
    folder=(ROOT/path).resolve();summary=json.loads((folder/"summary.json").read_text())
    assert summary["workload"]=="hot" and (folder/"SUCCESS").exists(),"host run must be a successful hot-workload capacity run"
    ladder=summary["ladders"]["local-two"];assert ladder["complete"] and ladder["ceiling"],"host run has no confirmed ceiling"
    return {"run":folder.relative_to(ROOT).as_posix(),"ceiling":ladder["ceiling"],"first_failing_rate":ladder["first_failing_rate"],"delay_ms":summary["delay_ms"]}

def main():
    parser=argparse.ArgumentParser(description=__doc__,formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--redis-server",default="bin/redis-build/redis-server");parser.add_argument("--wsl",default="Ubuntu" if os.name=="nt" else None)
    parser.add_argument("--workload",default="zipf",choices=v3.WORKLOADS)
    parser.add_argument("--modes",default="local-one,local-two,shared,leased-v2",help="comma-separated subset of "+", ".join(m for m in v3.MODES if m!="local-two-aa"))
    parser.add_argument("--gateways",type=int,default=2,choices=(2,4,8));parser.add_argument("--delay",type=int,default=100,help="backend delay, ms; backend capacity is the aggregate permits / delay")
    parser.add_argument("--max-age",type=int,default=10);parser.add_argument("--concurrency",type=int,default=256,help="generator in-flight request and connection limit")
    parser.add_argument("--keys",type=int,default=10000);parser.add_argument("--zipf-s",type=float,default=1.1);parser.add_argument("--hot-keys",type=int,default=100);parser.add_argument("--cache-entries",type=int,default=1000)
    parser.add_argument("--seconds",type=int,default=30);parser.add_argument("--warmup",type=int,default=5)
    parser.add_argument("--start",type=int,default=400,help="lowest grid rate");parser.add_argument("--step",type=float,default=1.1,help="geometric grid step");parser.add_argument("--rungs",type=int,default=40)
    parser.add_argument("--max-rate",type=int,default=8000,help="grid top")
    parser.add_argument("--start-fraction",type=float,default=0.7,help="each mode starts at the highest rung at or below this fraction of its predicted ceiling")
    parser.add_argument("--confirm",type=int,default=3,help="confirmation trials at the highest passing rung")
    parser.add_argument("--host-run",help="folder of the host-ceiling control run (hot, local-two)")
    parser.add_argument("--slo-p99-ms",type=int,help="scheduled-p99 bound; default 3x --delay. The host control sets it to the backend runs' bound")
    parser.add_argument("--affinity",default="off");parser.add_argument("--quick",action="store_true");parser.add_argument("--skip-build",action="store_true")
    parser.add_argument("--complete",action="store_true",help="also run tests, race tests, vet and build into the artifact")
    parser.add_argument("--resume",type=Path,help="resume an interrupted artifact directory, replaying recorded trials")
    args=parser.parse_args()
    awake=host.keep_awake()
    args.modes=[m for m in args.modes.split(",") if m]
    if not args.modes or len(set(args.modes))!=len(args.modes) or any(m not in v3.MODES or m=="local-two-aa" for m in args.modes):parser.error("modes must be distinct V3 modes other than local-two-aa")
    if args.quick:args.seconds=2;args.warmup=1;args.confirm=1
    if args.seconds<1 or args.warmup<0 or args.delay<1 or args.concurrency<1 or not args.step>1 or args.rungs<1 or args.start<1 or not 0<args.start_fraction<=1 or args.confirm<0:parser.error("invalid settings")
    if args.keys<1 or not args.zipf_s>1 or args.hot_keys<1 or args.cache_entries<1:parser.error("invalid key-space or cache settings")
    try:args.layout=host.layout(args.affinity)
    except (ValueError,OSError) as error:parser.error(f"affinity: {error}")
    # Fields run_trial and the V2.7 checks read; capacity trials never run cold bursts or soaks.
    args.cold_delay=500;args.soak=0;args.race=False;args.attempt=0
    args.budget={**v3.BUDGET,"cache_entries_per_node":args.cache_entries}
    grid=predict.ladder(args.start,args.step,args.rungs,args.max_rate)
    reference=host_ceiling(args.host_run) if args.host_run else None
    folder=args.resume.resolve() if args.resume else ROOT/"bench/results/v3"/datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S-v34")
    if not folder.is_relative_to(ROOT/"bench/results/v3"):parser.error("artifact directory must be inside this checkout's bench/results/v3")
    recorded={}
    if args.resume:
        original=json.loads((folder/"manifest.json").read_text())
        for key in SETTINGS:
            if original["settings"].get(key)!=getattr(args,key):parser.error(f"resume setting differs: {key}")
        if original["host"]!=reference:parser.error("resume host reference differs")
        for name,digest in original["hashes"].items():
            if name.endswith((".go",".exe")) or name in ("go.mod","go.sum"):
                if hashlib.sha256((ROOT/name).read_bytes()).hexdigest()!=digest:parser.error(f"resume application/binary hash differs: {name}")
        if (folder/"trials.jsonl").exists():
            for line in (folder/"trials.jsonl").read_text().splitlines():
                row=json.loads(line);key=(row["mode"],row["rate"],row["trial"])
                if key in recorded:parser.error("duplicate recorded trial")
                recorded[key]=row
        args.attempt=len(list(folder.glob("resume-*.json")))+1
        (folder/f"resume-{args.attempt}.json").write_text(json.dumps({"time":datetime.now(timezone.utc).isoformat(),"completed_trials_preserved":len(recorded),"kept_awake":awake,
            "note":"Identical settings and application/binary hashes; recorded trials replayed, not rerun. Original FAILED marker/logs retained.",
            "harness_hashes":{str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in (ROOT/"bench").glob("*.py")}},indent=2))
    else:folder.mkdir(parents=True)
    try:
        if not args.skip_build:final.build(folder)
        if not (folder/"predictions.json").exists():
            # Written before any trial. The hot workload does almost no backend work, so the
            # host-ceiling control has nothing to predict from the backend model.
            forecast=None
            if args.workload!="hot":
                options=argparse.Namespace(workload=args.workload,keys=args.keys,zipf_s=args.zipf_s,hot_keys=args.hot_keys,seconds=args.seconds,warmup=args.warmup,max_age=args.max_age,delay=args.delay,
                                           cache_entries=args.cache_entries,gateways=args.gateways,permits=args.budget["aggregate_permits"],permit_queue=args.budget["aggregate_permit_queue"],
                                           permit_wait=50,flights=args.budget["aggregate_cache_flights"],slo_p99_ms=args.slo_p99_ms)
                forecast=predict.capacity(options,grid,tuple(dict.fromkeys(predict.MODEL_FOR_MODE[m] for m in args.modes)))
            (folder/"predictions.json").write_text(json.dumps(forecast,indent=2))
        forecast=json.loads((folder/"predictions.json").read_text())
        starts={}
        for mode in args.modes:
            ceiling=forecast["ceilings"].get(predict.MODEL_FOR_MODE[mode]) if forecast else None
            starts[mode]=max([k for k,rate in enumerate(grid) if ceiling and rate<=args.start_fraction*ceiling],default=0)
        if not args.resume:
            sources=list((ROOT/"cmd").rglob("*.go"))+list((ROOT/"internal").rglob("*.go"))+list((ROOT/"bench").glob("*.py"))+[ROOT/"go.mod",ROOT/"go.sum"]
            binaries=[ROOT/"bin"/(name+v3.SUFFIX) for name in v3.BINARIES]
            manifest={"source":final.identity(),"time":datetime.now(timezone.utc).isoformat(),"platform":sys.platform,"host":reference,"python":sys.version,"cpu_count":os.cpu_count(),
                      "go":subprocess.check_output(["go","version"],text=True).strip(),"settings":{k:str(v) if isinstance(v,Path) else v for k,v in vars(args).items() if k not in ("layout","budget")},
                      "budget":args.budget,"permit_wait_ms":50,"slo":predict.SLO,"slo_p99_ms":logic.p99_limit(vars(args)),"grid":grid,"start_rungs":starts,
                      "affinity":args.layout and {**args.layout,"unpinned":"Redis (inside the WSL2 VM on Windows), the Python harness and the OS are not pinned."},"power":host.power(),"kept_awake":awake,
                      "redis_version":"7.4.2","hashes":{str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sources+binaries}}
            (folder/"manifest.json").write_text(json.dumps(manifest,indent=2))
        ladders={mode:logic.Ladder(mode,grid,starts[mode],args.confirm) for mode in args.modes}
        with (folder/"trials.jsonl").open("a") as out:
            round=0
            while True:
                active=[m for m in args.modes if not ladders[m].done]
                if not active:break
                shift=round%len(active)
                for mode in active[shift:]+active[:shift]:
                    rate,trial=ladders[mode].next()
                    row=recorded.get((mode,rate,trial))
                    if row is None:
                        row=v3.run_trial(folder,args,mode,"capacity",rate,trial);row["workload"]=args.workload
                        out.write(json.dumps(row)+"\n");out.flush()
                    v=logic.steady.summarize(folder,args.budget,row,None);result=logic.verdict(v,logic.p99_limit(vars(args)))
                    ladders[mode].record(result["passed"])
                    detail="" if result["passed"] else f" {result['cause']}: {[k for k,ok in result['checks'].items() if not ok]} success {result['success_ratio']:.3%} p99 {result['scheduled_p99_ms']:.0f} ms drops {v['generator_drops']} {({k:n for k,n in result['failure_causes'].items() if n})}"
                    print(f"{mode} r{rate} t{trial} {'pass' if result['passed'] else 'FAIL'}{detail}",flush=True)
                round+=1
        subprocess.run([sys.executable,str(ROOT/"bench/summarize-capacity-v3.py"),str(folder)],cwd=ROOT,check=True)
        if args.complete:final.verify(folder,args)
        pack_logs.pack(folder)
        (folder/"SUCCESS").write_text("Capacity ladders finished; every trial reconciled.\n");print(folder,flush=True)
    except BaseException as error:
        (folder/"FAILED").write_text(str(error)+"\n")
        try:pack_logs.pack(folder)
        except Exception as packing:print("log packing failed; logs left in place:",packing,file=sys.stderr)
        raise

if __name__=="__main__":main()
