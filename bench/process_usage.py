"""CPU seconds and current resident bytes using OS APIs, without dependencies."""
import os
from pathlib import Path

if os.name == "nt":
    import ctypes
    from ctypes import wintypes
    class Memory(ctypes.Structure):
        _fields_=[("cb",wintypes.DWORD),("PageFaultCount",wintypes.DWORD)]+[(name,ctypes.c_size_t) for name in ("PeakWorkingSetSize","WorkingSetSize","QuotaPeakPagedPoolUsage","QuotaPagedPoolUsage","QuotaPeakNonPagedPoolUsage","QuotaNonPagedPoolUsage","PagefileUsage","PeakPagefileUsage")]
    kernel=ctypes.WinDLL("kernel32",use_last_error=True)
    psapi=ctypes.WinDLL("psapi",use_last_error=True)
    kernel.OpenProcess.argtypes=[wintypes.DWORD,wintypes.BOOL,wintypes.DWORD];kernel.OpenProcess.restype=wintypes.HANDLE
    kernel.CloseHandle.argtypes=[wintypes.HANDLE]
    kernel.GetProcessTimes.argtypes=[wintypes.HANDLE]+[ctypes.POINTER(wintypes.FILETIME)]*4
    psapi.GetProcessMemoryInfo.argtypes=[wintypes.HANDLE,ctypes.POINTER(Memory),wintypes.DWORD]

def process_usage(pid):
    if os.name == "nt":
        handle=kernel.OpenProcess(0x1010,False,pid)
        if not handle:raise OSError(ctypes.get_last_error(),"OpenProcess")
        try:
            times=[wintypes.FILETIME() for _ in range(4)]
            if not kernel.GetProcessTimes(handle,*[ctypes.byref(v) for v in times]):raise OSError(ctypes.get_last_error(),"GetProcessTimes")
            memory=Memory();memory.cb=ctypes.sizeof(memory)
            if not psapi.GetProcessMemoryInfo(handle,ctypes.byref(memory),memory.cb):raise OSError(ctypes.get_last_error(),"GetProcessMemoryInfo")
            ticks=sum((v.dwHighDateTime<<32)+v.dwLowDateTime for v in times[2:])
            return {"cpu_seconds":ticks/10_000_000,"rss_bytes":memory.WorkingSetSize}
        finally:kernel.CloseHandle(handle)
    raw=Path(f"/proc/{pid}/stat").read_text();fields=raw[raw.rindex(")")+2:].split()
    return {"cpu_seconds":(int(fields[11])+int(fields[12]))/os.sysconf("SC_CLK_TCK"),"rss_bytes":int(fields[21])*os.sysconf("SC_PAGE_SIZE")}
