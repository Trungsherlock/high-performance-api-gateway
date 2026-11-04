"""Physical-core discovery, per-process CPU affinity and power state using OS APIs, without dependencies."""
import os
from pathlib import Path
import re
import struct
import subprocess

GROUPS=("generator","gateways","support")
# Windows 11 power-mode overlays (Settings > Power mode); any other GUID is recorded raw.
OVERLAYS={"961cc777-2547-4f9d-8174-7d86181b8a7a":"best power efficiency","00000000-0000-0000-0000-000000000000":"balanced","ded574b5-45a0-4f42-8737-46345c09c238":"best performance"}

if os.name=="nt":
    import ctypes
    from ctypes import wintypes
    assert ctypes.sizeof(ctypes.c_void_p)==8,"64-bit Python required for 64-bit affinity masks"
    kernel=ctypes.WinDLL("kernel32",use_last_error=True)
    kernel.OpenProcess.argtypes=[wintypes.DWORD,wintypes.BOOL,wintypes.DWORD];kernel.OpenProcess.restype=wintypes.HANDLE
    kernel.CloseHandle.argtypes=[wintypes.HANDLE];kernel.GetCurrentProcess.restype=wintypes.HANDLE
    kernel.SetProcessAffinityMask.argtypes=[wintypes.HANDLE,ctypes.c_size_t]
    kernel.GetProcessAffinityMask.argtypes=[wintypes.HANDLE,ctypes.POINTER(ctypes.c_size_t),ctypes.POINTER(ctypes.c_size_t)]
    kernel.GetLogicalProcessorInformationEx.argtypes=[ctypes.c_int,ctypes.c_void_p,ctypes.POINTER(wintypes.DWORD)]
    class PowerStatus(ctypes.Structure):
        _fields_=[("ACLineStatus",ctypes.c_ubyte),("BatteryFlag",ctypes.c_ubyte),("BatteryLifePercent",ctypes.c_ubyte),("SystemStatusFlag",ctypes.c_ubyte),("BatteryLifeTime",wintypes.DWORD),("BatteryFullLifeTime",wintypes.DWORD)]
    kernel.GetSystemPowerStatus.argtypes=[ctypes.POINTER(PowerStatus)]
    class GUID(ctypes.Structure):
        _fields_=[("Data1",wintypes.DWORD),("Data2",wintypes.WORD),("Data3",wintypes.WORD),("Data4",ctypes.c_ubyte*8)]
        def __str__(self):
            d=bytes(self.Data4);return f"{self.Data1:08x}-{self.Data2:04x}-{self.Data3:04x}-{d[:2].hex()}-{d[2:].hex()}"

def parse(text):
    """Linux cpulist syntax, such as '0-3,8,10-11', as a set of logical CPUs."""
    cpus=set()
    for part in text.strip().split(","):
        if part:
            low,_,high=part.partition("-");cpus.update(range(int(low),int(high or low)+1))
    return cpus

def bits(mask):return [i for i in range(64) if mask>>i&1]

def allowed():
    if os.name!="nt":return os.sched_getaffinity(0)
    current,system=ctypes.c_size_t(),ctypes.c_size_t()
    if not kernel.GetProcessAffinityMask(kernel.GetCurrentProcess(),ctypes.byref(current),ctypes.byref(system)):raise OSError(ctypes.get_last_error(),"GetProcessAffinityMask")
    return set(bits(current.value))

def cores():
    """Physical cores available to this process as [efficiency class, logical CPUs]; higher classes are faster."""
    usable=allowed();result=[]
    if os.name=="nt":
        size=wintypes.DWORD(0);kernel.GetLogicalProcessorInformationEx(0,None,ctypes.byref(size))
        buffer=ctypes.create_string_buffer(size.value)
        if not kernel.GetLogicalProcessorInformationEx(0,buffer,ctypes.byref(size)):raise OSError(ctypes.get_last_error(),"GetLogicalProcessorInformationEx")
        raw=buffer.raw;offset=0
        while offset<size.value:
            # PROCESSOR_RELATIONSHIP after the 8-byte header: Flags, EfficiencyClass, 20 reserved
            # bytes, GroupCount, then 8-byte-aligned GROUP_AFFINITY {Mask, Group}.
            length=struct.unpack_from("<I",raw,offset+4)[0];efficiency=raw[offset+9];count=struct.unpack_from("<H",raw,offset+30)[0]
            mask,group=struct.unpack_from("<QH",raw,offset+32)
            if count!=1 or group!=0:raise OSError("only processor group 0 is supported")
            cpus=[c for c in bits(mask) if c in usable]
            if cpus:result.append([efficiency,cpus])
            offset+=length
        return result
    # Intel hybrid parts list their performance cores here; otherwise every core is one class.
    performance=Path("/sys/devices/cpu_core/cpus");fast=parse(performance.read_text()) if performance.exists() else set()
    seen=set()
    for cpu in sorted(usable):
        if cpu in seen:continue
        siblings=parse(Path(f"/sys/devices/system/cpu/cpu{cpu}/topology/thread_siblings_list").read_text());seen|=siblings
        result.append([1 if cpu in fast else 0,sorted(siblings&usable)])
    return result

def layout(spec):
    """Resolve 'off', 'auto' or 'generator=0-3;gateways=4-9;support=10-11' into disjoint logical-CPU sets."""
    if spec=="off":return None
    available=cores()
    if spec=="auto":
        # Whole physical cores, fastest class first, so no two groups share SMT siblings. CPU 0's
        # core, which usually services the most interrupts, sorts last in its class.
        ordered=sorted(available,key=lambda c:(-c[0],0 in c[1],c[1][0]))
        fastest=[c for c in ordered if c[0]==ordered[0][0]]
        pool=fastest if len(fastest)>=3 else ordered
        if len(pool)<3:raise ValueError("auto affinity needs three physical cores; use --affinity off or explicit sets")
        gateways=len(pool)//2;generator=len(pool)//3
        split={"gateways":pool[:gateways],"generator":pool[gateways:gateways+generator],"support":pool[gateways+generator:]}
        groups={name:sorted(c for core in split[name] for c in core[1]) for name in GROUPS}
    else:
        groups={}
        for part in spec.split(";"):
            name,_,cpus=part.partition("=");groups[name.strip()]=sorted(parse(cpus))
        if sorted(groups)!=sorted(GROUPS) or not all(groups.values()):raise ValueError("affinity needs non-empty generator, gateways and support sets")
        listed=[c for cpus in groups.values() for c in cpus]
        if len(set(listed))!=len(listed):raise ValueError("affinity sets overlap")
        if not set(listed)<=allowed():raise ValueError("affinity names logical CPUs this process cannot use")
    used={c for cpus in groups.values() for c in cpus}
    return {"spec":spec,"groups":groups,"gomaxprocs":{name:len(cpus) for name,cpus in groups.items()},"unpinned_cpus":sorted(allowed()-used),"cores":available}

def launch_options(cpus):
    """Popen arguments that pin a child before exec on Linux, so every thread inherits the set."""
    return {"preexec_fn":lambda:os.sched_setaffinity(0,cpus)} if cpus and os.name!="nt" else {}

def pin(pid,cpus):
    """Restrict a running process to cpus and return the set read back. On Windows the process
    mask also replaces every existing thread's mask; on Linux launch_options already applied it."""
    if os.name!="nt":return sorted(os.sched_getaffinity(pid))
    # PROCESS_SET_INFORMATION | PROCESS_QUERY_LIMITED_INFORMATION
    handle=kernel.OpenProcess(0x1200,False,pid)
    if not handle:raise OSError(ctypes.get_last_error(),"OpenProcess")
    try:
        if not kernel.SetProcessAffinityMask(handle,sum(1<<c for c in cpus)):raise OSError(ctypes.get_last_error(),"SetProcessAffinityMask")
        current,system=ctypes.c_size_t(),ctypes.c_size_t()
        if not kernel.GetProcessAffinityMask(handle,ctypes.byref(current),ctypes.byref(system)):raise OSError(ctypes.get_last_error(),"GetProcessAffinityMask")
        return bits(current.value)
    finally:kernel.CloseHandle(handle)

def power():
    """Active power plan, Windows power-mode overlay and AC line state, as far as the OS reports them."""
    if os.name=="nt":
        status=PowerStatus();ok=kernel.GetSystemPowerStatus(ctypes.byref(status))
        text=subprocess.run(["powercfg","/getactivescheme"],capture_output=True,text=True).stdout
        match=re.search(r"([0-9a-f-]{36})\s+\((.*)\)",text)
        overlay=None
        try:
            powrprof=ctypes.WinDLL("powrprof");guid=GUID()
            if powrprof.PowerGetEffectiveOverlayScheme(ctypes.byref(guid))==0:overlay=str(guid)
        except (OSError,AttributeError):pass
        return {"ac_online":{0:False,1:True}.get(status.ACLineStatus) if ok else None,"battery_percent":status.BatteryLifePercent if ok and status.BatteryLifePercent!=255 else None,
                "power_plan":{"guid":match[1],"name":match[2]} if match else text.strip(),"power_mode_overlay":{"guid":overlay,"name":OVERLAYS.get(overlay)} if overlay else None}
    supplies=[p for p in Path("/sys/class/power_supply").glob("*") if (p/"type").exists() and (p/"type").read_text().strip()=="Mains"]
    def read(path):return path.read_text().strip() if path.exists() else None
    return {"ac_online":any(read(p/"online")=="1" for p in supplies) if supplies else None,"platform_profile":read(Path("/sys/firmware/acpi/platform_profile")),
            "cpu0_governor":read(Path("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor"))}
