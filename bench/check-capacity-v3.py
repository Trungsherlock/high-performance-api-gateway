"""Post-hoc model check for a V3.4 capacity run. predictions.json, written before the run,
predicts each rung from the climb trial's seed only. This replays every measured trial's own
seed (retries and confirmations included) through predict-v3.py's backend-limit model and
compares rejected requests and SLO verdicts. Written to model-check.json; run it after the
run, never during one (it competes for CPU)."""
import argparse
import importlib.util
import json
from pathlib import Path

def load(name,file):
    spec=importlib.util.spec_from_file_location(name,Path(__file__).with_name(file));module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
summary=load("capacity_summary","summarize-capacity-v3.py")
predict=summary.predict

def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument("folder",type=Path);args=parser.parse_args()
    manifest=json.loads((args.folder/"manifest.json").read_text());settings=manifest["settings"];budget=manifest["budget"]
    s=json.loads((args.folder/"summary.json").read_text())
    options=argparse.Namespace(workload=settings["workload"],keys=settings["keys"],zipf_s=settings["zipf_s"],hot_keys=settings["hot_keys"],seconds=settings["seconds"],warmup=settings["warmup"],
                               max_age=settings["max_age"],delay=settings["delay"],cache_entries=settings["cache_entries"],gateways=settings["gateways"],permits=budget["aggregate_permits"],
                               permit_queue=budget["aggregate_permit_queue"],permit_wait=manifest["permit_wait_ms"],flights=budget["aggregate_cache_flights"],slo_p99_ms=settings.get("slo_p99_ms"))
    rows=[];cache={}
    for t in s["trials"]:
        model=predict.MODEL_FOR_MODE[t["mode"]];key=(model,t["rate"],t["trial"])
        if key not in cache:cache[key]=predict.rung(options,t["rate"],model,t["trial"])
        p=cache[key];measured=t["sent"]-t["success"]
        rows.append({"mode":t["mode"],"rate":t["rate"],"trial":t["trial"],"seed":42+t["trial"],"measured_failed":measured,"measured_passed":t["verdict"]["passed"],
                     "predicted_rejected":sum(p["rejected"].values()),"predicted_passed":p["passes"],"measured_backend_calls":t["backend_calls"],"predicted_fills":p["fills"]})
    agree=sum(r["measured_passed"]==r["predicted_passed"] for r in rows)
    (args.folder/"model-check.json").write_text(json.dumps({"note":__doc__,"trials":rows,"verdicts_agree":agree,"trials_checked":len(rows)},indent=2))
    print(f"{'mode':<10} {'rate':>5} {'t':>2} {'failed':>7} {'model':>6} {'verdict':>8} {'model':>6} {'fills':>6} {'model':>6}")
    for r in rows:
        print(f"{r['mode']:<10} {r['rate']:>5} {r['trial']:>2} {r['measured_failed']:>7} {r['predicted_rejected']:>6} {'pass' if r['measured_passed'] else 'FAIL':>8} {'pass' if r['predicted_passed'] else 'FAIL':>6} {r['measured_backend_calls']:>6} {r['predicted_fills']:>6}")
    print(f"SLO verdicts agree in {agree} of {len(rows)} trials")

if __name__=="__main__":main()
