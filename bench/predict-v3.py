"""Predict V3 backend fills from the generator's own seeded key stream (loadgen -plan-only).

Each model replays the planned arrivals in order through round-robin routing and per-node
LRU/TTL caches, ignoring network cost:
  one          one gateway with local coalescing (local-one)
  local        independent gateways (local-two)
  shared       gateways plus a shared store; a fill becomes visible only when it completes,
               so other gateways that miss meanwhile fill again (shared)
  leased       as shared, but a fill in flight anywhere makes other gateways wait (leased-v2)
  shared_ideal leased with instant fills: the floor any shared tier can reach
A fill lasts the backend delay; requests on the same gateway during it are local followers.

V3.4 adds the gateway's per-node backend limits (--capacity): a fill needs a cache-flight
slot and an upstream permit; with every permit busy it queues FIFO in a bounded waiter queue
for at most the permit wait, else it is rejected with 503 together with the requests
coalesced onto it. The capacity ladder then predicts, per mode and rate, rejected requests,
latency and whether the SLO holds. Network, Redis and polling time are ignored.
"""
import argparse
from collections import OrderedDict
import heapq
import json
import os
from pathlib import Path
import statistics
import subprocess

ROOT=Path(__file__).resolve().parent.parent
SUFFIX=".exe" if os.name=="nt" else ""
MODEL_FOR_MODE={"local-one":"one","local-two":"local","local-two-aa":"local","shared":"shared","leased":"leased","leased-v2":"leased"}
RESTART={"stop":15.0,"start":20.0,"seconds":35}  # measurement-relative; gateway-1 stops, then restarts empty
# V3.4 SLO over a measured window: success ratio, scheduled p99 as a multiple of the backend delay, no drops.
SLO={"success_ratio":0.999,"p99_delay_multiple":3,"generator_drops":0}

class L1:
    """Entry-bounded LRU with absolute expiries, like internal/cache for these small bodies.
    An entry also records when its value becomes available: a later time means a fill for it
    is still in flight, so a request that finds it waits as a follower."""
    def __init__(self,entries):self.entries=entries;self.items=OrderedDict()
    def get(self,key,t):
        item=self.items.get(key)
        if item is None or item[1]<=t:return None
        self.items.move_to_end(key);return item[0]
    def put(self,key,available,expiry):
        self.items[key]=(available,expiry);self.items.move_to_end(key)
        while len(self.items)>self.entries:self.items.popitem(last=False)

def plan(args,seed,seconds):
    command=[ROOT/"bin"/("loadgen"+SUFFIX),"-plan-only","-workload",args.workload,"-keys",args.keys,"-zipf-s",args.zipf_s,"-hot-keys",args.hot_keys,
             "-rate",args.rate,"-duration",f"{seconds}s","-warmup",f"{args.warmup}s","-seed",seed]
    phases={p["Phase"]:p for p in map(json.loads,subprocess.check_output(list(map(str,command)),text=True).splitlines())}
    stream=[(i/args.rate,k) for i,k in enumerate(phases.get("warmup",{"Keys":[]})["Keys"])]
    start=args.warmup
    stream+=[(start+i/args.rate,k) for i,k in enumerate(phases["measurement"]["Keys"])]
    return stream,start,len(phases["measurement"]["Keys"])

def simulate(stream,model,nodes,ttl,entries,fill,down=None,limits=None,outcomes=None):
    """Return the times of backend fills. down=(stop,start) takes gateway 1 out of rotation,
    then returns it with an empty L1. limits={"permits","queue","wait","flights"} per node adds
    the backend limits; without it fills never wait, exactly as in V3.3. outcomes, when given,
    receives (arrival, seconds until the response, rejection reason or None) per request."""
    if model=="one":nodes=1
    caches=[L1(entries) for _ in range(nodes)];local=[{} for _ in range(nodes)];leases={};store={};fills=[];turn=0;restarted=False
    note=outcomes.append if outcomes is not None else lambda outcome:None
    if limits:
        permits=[[0.0]*limits["permits"] for _ in range(nodes)];waiting=[[] for _ in range(nodes)];flights=[{} for _ in range(nodes)]
        def admit(n,t):
            # Distinct keys in flight on node n, including keys a lease follower waits on.
            flights[n]={k:e for k,e in flights[n].items() if e>t};return len(flights[n])<limits["flights"]
        def acquire(n,t):
            # Earliest-free permit = FIFO c-server queue; waiter slots last until a permit or the wait expires.
            waiting[n]=[e for e in waiting[n] if e>t];begin=max(t,permits[n][0])
            if begin>t:
                if len(waiting[n])>=limits["queue"]:return t,"queue_full"
                if begin-t>limits["wait"]:waiting[n].append(t+limits["wait"]);return t+limits["wait"],"wait_timeout"
                waiting[n].append(begin)
            heapq.heapreplace(permits[n],begin+fill);return begin+fill,None
    for t,key in stream:
        healthy=[n for n in range(nodes) if not (down and n==1 and down[0]<=t<down[1])]
        if down and not restarted and t>=down[1]:
            caches[1]=L1(entries);local[1]={};restarted=True
            if limits:permits[1]=[0.0]*limits["permits"];waiting[1]=[];flights[1]={}
        n=next(i for i in ((turn+j)%nodes for j in range(nodes)) if i in healthy);turn=(n+1)%nodes
        available=caches[n].get(key,t)
        if available is not None:note((t,max(0.0,available-t),None));continue
        flight=local[n].get(key)
        if flight and t<flight[0]:
            # Coalesced onto this node's flight, sharing its outcome.
            if flight[2] is None:caches[n].put(key,flight[0],flight[1])
            note((t,flight[0]-t,flight[2]));continue
        if limits and not admit(n,t):note((t,0.0,"cache_flights_full"));continue
        if model in ("shared","leased"):
            available,expiry=store.get(key,(None,None))
            if available is not None and available<=t<expiry:caches[n].put(key,t,expiry);note((t,0.0,None));continue
        if model=="leased":
            lease=leases.get(key)
            # A rejected owner releases its lease; a request that finds it fills for itself.
            if lease and t<lease[0] and lease[2] is None:
                local[n][key]=lease;caches[n].put(key,lease[0],lease[1])
                if limits:flights[n][key]=lease[0]
                note((t,lease[0]-t,None));continue
        done,failure=acquire(n,t) if limits else (t+fill,None)
        if limits:flights[n][key]=done
        if failure:
            local[n][key]=(done,None,failure)
            if model=="leased":leases[key]=(done,None,failure)
            note((t,done-t,failure));continue
        fills.append(t);expiry=t+ttl;flight=(done,expiry,None);local[n][key]=flight;caches[n].put(key,done,expiry)
        if model=="leased":leases[key]=flight
        if model in ("shared","leased"):store[key]=(done,expiry)
        note((t,done-t,None))
    return fills

def count(fills,lo,hi):return sum(lo<=t<hi for t in fills)

def predict(args):
    ttl=min(args.max_age,10);fill=args.delay/1000;out={"settings":{k:v for k,v in vars(args).items() if k!="out"},"ttl_seconds":ttl,"fill_seconds":fill,"blocks":{},"restart":{}}
    models={"one":fill,"local":fill,"shared":fill,"leased":fill,"shared_ideal":0.0}
    for trial in range(1,args.trials+1):
        stream,start,requests=plan(args,42+trial,args.seconds)
        keys={k for t,k in stream if t>=start}
        fills={name:count(simulate(stream,"leased" if name=="shared_ideal" else name,args.gateways,ttl,args.cache_entries,f),start,float("inf")) for name,f in models.items()}
        out["blocks"][str(trial)]={"requests":requests,"distinct_keys":len(keys),"fills":fills,
                                   "cross_node_duplicate_fraction":(fills["local"]-fills["shared_ideal"])/fills["local"] if fills["local"] else None}
    for trial in range(1,args.restart_trials+1):
        stream,start,_=plan(args,42+trial,RESTART["seconds"])
        down=(start+RESTART["stop"],start+RESTART["start"]);windows={"pre":(5,15),"down":(15,20),"post":(20,30),"early":(20,22)}
        out["restart"][str(trial)]={name:{w:count(simulate(stream,name,args.gateways,ttl,args.cache_entries,fill,down),start+lo,start+hi) for w,(lo,hi) in windows.items()} for name in ("local","shared","leased")}
    names=list(models)+["cross_node_duplicate_fraction"]
    out["medians"]={name:statistics.median(b["fills"][name] if name in b["fills"] else b[name] for b in out["blocks"].values()) for name in names} if out["blocks"] else {}
    out["restart_medians"]={m:{w:statistics.median(r[m][w] for r in out["restart"].values()) for w in ("pre","down","post","early")} for m in ("local","shared","leased")} if out["restart"] else {}
    return out

def ladder(start,step,rungs,top=None):
    """The shared geometric rate grid every mode climbs, in whole arrivals per second."""
    rates=[]
    for k in range(rungs):
        rate=round(start*step**k)
        if top and rate>top:break
        if not rates or rate>rates[-1]:rates.append(rate)
    return rates

def percentile(values,p):
    # The generator's nearest-rank percentile (cmd/loadgen percentiles).
    values=sorted(values);return values[(len(values)*p+99)//100-1] if values else None

def rung(args,rate,model,trial=1):
    """One capacity trial at rate: the climb trial's seeded stream through the limited model."""
    nodes=1 if model=="one" else args.gateways;fill=args.delay/1000
    limits={"permits":args.permits//nodes,"queue":args.permit_queue//nodes,"wait":args.permit_wait/1000,"flights":args.flights//nodes}
    stream,start,requests=plan(argparse.Namespace(**{**vars(args),"rate":rate}),42+trial,args.seconds)
    outcomes=[];fills=count(simulate(stream,model,args.gateways,min(args.max_age,10),args.cache_entries,fill,limits=limits,outcomes=outcomes),start,float("inf"))
    window=[o for o in outcomes if o[0]>=start];rejected={}
    for _,_,reason in window:
        if reason:rejected[reason]=rejected.get(reason,0)+1
    success=1-sum(rejected.values())/len(window);p99=percentile([o[1]*1000 for o in window],99)
    return {"requests":len(window),"fills":fills,"fills_per_second":fills/args.seconds,"backend_calls_per_request":fills/len(window),
            "backend_utilization":fills*fill/(args.permits*args.seconds),"rejected":rejected,"success_ratio":success,"p99_ms":p99,
            "passes":success>=SLO["success_ratio"] and p99<=(getattr(args,"slo_p99_ms",None) or SLO["p99_delay_multiple"]*args.delay)}

def capacity(args,rates,models=("one","local","shared","leased")):
    """Climb the grid per model, as the harness does, and stop each model one rung after it
    first fails. The predicted ceiling is the last passing rung before that failure."""
    out={"slo":SLO,"limits":{"permits":args.permits,"permit_queue":args.permit_queue,"permit_wait_ms":args.permit_wait,"flights":args.flights},"rates":{},"ceilings":{},"first_failures":{}}
    active=list(models)
    for rate in rates:
        if not active:break
        row={}
        for model in list(active):
            row[model]=result=rung(args,rate,model)
            if result["passes"]:out["ceilings"][model]=rate
            elif model not in out["first_failures"]:out["first_failures"][model]=rate;active.remove(model)
        out["rates"][str(rate)]=row
    for model in models:out["ceilings"].setdefault(model,None);out["first_failures"].setdefault(model,None)
    return out

def main():
    parser=argparse.ArgumentParser(description=__doc__,formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--workload",default="mixed",choices=("hot","mixed","unique","zipf","hotset"))
    parser.add_argument("--keys",type=int,default=10000);parser.add_argument("--zipf-s",type=float,default=1.1);parser.add_argument("--hot-keys",type=int,default=100)
    parser.add_argument("--rate",type=int,default=500);parser.add_argument("--seconds",type=int,default=30);parser.add_argument("--warmup",type=int,default=5)
    parser.add_argument("--trials",type=int,default=5);parser.add_argument("--restart-trials",type=int,default=0)
    parser.add_argument("--max-age",type=int,default=10);parser.add_argument("--delay",type=int,default=50,help="backend delay, ms: the duration of one fill")
    parser.add_argument("--cache-entries",type=int,default=1000);parser.add_argument("--gateways",type=int,default=2)
    parser.add_argument("--capacity",action="store_true",help="V3.4: predict SLO outcomes on a rate ladder with the gateway's backend limits")
    parser.add_argument("--start",type=int,default=200);parser.add_argument("--step",type=float,default=1.1);parser.add_argument("--rungs",type=int,default=40);parser.add_argument("--max-rate",type=int,default=6000)
    parser.add_argument("--permits",type=int,default=32,help="aggregate upstream permits, split evenly across gateways")
    parser.add_argument("--permit-queue",type=int,default=32,help="aggregate permit waiter slots");parser.add_argument("--permit-wait",type=int,default=50,help="permit wait, ms")
    parser.add_argument("--flights",type=int,default=64,help="aggregate cache flights")
    parser.add_argument("--out",type=Path)
    args=parser.parse_args()
    if args.capacity:
        result=capacity(args,ladder(args.start,args.step,args.rungs,args.max_rate))
        if args.out:args.out.write_text(json.dumps(result,indent=2))
        print(f"{args.workload}: backend {args.permits} permits / {args.delay} ms = {args.permits*1000/args.delay:.0f} fills/s; SLO success >= {SLO['success_ratio']:.1%}, p99 <= {SLO['p99_delay_multiple']*args.delay} ms")
        for model in result["ceilings"]:
            ceiling,failed=result["ceilings"][model],result["first_failures"][model]
            detail=""
            if failed:
                r=result["rates"][str(failed)][model];detail=f"; at {failed}/s: success {r['success_ratio']:.3%}, p99 {r['p99_ms']:.0f} ms, utilization {r['backend_utilization']:.0%}, rejected {r['rejected']}"
            if ceiling:
                c=result["rates"][str(ceiling)][model];detail=f"; at ceiling {c['backend_calls_per_request']:.3f} calls/request, utilization {c['backend_utilization']:.0%}"+detail
            print(f"  {model:<6} ceiling {ceiling}/s, first failure {failed}/s{detail}")
        return
    result=predict(args)
    if args.out:args.out.write_text(json.dumps(result,indent=2))
    m=result["medians"]
    if m:
        print(f"{args.workload}: median predicted backend fills per {args.seconds}s block over {args.trials} blocks (ttl {result['ttl_seconds']}s, fill {args.delay}ms)")
        print("  "+"  ".join(f"{k} {m[k]:.0f}" for k in ("one","local","shared","leased","shared_ideal"))+f"  cross-node duplicates {m['cross_node_duplicate_fraction']:.1%}")
    for model,w in result["restart_medians"].items():
        print(f"  restart {model}: fills pre(5-15s) {w['pre']:.0f}, down(15-20s) {w['down']:.0f}, post(20-30s) {w['post']:.0f}, early(20-22s) {w['early']:.0f}")

if __name__=="__main__":main()
