"""Reconcile V2.3 burst, failure/rejoin, and fixed-budget steady records."""
import argparse
import json
import statistics
from datetime import datetime, timedelta
from pathlib import Path

def read(p):
    return json.loads(p.read_text(encoding='utf-8-sig'))
def lines(p):
    return [json.loads(s) for s in p.read_text(encoding='utf-8-sig').splitlines() if s.strip()]
def burst(p):
    value=read(p)
    assert value['Requests']==100 and value['TransportErrors']==0, (p,value)
    assert value['BodiesConsistent'], (p,'body consistency')
    assert sum(value['StatusCounts'].values())==100
    return value

def delta(a,b):
    return {k:v-b[k] for k,v in a.items()}

def summarize(folder):
    m=read(folder/'manifest.json')
    assert (folder/'status.txt').read_text(encoding='utf-8-sig').strip()=='completed'
    assert m['perGatewayPermits']*m['gateways']==m['aggregatePermits']==32
    assert m['perGatewayWaiters']*m['gateways']==m['aggregateWaiters']==32
    trials=[]
    for i in range(1,m['trials']+1):
        before,after=(read(folder/f'{i}-{key}.json') for key in ['before','after'])
        assert after['backend']['peak']<=m['aggregatePermits']
        assert after['backend']['active']==0
        if m['scenario']=='burst':
            r=burst(folder/f'{i}-generator.stdout')
            assert r['StatusCounts']=={'200':100}
            assert after['backend']['calls']-before['backend']['calls']==m['gateways']
            for name,gw in after['gateways'].items():
                c=delta(gw['counters'],before['gateways'][name]['counters'])
                assert c['incoming']==c['completed']==100//m['gateways']
                assert c['backend_attempts']==1
                assert c['cache_misses']==100//m['gateways'] and c['local_followers']==100//m['gateways']-1
                assert all(v==0 for v in gw['gauges'].values())
            if not m['direct']:
                assert after['front']['Requests']==after['front']['Completed']==100
                assert all(t['Selected']==100//m['gateways'] for t in after['front']['Targets'])
            trials.append({'trial':i,'backend_calls':after['backend']['calls'],'burst':r})
        elif m['scenario']=='failure':
            primed=read(folder/f'{i}-before-stop.json');survivor=read(folder/f'{i}-survivor.json');rejoining=read(folder/f'{i}-before-rejoin.json')
            a=burst(folder/f'{i}-generator-before-stop.stdout');transition=burst(folder/f'{i}-generator-transition.stdout');b=burst(folder/f'{i}-generator-survivor.stdout');c=burst(folder/f'{i}-generator-rejoined.stdout')
            assert a['StatusCounts']==b['StatusCounts']==c['StatusCounts']=={'200':100}
            assert set(transition['StatusCounts'])<= {'200','502','503','504'}
            assert primed['backend']['calls']==2 and all(t['Selected']==50 for t in primed['front']['Targets'])
            assert not survivor['front']['Targets'][0]['Healthy'] and survivor['front']['Targets'][1]['Healthy']
            assert survivor['gateways']['gateway-1'].get('unavailable')
            assert all(t['Healthy'] for t in after['front']['Targets'])
            for old,new in zip(rejoining['front']['Targets'],after['front']['Targets']):
                assert new['Selected']-old['Selected']==50
            assert after['gateways']['gateway-1']['counters']['backend_attempts']==1
            assert after['gateways']['gateway-1']['counters']['incoming']==50
            assert after['backend']['calls']==3
            for g in after['gateways'].values():
                assert all(v==0 for v in g['gauges'].values())
            trials.append({'trial':i,'transition_statuses':transition['StatusCounts'],'exclusion_observation':read(folder/f'{i}-exclusion.json'),'backend_calls':after['backend']['calls'],'survivor_success':100,'rejoined_success':100})
        else:
            records=lines(folder/f'{i}-generator.stdout');r=next(r for r in records if r['Phase']=='measurement')
            for phase in records:
                assert phase['Sent']+phase['GeneratorDrops']==phase['Offered']
                assert phase['Completed']==phase['Sent'] and phase['TransportErrors']==0
                assert phase['StatusCounts'].get('200',0)+phase['StatusCounts'].get('503',0)==phase['Sent']
                front_unavailable=0 if m['direct'] else phase['After']['front']['Unavailable']-phase['Before']['front']['Unavailable']
                assert sum(phase['RejectionReasons'].values())+front_unavailable==phase['Rejected']
                counters=[]
                for n in range(1,m['gateways']+1):
                    old,new=phase['Before'][f'gateway_{n}'],phase['After'][f'gateway_{n}']
                    c=delta(new['counters'],old['counters']);counters.append(c)
                    assert c['incoming']==c['completed']==c['cache_misses']
                    assert c['cache_hits']==0
                    assert c['permit_rejections']==c['permit_queue_full']+c['permit_wait_timeout']
                    assert all(v==0 for v in new['gauges'].values())
                    for h in new['histograms'].values():assert sum(h['Buckets'])==h['Count']
                assert sum(c['incoming'] for c in counters)+front_unavailable==phase['Sent']
                assert sum(c['status_2xx'] for c in counters)==phase['Success']
                assert sum(c['status_5xx'] for c in counters)+front_unavailable==phase['Rejected']
                assert sum(c['backend_attempts'] for c in counters)==phase['After']['backend']['calls']-phase['Before']['backend']['calls']
                if not m['direct']:
                    f0,f1=phase['Before']['front'],phase['After']['front']
                    assert f1['Requests']-f0['Requests']==f1['Completed']-f0['Completed']==phase['Sent']
                    assert f1['Active']==0 and f1['Aborted']==f0['Aborted']
                    for j,(t0,t1) in enumerate(zip(f0['Targets'],f1['Targets'])):
                        assert t1['Selected']-t0['Selected']==counters[j]['incoming']
                        assert t1['Errors']==t0['Errors']
            # Limits apply to every sampled instance; backend peak is continuously maintained.
            for sample in lines(folder/f'{i}-topology.jsonl'):
                for g in sample['gateways'].values():
                    assert g['gauges']['active_backend']<=m['perGatewayPermits']
                    assert g['gauges']['permit_waiters']<=m['perGatewayWaiters']
            start=datetime.fromisoformat(r['StartUTC'].replace('Z','+00:00'));end=start+timedelta(seconds=r['ElapsedSeconds'])
            resources=[v for v in lines(folder/f'{i}-resources.jsonl') if start<=datetime.fromisoformat(v['time'].replace('Z','+00:00'))<=end]
            per_process={}
            for role in sorted({v['role'] for v in resources}):
                series=[v for v in resources if v['role']==role]
                per_process[role]={'sample_window_cpu_seconds':series[-1]['cpuSeconds']-series[0]['cpuSeconds'],'max_sampled_rss_mib':max(v['rssBytes'] for v in series)/2**20,'samples':len(series)}
            trials.append({'trial':i,'offered':r['Offered'],'sent':r['Sent'],'success':r['Success'],'rejected':r['Rejected'],'drops':r['GeneratorDrops'],'transport_errors':r['TransportErrors'],'late_arrivals':r['LateArrivals'],'successful_rps':r['Success']/r['ElapsedSeconds'],'p95_ms':r['SendLatencyMS']['p95'],'p99_ms':r['SendLatencyMS']['p99'],'scheduled_p99_ms':r['ScheduledLatencyMS']['p99'],'backend_calls':r['After']['backend']['calls']-r['Before']['backend']['calls'],'backend_peak':r['After']['backend']['peak'],'front_unavailable':0 if m['direct'] else r['After']['front']['Unavailable']-r['Before']['front']['Unavailable'],'front_probe_failures':0 if m['direct'] else sum(t['ProbeFailures'] for t in r['After']['front']['Targets'])-sum(t['ProbeFailures'] for t in r['Before']['front']['Targets']),'gateway_requests':[r['After'][f'gateway_{n}']['counters']['incoming']-r['Before'][f'gateway_{n}']['counters']['incoming'] for n in range(1,m['gateways']+1)],'resources':per_process})
    return {'run':folder.name,'scenario':m['scenario'],'gateways':m['gateways'],'direct':m['direct'],'trials':trials}

if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('folders',nargs='*',type=Path);p.add_argument('--index',type=Path);p.add_argument('--output',type=Path);args=p.parse_args()
    if args.index:args.folders.extend(args.index.parent/item['folder'] for item in lines(args.index))
    if not args.folders:p.error('provide folders or --index')
    result=[summarize(folder) for folder in args.folders]
    if args.output:args.output.write_text(json.dumps(result,indent=2)+'\n',encoding='utf-8')
    for row in result:
        print(row['run'],len(row['trials']),'trials reconciled')
        if row['scenario']=='steady':
            ts=row['trials'];print('median RPS',statistics.median(t['successful_rps'] for t in ts),'503 %',100*sum(t['rejected'] for t in ts)/sum(t['sent'] for t in ts),'p99',statistics.median(t['p99_ms'] for t in ts))
