"""Summarize a V3.4 capacity run (capacity-v3.py). Every trial is reconciled as in V3.3
(summarize-v3.py) and judged against the SLO; each mode's ladder is replayed from its trials
in recorded order, giving its ceiling (the highest rung whose climb trial and confirmations all
passed), its first failing rate with the reasons, and a bound label:
  backend-bound       most failed responses at the first failing rate were permit or
                      cache-admission 503s, and the ceiling is at most half the host ceiling
  coordination-bound  most were lease or Redis coordination 503s
  host-bound          the rate failed on drops, latency, front or transport errors, or the
                      ceiling is above half the host ceiling, so the host may have limited it
The ladder logic and SLO verdict here also drive capacity-v3.py."""
import argparse
import importlib.util
import json
from pathlib import Path
import statistics

def load(name,file):
    spec=importlib.util.spec_from_file_location(name,Path(__file__).with_name(file));module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
steady=load("v3_summary","summarize-v3.py")
predict=steady.predict
PERMIT=("queue_full","wait_timeout");ADMISSION=("cache_flights_full","cache_waiters_full");COORDINATION=("coordination_error","coordination_timeout","lease_lost")

def p99_limit(settings):
    """The SLO's scheduled-p99 bound: 3x the backend delay unless the run fixed it (the host
    control runs a near-zero delay but is judged by the backend runs' bound)."""
    return settings.get("slo_p99_ms") or predict.SLO["p99_delay_multiple"]*settings["delay"]

def verdict(v,limit_ms):
    """SLO checks for one reconciled trial; on failure, which responses failed and the dominant cause."""
    slo=predict.SLO;reasons=v["rejection_reasons"];failed=v["sent"]-v["success"]
    causes={"permit":sum(reasons.get(k,0) for k in PERMIT),"admission":sum(reasons.get(k,0) for k in ADMISSION),"coordination":sum(reasons.get(k,0) for k in COORDINATION),
            "front_unavailable":v["front_unavailable"],"transport":v["transport_errors"]}
    causes["other"]=failed-sum(causes.values())
    success=v["success"]/v["sent"] if v["sent"] else 0.0;p99=v["scheduled_latency_ms"]["p99"]
    checks={"success_ratio":success>=slo["success_ratio"],"scheduled_p99":p99<=limit_ms,"generator_drops":v["generator_drops"]<=slo["generator_drops"]}
    passed=all(checks.values());cause=None
    if not passed:
        # Backend waits are bounded (permit wait + fill + lease poll stay under 3x the delay), so
        # latency or drop failures without a majority of limit 503s point at the host.
        if not checks["success_ratio"] and 2*(causes["permit"]+causes["admission"])>=failed:cause="backend"
        elif not checks["success_ratio"] and 2*causes["coordination"]>=failed:cause="coordination"
        else:cause="host"
    return {"passed":passed,"checks":checks,"success_ratio":success,"scheduled_p99_ms":p99,"failed":failed,"failure_causes":causes,"cause":cause}

class Ladder:
    """One mode's climb on the shared grid. Each rung gets a climb trial (trial 1); a failure is
    retried once at the same rung (trial 2), and a second failure ends the climb. If the start
    rung itself fails twice, the ladder steps down until a rung passes. The highest passing rung
    then needs `confirmations` more passing trials (trials 3, 4, ...); a failed confirmation moves
    the candidate one rung down and starts its confirmations again."""
    def __init__(self,mode,grid,start,confirmations):
        self.mode=mode;self.grid=grid;self.rung=start;self.state="climb";self.best=None;self.confirmations=confirmations;self.confirmed=0
        self.descending=False;self.done=False;self.ceiling=None;self.top_reached=False
    def next(self):
        if self.done:return None
        if self.state=="climb":return self.grid[self.rung],1
        if self.state=="retry":return self.grid[self.rung],2
        return self.grid[self.best],3+self.confirmed
    def confirm(self):
        if self.confirmations:self.state="confirm";self.confirmed=0
        else:self.done=True;self.ceiling=self.best
    def record(self,passed):
        if self.state in ("climb","retry"):
            if passed:
                self.best=self.rung
                if self.descending:self.confirm()
                elif self.rung+1<len(self.grid):self.rung+=1;self.state="climb"
                else:self.top_reached=True;self.confirm()
            elif self.state=="climb":self.state="retry"
            elif self.best is not None:self.confirm()
            elif self.rung>0:self.rung-=1;self.state="climb";self.descending=True
            else:self.done=True
        elif passed:
            self.confirmed+=1
            if self.confirmed==self.confirmations:self.done=True;self.ceiling=self.best
        elif self.best>0:self.best-=1;self.confirmed=0
        else:self.done=True;self.best=None

def median(values):
    values=[v for v in values if v is not None];return statistics.median(values) if values else None

def cpu_cores(v):
    """Cores used by all sampled processes (generator, front, gateways, backend, Redis) over the sampled span."""
    cores=[p["sampled_cpu_seconds"]/p["sampled_span_seconds"] for p in v["resources"].values() if p["sampled_cpu_seconds"] is not None and p["sampled_span_seconds"]]
    return sum(cores) if cores else None

def rung_metrics(trials,permits,delay,seconds):
    """Medians over the trials run at one rung."""
    capacity=permits*1000/delay
    return {"trials":len(trials),"passed":sum(t["verdict"]["passed"] for t in trials),"successful_rps":median([t["successful_rps"] for t in trials]),
            "success_ratio":median([t["verdict"]["success_ratio"] for t in trials]),"scheduled_p99_ms":median([t["scheduled_latency_ms"]["p99"] for t in trials]),
            "send_p99_ms":median([t["send_latency_ms"]["p99"] for t in trials]),"backend_calls_per_request":median([t["backend_calls_per_request"] for t in trials]),
            "backend_utilization":median([t["backend_calls"]/seconds/capacity for t in trials]),"shared_lookup_hit_ratio":median([t["shared_lookup_hit_ratio"] for t in trials]),
            "generator_drops":sum(t["generator_drops"] for t in trials),"rejection_reasons":{k:sum(t["rejection_reasons"].get(k,0) for t in trials) for k in sorted({k for t in trials for k in t["rejection_reasons"]})},
            "cpu_cores":median([cpu_cores(t) for t in trials])}

def label(cause,ceiling,host_ceiling,host_stage):
    if host_stage:return "host ceiling"
    if ceiling and host_ceiling and ceiling>host_ceiling/2:return "host-bound"
    return {"backend":"backend-bound","coordination":"coordination-bound","host":"host-bound",None:"not reached"}[cause]

def ladder_summary(mode,trials,manifest,predictions):
    settings=manifest["settings"];grid=manifest["grid"];ladder=Ladder(mode,grid,manifest["start_rungs"][mode],settings["confirm"])
    for t in trials:
        expected=ladder.next();assert expected==(t["rate"],t["trial"]),(mode,expected,t["rate"],t["trial"]);ladder.record(t["verdict"]["passed"])
    complete=ladder.done;ceiling=ladder.grid[ladder.best] if ladder.done and ladder.best is not None else None
    above=[t for t in trials if not t["verdict"]["passed"] and (ceiling is None or t["rate"]>ceiling)]
    first=min((t["rate"] for t in above),default=None);failing=[t for t in above if t["rate"]==first]
    causes=[t["verdict"]["cause"] for t in failing];cause=max(set(causes),key=causes.count) if causes else None
    host=manifest.get("host") or {};at=lambda rate:[t for t in trials if t["rate"]==rate]
    model=predict.MODEL_FOR_MODE[mode];forecast=predictions["ceilings"].get(model) if predictions else None
    return {"mode":mode,"complete":complete,"ceiling":ceiling,"confirmations":ladder.confirmed if ceiling else None,"top_of_grid_passed":ladder.top_reached,
            "first_failing_rate":first,"cause":cause,"bound":label(cause,ceiling,host.get("ceiling"),settings["workload"]=="hot"),
            "failing_trials":[{"rate":t["rate"],"trial":t["trial"],"checks":t["verdict"]["checks"],"success_ratio":t["verdict"]["success_ratio"],"scheduled_p99_ms":t["verdict"]["scheduled_p99_ms"],
                               "generator_drops":t["generator_drops"],"failure_causes":t["verdict"]["failure_causes"],"cause":t["verdict"]["cause"]} for t in above],
            "at_ceiling":rung_metrics(at(ceiling),manifest["budget"]["aggregate_permits"],settings["delay"],settings["seconds"]) if ceiling else None,
            "at_first_failure":rung_metrics(failing,manifest["budget"]["aggregate_permits"],settings["delay"],settings["seconds"]) if failing else None,
            "predicted_ceiling":forecast,"predicted_first_failure":predictions["first_failures"].get(model) if predictions else None,
            "trials":[(t["rate"],t["trial"],t["verdict"]["passed"]) for t in trials]}

def ratios(ladders,workload_trials,base="local-two"):
    """Ceiling ratios against local-two, and the inverse backend-calls-per-request ratio at the
    highest rate both modes passed in a climb trial (the plan's zipf prediction). A ratio is a
    capacity ratio only when both ceilings are backend-bound; "bounds" says which they are."""
    out={}
    if base not in ladders or not ladders[base]["ceiling"]:return out
    for mode,l in ladders.items():
        if mode==base or not l["ceiling"]:continue
        passed={(t["mode"],t["rate"]):t for t in workload_trials if t["trial"]==1 and t["verdict"]["passed"]}
        common=max((r for (m,r) in passed if m==mode and (base,r) in passed),default=None)
        inverse=passed[base,common]["backend_calls_per_request"]/passed[mode,common]["backend_calls_per_request"] if common else None
        out[mode]={"bounds":[l["bound"],ladders[base]["bound"]],"capacity_ratio":l["bound"]==ladders[base]["bound"]=="backend-bound","ceiling_ratio":l["ceiling"]/ladders[base]["ceiling"],"predicted_ratio":l["predicted_ceiling"]/ladders[base]["predicted_ceiling"] if l["predicted_ceiling"] and ladders[base]["predicted_ceiling"] else None,
                   "inverse_backend_call_ratio":inverse,"at_rate":common}
    return out

def summarize(folder):
    manifest=json.loads((folder/"manifest.json").read_text());settings=manifest["settings"]
    rows=[json.loads(line) for line in (folder/"trials.jsonl").read_text().splitlines()] if (folder/"trials.jsonl").exists() else []
    predictions=json.loads((folder/"predictions.json").read_text()) if (folder/"predictions.json").exists() else None
    trials=[]
    for row in rows:
        v=steady.summarize(folder,manifest["budget"],row,None);v["verdict"]=verdict(v,p99_limit(settings));trials.append(v)
    ladders={mode:ladder_summary(mode,[t for t in trials if t["mode"]==mode],manifest,predictions) for mode in settings["modes"] if any(t["mode"]==mode for t in trials)}
    return {"workload":settings["workload"],"slo":predict.SLO,"p99_limit_ms":p99_limit(settings),"delay_ms":settings["delay"],"backend_capacity_fills_per_second":manifest["budget"]["aggregate_permits"]*1000/settings["delay"],
            "grid":manifest["grid"],"host":manifest.get("host"),"ladders":ladders,"ratios_vs_local_two":ratios(ladders,trials),"trials":trials}

def table(s):
    slo=s["slo"];host=s["host"] or {}
    print(f"{s['workload']}: SLO success >= {slo['success_ratio']:.1%}, scheduled p99 <= {s['p99_limit_ms']} ms, no drops; backend {s['delay_ms']} ms, {s['backend_capacity_fills_per_second']:.0f} fills/s; "
          +(f"host ceiling {host['ceiling']}/s (half {host['ceiling']/2:.0f})" if host.get("ceiling") else "no host ceiling recorded"))
    print(f"{'mode':<11} {'ceiling':>7} {'conf':>4} {'1st fail':>8} {'bound':<19} {'calls/req':>9} {'util':>5} {'p99 ms':>6} {'cpu':>5} {'pred':>5} {'pred fail':>9}  failing checks / causes at first failure")
    for mode,l in s["ladders"].items():
        c=l["at_ceiling"] or {};fmt=lambda v,spec:format(v,spec) if v is not None else "-"
        details=[]
        for f in l["failing_trials"]:
            if f["rate"]!=l["first_failing_rate"]:continue
            broken=[k for k,ok in f["checks"].items() if not ok];causes={k:v for k,v in f["failure_causes"].items() if v}
            details.append(f"t{f['trial']}: {','.join(broken)} success {f['success_ratio']:.3%} p99 {f['scheduled_p99_ms']:.0f} drops {f['generator_drops']} {causes}")
        conf="open" if not l["complete"] else f"{l['confirmations']}" if l["ceiling"] else "-"
        print(f"{mode:<11} {fmt(l['ceiling'],'d'):>7} {conf:>4} {fmt(l['first_failing_rate'],'d'):>8} {l['bound']:<19} {fmt(c.get('backend_calls_per_request'),'.3f'):>9} {fmt(c.get('backend_utilization'),'.0%'):>5} {fmt(c.get('scheduled_p99_ms'),'.0f'):>6} {fmt(c.get('cpu_cores'),'.1f'):>5} {fmt(l['predicted_ceiling'],'d'):>5} {fmt(l['predicted_first_failure'],'d'):>9}  {'; '.join(details)}")
    for mode,r in s["ratios_vs_local_two"].items():
        inverse=f", inverse backend calls/request at {r['at_rate']}/s {r['inverse_backend_call_ratio']:.2f}" if r["inverse_backend_call_ratio"] else ""
        predicted=f" (predicted {r['predicted_ratio']:.2f})" if r["predicted_ratio"] else ""
        caveat="" if r["capacity_ratio"] else f" [{mode} {r['bounds'][0]}, local-two {r['bounds'][1]}: not a capacity ratio]"
        print(f"  {mode} / local-two ceiling {r['ceiling_ratio']:.2f}{predicted}{inverse}{caveat}")

def main():
    parser=argparse.ArgumentParser(description=__doc__,formatter_class=argparse.RawDescriptionHelpFormatter);parser.add_argument("folder",type=Path);args=parser.parse_args()
    s=summarize(args.folder);(args.folder/"summary.json").write_text(json.dumps(s,indent=2));table(s)
    print("Reconciled",len(s["trials"]),"trials; summary:",args.folder/"summary.json",flush=True)

if __name__=="__main__":main()
