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
"""
import argparse
from collections import OrderedDict
import json
import os
from pathlib import Path
import statistics
import subprocess

ROOT=Path(__file__).resolve().parent.parent
SUFFIX=".exe" if os.name=="nt" else ""
MODEL_FOR_MODE={"local-one":"one","local-two":"local","local-two-aa":"local","shared":"shared","leased":"leased","leased-v2":"leased"}
RESTART={"stop":15.0,"start":20.0,"seconds":35}  # measurement-relative; gateway-1 stops, then restarts empty

class L1:
    """Entry-bounded LRU with absolute expiries, like internal/cache for these small bodies."""
    def __init__(self,entries):self.entries=entries;self.items=OrderedDict()
    def get(self,key,t):
        expiry=self.items.get(key)
        if expiry is None or expiry<=t:return False
        self.items.move_to_end(key);return True
    def put(self,key,expiry):
        self.items[key]=expiry;self.items.move_to_end(key)
        while len(self.items)>self.entries:self.items.popitem(last=False)

def plan(args,seed,seconds):
    command=[ROOT/"bin"/("loadgen"+SUFFIX),"-plan-only","-workload",args.workload,"-keys",args.keys,"-zipf-s",args.zipf_s,"-hot-keys",args.hot_keys,
             "-rate",args.rate,"-duration",f"{seconds}s","-warmup",f"{args.warmup}s","-seed",seed]
    phases={p["Phase"]:p for p in map(json.loads,subprocess.check_output(list(map(str,command)),text=True).splitlines())}
    stream=[(i/args.rate,k) for i,k in enumerate(phases.get("warmup",{"Keys":[]})["Keys"])]
    start=args.warmup
    stream+=[(start+i/args.rate,k) for i,k in enumerate(phases["measurement"]["Keys"])]
    return stream,start,len(phases["measurement"]["Keys"])

def simulate(stream,model,nodes,ttl,entries,fill,down=None):
    """Return the times of backend fills. down=(stop,start) takes gateway 1 out of rotation,
    then returns it with an empty L1."""
    if model=="one":nodes=1
    caches=[L1(entries) for _ in range(nodes)];local=[{} for _ in range(nodes)];leases={};store={};fills=[];turn=0;restarted=False
    for t,key in stream:
        healthy=[n for n in range(nodes) if not (down and n==1 and down[0]<=t<down[1])]
        if down and not restarted and t>=down[1]:caches[1]=L1(entries);local[1]={};restarted=True
        n=next(i for i in ((turn+j)%nodes for j in range(nodes)) if i in healthy);turn=(n+1)%nodes
        if caches[n].get(key,t):continue
        if model in ("shared","leased"):
            available,expiry=store.get(key,(None,None))
            if available is not None and available<=t<expiry:caches[n].put(key,expiry);continue
        flights=leases if model=="leased" else local[n]
        done,expiry=flights.get(key,(None,None))
        if done is not None and t<done:caches[n].put(key,expiry);continue  # follower: same value, same expiry
        fills.append(t);expiry=t+ttl;flights[key]=(t+fill,expiry);caches[n].put(key,expiry)
        if model in ("shared","leased"):store[key]=(t+fill,expiry)
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

def main():
    parser=argparse.ArgumentParser(description=__doc__,formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--workload",default="mixed",choices=("hot","mixed","unique","zipf","hotset"))
    parser.add_argument("--keys",type=int,default=10000);parser.add_argument("--zipf-s",type=float,default=1.1);parser.add_argument("--hot-keys",type=int,default=100)
    parser.add_argument("--rate",type=int,default=500);parser.add_argument("--seconds",type=int,default=30);parser.add_argument("--warmup",type=int,default=5)
    parser.add_argument("--trials",type=int,default=5);parser.add_argument("--restart-trials",type=int,default=0)
    parser.add_argument("--max-age",type=int,default=10);parser.add_argument("--delay",type=int,default=50,help="backend delay, ms: the duration of one fill")
    parser.add_argument("--cache-entries",type=int,default=1000);parser.add_argument("--gateways",type=int,default=2)
    parser.add_argument("--out",type=Path)
    args=parser.parse_args();result=predict(args)
    if args.out:args.out.write_text(json.dumps(result,indent=2))
    m=result["medians"]
    if m:
        print(f"{args.workload}: median predicted backend fills per {args.seconds}s block over {args.trials} blocks (ttl {result['ttl_seconds']}s, fill {args.delay}ms)")
        print("  "+"  ".join(f"{k} {m[k]:.0f}" for k in ("one","local","shared","leased","shared_ideal"))+f"  cross-node duplicates {m['cross_node_duplicate_fraction']:.1%}")
    for model,w in result["restart_medians"].items():
        print(f"  restart {model}: fills pre(5-15s) {w['pre']:.0f}, down(15-20s) {w['down']:.0f}, post(20-30s) {w['post']:.0f}, early(20-22s) {w['early']:.0f}")

if __name__=="__main__":main()
