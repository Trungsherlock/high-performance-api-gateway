"""Reconcile V3 artifacts, derive per-fill/per-request costs, and summarize trial ranges without discarding errors.
Paired differences compare each mode with local-two in the same block; the A/A copy
(local-two-aa) sets the noise floor below which no difference is called an effect."""
import argparse
from datetime import datetime
import importlib.util
import json
from pathlib import Path
import statistics

spec=importlib.util.spec_from_file_location("final_summary",Path(__file__).with_name("summarize-v27.py"))
final=importlib.util.module_from_spec(spec);spec.loader.exec_module(final)
ratio=final.ratio
CAUSES=("timeout","pool_timeout","connection","other")

def gateway_delta(after,before):
    return {k:sum(n["counters"][k]-o["counters"][k] for n,o in zip(after,before)) for k in after[0]["counters"]}

def summarize(folder,budget,row):
    samples=[json.loads(line) for line in (folder/row["resources"]).read_text().splitlines()]
    nodes=len(row["after"]["gateways"]);assert nodes==row["gateways"]
    for sample in samples:
        for node in sample["gateways"]:
            g=node["gauges"]
            assert 0<=g["active_backend"]<=budget["aggregate_permits"]//nodes and 0<=g["permit_waiters"]<=budget["aggregate_permit_queue"]//nodes
            assert 0<=g["flights"]<=budget["aggregate_cache_flights"]//nodes and 0<=g["cache_waiters"]<=budget["aggregate_cache_waiters"]//nodes
            assert node["cache_entries"]<=budget["cache_entries_per_node"] and node["cache_payload_bytes"]<=budget["cache_payload_bytes_per_node"]
        assert sample["backend"]["active"]<=budget["aggregate_permits"]
    if row["kind"]=="cold":
        r=row["output"][0];assert r["StatusCounts"]=={"200":100} and r["TransportErrors"]==0 and r["BodiesConsistent"],r
        backend=row["after"]["backend"]["calls"]-row["before"]["backend"]["calls"]
        # Controlled overlap: one fill per reached gateway unless leases coordinate them.
        assert backend==(1 if row["mode"] in ("local-one","leased") else nodes),backend
        counters=gateway_delta(row["after"]["gateways"],row["before"]["gateways"])
        assert counters["incoming"]==counters["completed"]==100 and counters["backend_attempts"]==backend
        return {"mode":row["mode"],"kind":"cold","trial":row["trial"],"gateways":nodes,"backend_calls":backend,"duration_seconds":r["ElapsedSeconds"],"local_followers":counters["local_followers"],"distributed_followers":counters["distributed_followers"],"power":row.get("power")}
    r=next(x for x in row["output"] if x["Phase"]=="measurement")
    assert r["Offered"]==r["Sent"]+r["GeneratorDrops"] and r["Sent"]==r["Completed"]==sum(r["StatusCounts"].values())
    before,after=r["Before"],r["After"]
    assert not any(k.endswith("_error") for k in before|after), (before,after)
    gateways=[k for k in before if k.startswith("gateway_")];assert len(gateways)==nodes
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
    causes={k:counters["dependency_error_"+k] for k in CAUSES}
    assert sum(causes.values())==counters["dependency_errors"],(causes,counters["dependency_errors"])
    dependencies=sum(after[n]["histograms"]["cache_dependency"]["Count"]-before[n]["histograms"]["cache_dependency"]["Count"] for n in gateways)
    backend=counters["backend_attempts"]
    start=datetime.fromisoformat(r["StartUTC"]).timestamp();end=start+r["ElapsedSeconds"]
    return {"mode":row["mode"],"kind":row["kind"],"rate":row["rate"],"trial":row["trial"],"gateways":nodes,"offered":r["Offered"],"sent":r["Sent"],"success":r["Success"],"successful_rps":r["Success"]/r["ElapsedSeconds"],
            "generator_drops":r["GeneratorDrops"],"late_arrivals":r["LateArrivals"],"transport_errors":r["TransportErrors"],"rejected":r["Rejected"],"rejection_reasons":r["RejectionReasons"],"degraded_reasons":r.get("DegradedReasons",{}),"front_unavailable":front["Unavailable"],
            "send_latency_ms":r["SendLatencyMS"],"scheduled_latency_ms":r["ScheduledLatencyMS"],"scheduling_lag_ms":r["SchedulingLagMS"],"backend_calls":backend,"backend_peak_process_lifetime":after["backend"]["peak"],
            "cache_l1_hits":counters["cache_hits"],"cache_misses":counters["cache_misses"],"l1_hit_ratio":ratio(counters["cache_hits"],counters["cache_hits"]+counters["cache_misses"]),
            "shared_hits":counters["shared_hits"],"shared_misses":counters["shared_misses"],"shared_lookup_hit_ratio":ratio(counters["shared_hits"],counters["shared_hits"]+counters["shared_misses"]),
            "dependency_operations":dependencies,"dependency_errors":counters["dependency_errors"],"dependency_error_causes":causes,
            # Each backend call is one fill in these eligible-GET workloads; requests are client requests sent.
            "dependency_ops_per_fill":ratio(dependencies,backend),"dependency_ops_per_request":ratio(dependencies,r["Sent"]),"backend_calls_per_request":ratio(backend,r["Sent"]),
            "local_followers":counters["local_followers"],"distributed_followers":counters["distributed_followers"],"degraded_flights":counters["degraded_flights"],"resources":final.resources(samples,start,end),"power":row.get("power")}

def spread(values):
    values=[v for v in values if v is not None]
    return {"median":statistics.median(values),"min":min(values),"max":max(values)} if values else None

METRICS=[("successful_rps",lambda x:x["successful_rps"]),("send_p50_ms",lambda x:x["send_latency_ms"]["p50"]),("send_p95_ms",lambda x:x["send_latency_ms"]["p95"]),
         ("send_p99_ms",lambda x:x["send_latency_ms"]["p99"]),("scheduled_p99_ms",lambda x:x["scheduled_latency_ms"]["p99"]),("backend_calls",lambda x:x["backend_calls"]),
         ("dependency_operations",lambda x:x["dependency_operations"]),("l1_hit_ratio",lambda x:x["l1_hit_ratio"]),("shared_lookup_hit_ratio",lambda x:x["shared_lookup_hit_ratio"]),
         ("dependency_ops_per_fill",lambda x:x["dependency_ops_per_fill"]),("dependency_ops_per_request",lambda x:x["dependency_ops_per_request"]),("backend_calls_per_request",lambda x:x["backend_calls_per_request"])]
COUNTS=("generator_drops","late_arrivals","transport_errors","rejected","dependency_errors","front_unavailable")
PAIRED=[m for m in METRICS if m[0] in ("send_p50_ms","send_p95_ms","send_p99_ms","scheduled_p99_ms","backend_calls")]

def paired(results,rate,modes):
    """Per-block differences from local-two at one rate, and the A/A noise floor when local-two-aa ran."""
    steady={(v["mode"],v["trial"]):v for v in results if v["kind"]=="steady" and v["rate"]==rate}
    diffs={}
    for mode in modes:
        blocks=sorted(t for m,t in steady if m==mode and ("local-two",t) in steady)
        if mode!="local-two" and blocks:
            diffs[mode]={"blocks":blocks,**{key:[fn(steady[mode,t])-fn(steady["local-two",t]) for t in blocks] for key,fn in PAIRED}}
    floor=None
    if "local-two-aa" in diffs:
        aa=diffs["local-two-aa"]
        floor={key:{"max_abs":max(abs(d) for d in aa[key]),"median_abs":statistics.median(abs(d) for d in aa[key]),"blocks":len(aa["blocks"])} for key,_ in PAIRED}
    out={}
    for mode,d in diffs.items():
        out[mode]={"blocks":d["blocks"]}
        for key,_ in PAIRED:
            # Effect only if the median paired difference exceeds every A/A difference seen.
            out[mode][key]={"per_block":d[key],**spread(d[key]),"positive_blocks":sum(v>0 for v in d[key]),
                            "beyond_noise":abs(statistics.median(d[key]))>floor[key]["max_abs"] if floor and mode!="local-two-aa" else None}
    return out,floor

def label(key):return key.removesuffix("_ms").removeprefix("send_").replace("scheduled_","sched ").replace("_"," ")

def table(groups,cold,pairs,floors):
    def median(g,key,spec):
        return format(g[key]["median"],spec) if g[key] else "-"
    print("Medians across steady trials; dependency errors by cause (timeout/pool/connection/other) are totals.")
    print(f"{'rate':>6} {'mode':<12} {'n':>2} {'ok rps':>8} {'p95 ms':>7} {'p99 ms':>7} {'backend':>8} {'ops/fill':>8} {'ops/req':>7} {'dep errors':>11}")
    for rate,modes in groups.items():
        for mode,g in modes.items():
            causes="/".join(str(g["dependency_error_causes"][k]) for k in CAUSES)
            print(f"{rate:>6} {mode:<12} {g['trials']:>2} {median(g,'successful_rps','.2f'):>8} {median(g,'send_p95_ms','.2f'):>7} {median(g,'send_p99_ms','.2f'):>7} {median(g,'backend_calls','.0f'):>8} {median(g,'dependency_ops_per_fill','.2f'):>8} {median(g,'dependency_ops_per_request','.3f'):>7} {causes:>11}")
        floor=floors.get(rate)
        if floor:
            print(f"  noise floor at {rate}/s, largest |local-two-aa - local-two| over {floor['send_p95_ms']['blocks']} blocks: "+", ".join(f"{label(k)} {v['max_abs']:.2f}" for k,v in floor.items()))
        else:print(f"  no A/A control at {rate}/s, so no noise floor: do not call latency differences effects")
        if pairs.get(rate):
            print(f"  paired vs local-two, median [min, max] per block; * beyond the noise floor")
            for mode,d in pairs[rate].items():
                cells=[f"{label(k)} {d[k]['median']:+.2f}{'*' if d[k]['beyond_noise'] else ''} [{d[k]['min']:+.2f}, {d[k]['max']:+.2f}]" for k,_ in PAIRED]
                print(f"    {mode:<13}"+"  ".join(cells))
    for mode,calls in cold.items():
        print(f"cold {mode}: backend calls by trial {calls}")

def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument("folder",type=Path);args=parser.parse_args();folder=args.folder
    manifest=json.loads((folder/"manifest.json").read_text());settings=manifest["settings"]
    rows=[json.loads(line) for line in (folder/"trials.jsonl").read_text().splitlines()]
    results=[summarize(folder,manifest["budget"],row) for row in rows]
    groups={}
    for rate in settings["rates"]:
        for mode in settings["modes"]:
            values=[v for v in results if v["kind"]=="steady" and v["mode"]==mode and v["rate"]==rate]
            if not values:continue
            group={"trials":len(values),"gateways":values[0]["gateways"]}
            group.update({key:spread([fn(v) for v in values]) for key,fn in METRICS})
            group.update({key:[v[key] for v in values] for key in COUNTS})
            group["dependency_error_causes"]={k:sum(v["dependency_error_causes"][k] for v in values) for k in CAUSES}
            groups.setdefault(str(rate),{})[mode]=group
    cold={mode:[v["backend_calls"] for v in results if v["kind"]=="cold" and v["mode"]==mode] for mode in settings["modes"]}
    cold={k:v for k,v in cold.items() if v}
    pairs,floors={},{}
    for rate in groups:
        pairs[rate],floors[rate]=paired(results,int(rate),settings["modes"])
    power=[v["power"] for v in results if v["power"]];battery=sum(p["ac_online"] is not True for p in power)
    host={"affinity":manifest.get("affinity"),"power_at_start":manifest.get("power"),"trials_with_power_record":len(power),"trials_not_on_ac":battery}
    (folder/"summary.json").write_text(json.dumps({"groups":groups,"paired_vs_local_two":pairs,"noise_floor":floors,"cold_backend_calls":cold,"host":host,"trials":results},indent=2))
    table(groups,cold,pairs,floors)
    if host["affinity"]:print("CPU sets:",", ".join(f"{k} {v}" for k,v in host["affinity"]["groups"].items()))
    else:print("CPU sets: none (processes unpinned)")
    if power:print(f"Power: {len(power)} trials recorded, {battery} not confirmed on AC; plan at start {manifest['power'].get('power_plan')}")
    print("Reconciled",len(results),"trials; summary:",folder/"summary.json",flush=True)

if __name__=="__main__":main()
