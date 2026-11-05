"""Reconcile final comparison artifacts and summarize trial ranges without discarding errors."""
import argparse
from datetime import datetime
import json
from pathlib import Path
import statistics

def delta(a,b):return {k:a[k]-b.get(k,0) for k in a}
def ratio(n,d):return n/d if d else None

def resources(samples,start,end):
    rows=[r for r in samples if start<=r["epoch"]<=end]
    roles=sorted({role for row in rows for role in row["processes"]})
    out={}
    for role in roles:
        values=[(r["epoch"],r["processes"][role]) for r in rows if role in r["processes"] and "error" not in r["processes"][role]]
        if not values:continue
        cpu=values[-1][1]["cpu_seconds"]-values[0][1]["cpu_seconds"] if len(values)>1 else None
        early=[v["rss_bytes"] for t,v in values if t<start+min(60,(end-start)/4)]
        late=[v["rss_bytes"] for t,v in values if t>end-min(60,(end-start)/4)]
        out[role]={"samples":len(values),"sampled_cpu_seconds":cpu,"sampled_span_seconds":values[-1][0]-values[0][0],"max_sampled_rss_mib":max(v["rss_bytes"] for _,v in values)/2**20,
                   "early_median_rss_mib":statistics.median(early)/2**20 if early else None,"late_median_rss_mib":statistics.median(late)/2**20 if late else None}
    return out

def summarize(folder,row):
    samples=[json.loads(line) for line in (folder/row["resources"]).read_text().splitlines()]
    nodes=len(row["after"]["gateways"])
    for sample in samples:
        for node in sample["gateways"]:
            g=node["gauges"]
            assert 0<=g["active_backend"]<=32//nodes and 0<=g["permit_waiters"]<=32//nodes
            assert 0<=g["flights"]<=64//nodes and 0<=g["cache_waiters"]<=1024//nodes
            assert node["cache_entries"]<=1000 and node["cache_payload_bytes"]<=16*2**20
        assert sample["backend"]["active"]<=32
    if row["kind"]=="cold":
        r=row["output"][0];assert r["StatusCounts"]=={"200":100} and r["TransportErrors"]==0 and r["BodiesConsistent"],r
        backend=row["after"]["backend"]["calls"]-row["before"]["backend"]["calls"]
        assert backend==({"local-one":1,"local-two":2,"shared":2,"leased":1}[row["mode"]]),backend
        counters={k:sum(n["counters"][k]-o["counters"][k] for n,o in zip(row["after"]["gateways"],row["before"]["gateways"])) for k in row["after"]["gateways"][0]["counters"]}
        assert counters["incoming"]==counters["completed"]==100 and counters["backend_attempts"]==backend
        return {"mode":row["mode"],"kind":"cold","trial":row["trial"],"backend_calls":backend,"duration_seconds":r["ElapsedSeconds"],"local_followers":counters["local_followers"],"distributed_followers":counters["distributed_followers"]}
    r=next(x for x in row["output"] if x["Phase"]=="measurement")
    assert r["Offered"]==r["Sent"]+r["GeneratorDrops"] and r["Sent"]==r["Completed"]==sum(r["StatusCounts"].values())
    before,after=r["Before"],r["After"]
    assert not any(k.endswith("_error") for k in before|after), (before,after)
    gateways=[k for k in before if k.startswith("gateway_")]
    counters={k:sum(after[n]["counters"][k]-before[n]["counters"][k] for n in gateways) for k in before[gateways[0]]["counters"]}
    front={k:after["front"][k]-before["front"][k] for k in ("Requests","Completed","Unavailable","Aborted")}
    assert front["Requests"]==front["Completed"]==r["Sent"] and front["Aborted"]==0
    assert counters["incoming"]==counters["completed"]==r["Sent"]-front["Unavailable"]
    assert counters["backend_attempts"]==after["backend"]["calls"]-before["backend"]["calls"]
    assert r["Rejected"]==sum(r["RejectionReasons"].values())+front["Unavailable"]
    for category in (1,2,3,4,5):
        count=sum(v for k,v in r["StatusCounts"].items() if int(k)//100==category)
        assert counters[f"status_{category}xx"]+(front["Unavailable"] if category==5 else 0)==count
    for n in gateways:
        assert all(v==0 for v in after[n]["gauges"].values()),after[n]["gauges"]
    dependencies=sum(after[n]["histograms"]["cache_dependency"]["Count"]-before[n]["histograms"]["cache_dependency"]["Count"] for n in gateways)
    start=datetime.fromisoformat(r["StartUTC"]).timestamp();end=start+r["ElapsedSeconds"]
    return {"mode":row["mode"],"kind":row["kind"],"trial":row["trial"],"offered":r["Offered"],"sent":r["Sent"],"success":r["Success"],"successful_rps":r["Success"]/r["ElapsedSeconds"],"generator_drops":r["GeneratorDrops"],"late_arrivals":r["LateArrivals"],"transport_errors":r["TransportErrors"],"rejected":r["Rejected"],"rejection_reasons":r["RejectionReasons"],"degraded_reasons":r.get("DegradedReasons",{}),"front_unavailable":front["Unavailable"],
            "send_latency_ms":r["SendLatencyMS"],"scheduled_latency_ms":r["ScheduledLatencyMS"],"scheduling_lag_ms":r["SchedulingLagMS"],"backend_calls":counters["backend_attempts"],"backend_peak_process_lifetime":after["backend"]["peak"],
            "cache_l1_hits":counters["cache_hits"],"cache_misses":counters["cache_misses"],"l1_hit_ratio":ratio(counters["cache_hits"],counters["cache_hits"]+counters["cache_misses"]),"shared_hits":counters["shared_hits"],"shared_misses":counters["shared_misses"],"shared_lookup_hit_ratio":ratio(counters["shared_hits"],counters["shared_hits"]+counters["shared_misses"]),
            "dependency_operations":dependencies,"dependency_errors":counters["dependency_errors"],"local_followers":counters["local_followers"],"distributed_followers":counters["distributed_followers"],"degraded_flights":counters["degraded_flights"],"resources":resources(samples,start,end)}

def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument("folder",type=Path);args=parser.parse_args();folder=args.folder
    rows=[json.loads(line) for line in (folder/"trials.jsonl").read_text().splitlines()]
    results=[summarize(folder,row) for row in rows]
    groups={}
    for mode in ("local-one","local-two","shared","leased"):
        values=[v for v in results if v["mode"]==mode and v["kind"]=="steady"]
        if not values:continue
        groups[mode]={"trials":len(values)}
        for key,fn in [("successful_rps",lambda x:x["successful_rps"]),("send_p95_ms",lambda x:x["send_latency_ms"]["p95"]),("send_p99_ms",lambda x:x["send_latency_ms"]["p99"]),("scheduled_p99_ms",lambda x:x["scheduled_latency_ms"]["p99"]),("backend_calls",lambda x:x["backend_calls"]),("dependency_operations",lambda x:x["dependency_operations"]),("l1_hit_ratio",lambda x:x["l1_hit_ratio"])]:
            vals=[fn(v) for v in values];groups[mode][key]={"median":statistics.median(vals),"min":min(vals),"max":max(vals)}
        for key in ("generator_drops","late_arrivals","transport_errors","rejected","dependency_errors","front_unavailable"):
            groups[mode][key]=[v[key] for v in values]
    (folder/"summary.json").write_text(json.dumps({"groups":groups,"trials":results},indent=2))
    print("Reconciled",len(results),"trials; summary:",folder/"summary.json",flush=True)

if __name__=="__main__":main()
