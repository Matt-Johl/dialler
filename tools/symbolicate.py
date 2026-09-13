#!/usr/bin/env python3
"""Symbolicate an iOS crash/CPU report (.ips, JSON or legacy text) or a
MetricKit diagnostic (JSON) against the build symbols kept by
tools/keep-build-symbols.sh in ios/dsyms/<UUID>/.

    make symbolicate FILE=path/to/report

Frames whose UUID is not in ios/dsyms are printed as-is (system libraries)."""
import json, os, re, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DSYMS = os.path.join(ROOT, "ios", "dsyms")


def binary_for(uuid):
    d = os.path.join(DSYMS, uuid.upper())
    if not os.path.isdir(d):
        return None
    for name in sorted(os.listdir(d)):
        p = os.path.join(d, name)
        if name.endswith(".dSYM"):
            inner = os.path.join(p, "Contents", "Resources", "DWARF")
            if os.path.isdir(inner):
                for f in os.listdir(inner):
                    return os.path.join(inner, f)
        elif os.path.isfile(p):
            return p
    return None


def text_vmaddr(binary):
    out = subprocess.run(["otool", "-l", binary], capture_output=True, text=True).stdout
    seg = False
    for line in out.splitlines():
        if "segname __TEXT" in line:
            seg = True
        elif seg and "vmaddr" in line:
            return int(line.split()[1], 16)
    return 0


_cache = {}


def symbolicate(uuid, offset):
    b = binary_for(uuid)
    if not b:
        return None
    key = (b, offset)
    if key in _cache:
        return _cache[key]
    base = text_vmaddr(b)
    out = subprocess.run(["atos", "-o", b, "-arch", "arm64", "-l", hex(base), hex(base + offset)],
                         capture_output=True, text=True).stdout.strip()
    _cache[key] = out
    return out


def do_ips(raw):
    header, _, body = raw.partition("\n")
    try:
        j = json.loads(body)
    except json.JSONDecodeError:
        return do_legacy(raw)
    images = j.get("usedImages", [])
    for ti, t in enumerate(j.get("threads", [])):
        mark = " (crashed)" if t.get("triggered") else ""
        print(f"Thread {ti}{mark} {t.get('name', '')} {t.get('queue', '')}")
        for fr in t.get("frames", []):
            idx = fr.get("imageIndex")
            img = images[idx] if idx is not None and idx < len(images) else {}
            off = fr.get("imageOffset", 0)
            sym = symbolicate(img.get("uuid", ""), off) or fr.get("symbol") or "???"
            print(f"  {img.get('name', '?')} + {off}  {sym}")


def do_legacy(raw):
    pat = re.compile(r"\(<([0-9A-Fa-f-]{36})> \+ (\d+)\)")
    for line in raw.splitlines():
        m = pat.search(line)
        if m:
            sym = symbolicate(m.group(1), int(m.group(2)))
            if sym:
                line = line.replace("???", sym, 1)
        print(line)


def do_metrickit(j):
    def walk(frame, depth):
        sym = symbolicate(frame.get("binaryUUID", ""), frame.get("offsetIntoBinaryTextSegment", 0)) or "???"
        print("  " * depth + f"{frame.get('binaryName', '?')} + {frame.get('offsetIntoBinaryTextSegment', 0)}  {sym}  x{frame.get('sampleCount', '')}")
        for sub in frame.get("subFrames", []):
            walk(sub, depth + 1)
    for kind in ("crashDiagnostics", "cpuExceptionDiagnostics", "hangDiagnostics", "diskWriteExceptionDiagnostics"):
        for d in j.get(kind, []):
            print(f"== {kind}: {json.dumps(d.get('diagnosticMetaData', {}))}")
            for cs in d.get("callStackTree", {}).get("callStacks", []):
                print(f"-- call stack (thread attributed: {cs.get('threadAttributed')})")
                for root in cs.get("callStackRootFrames", []):
                    walk(root, 1)


def main():
    if len(sys.argv) != 2:
        print(__doc__); sys.exit(2)
    raw = open(sys.argv[1], encoding="utf-8", errors="replace").read()
    stripped = raw.lstrip()
    if stripped.startswith("{") and "callStackTree" in raw:
        do_metrickit(json.loads(stripped))
    elif stripped.startswith("{"):
        do_ips(raw)
    else:
        do_legacy(raw)


if __name__ == "__main__":
    main()
