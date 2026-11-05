"""Move a run's process logs and Redis configs (*.log, *.conf, in any subfolder) into one logs.zip.
Summaries need only manifest.json, trials.jsonl and *-resources.jsonl, which stay as files.
Usage: python bench/pack_logs.py <run folder> [...]"""
from pathlib import Path
import sys
import zipfile

def pack(folder):
    """Append every log/config under folder to folder/logs.zip, verify the archive, then delete
    the originals. Repeated packing (a resumed run) appends; clashing names get a ~N suffix."""
    folder=Path(folder);archive=folder/"logs.zip"
    files=sorted(p for p in folder.rglob("*") if p.is_file() and p.suffix in (".log",".conf"))
    if not files:return 0
    with zipfile.ZipFile(archive,"a",zipfile.ZIP_DEFLATED) as out:
        names=set(out.namelist());added={}
        for path in files:
            name=base=path.relative_to(folder).as_posix();n=1
            while name in names:n+=1;name=f"{base}~{n}"
            out.write(path,name);names.add(name);added[name]=path.stat().st_size
    with zipfile.ZipFile(archive) as check:
        assert check.testzip() is None,"logs.zip failed its CRC check"
        sizes={i.filename:i.file_size for i in check.infolist()}
        assert all(sizes.get(name)==size for name,size in added.items()),"logs.zip is missing a file"
    for path in files:path.unlink()
    return len(files)

if __name__=="__main__":
    if len(sys.argv)<2:sys.exit(__doc__)
    for arg in sys.argv[1:]:print(arg,"packed",pack(arg),"files into logs.zip")
