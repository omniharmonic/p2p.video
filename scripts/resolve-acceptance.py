#!/usr/bin/env python3
"""Unattended DaVinci Resolve import check for p2p.video editor packages [SL-P0-10].

Builds a synthetic editor package (timelines from the real exporter, via the
Go fixture TestWriteResolveAcceptanceFixture; media rendered here with
ffmpeg), then, in a throwaway Resolve project named
`studiolink-acceptance-<YYYY-MM-DD>`, imports both timelines and checks
track counts, clip counts, positions and durations against the XML. The
throwaway project is deleted afterwards and the previously open project is
reloaded. Writes a JSON report.

    python3 scripts/resolve-acceptance.py [--package DIR] [--report FILE]
                                          [--keep-project] [--allow-switch]
    python3 scripts/resolve-acceptance.py --build-only DIR

Requirements (the script never changes them for you):
  * DaVinci Resolve is running.
  * Scripting access. DaVinci Resolve Studio: Preferences > System > General >
    "External scripting using" = Local. The free edition does not allow
    external scripting: copy this file to
    ~/Library/Application Support/Blackmagic Design/DaVinci Resolve/Fusion/Scripts/Utility/
    and run it from Workspace > Scripts (it then uses the in-app `resolve`).
    In-app runs need --package (build the package beforehand from a shell).
  * The open project, if any, has no unsaved changes: switching projects
    through the API closes it without saving. The script refuses to switch
    away from a project other than "Untitled Project" unless --allow-switch.
  * ffmpeg on PATH (or STUDIO_FFMPEG) and Go, to build the package.

Exit status: 0 pass, 1 a check failed, 2 Resolve not reachable (nothing done).
"""

import argparse
import datetime
import json
import os
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET
from fractions import Fraction

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FPS = 30  # the fixture's frame rate


def build_package(out_dir):
    """Write the timelines with the real exporter, then render matching media."""
    env = dict(os.environ, STUDIOLINK_ACCEPTANCE_PACKAGE=out_dir)
    subprocess.run(["go", "test", "./internal/export", "-run", "TestWriteResolveAcceptanceFixture", "-count=1"],
                   cwd=os.path.join(REPO, "host"), env=env, check=True, stdout=subprocess.DEVNULL)
    ffmpeg = os.environ.get("STUDIO_FFMPEG", "ffmpeg")
    with open(os.path.join(out_dir, "manifest.json")) as f:
        manifest = json.load(f)
    for s in manifest["sources"]:
        path = os.path.join(out_dir, s["file"])
        os.makedirs(os.path.dirname(path), exist_ok=True)
        dur = "%.6f" % s["durationSec"]
        if s["kind"] == "video":
            size = "%dx%d" % (s.get("width") or 1920, s.get("height") or 1080)
            args = [ffmpeg, "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=%s:rate=%d" % (size, FPS),
                    "-t", dur, "-c:v", "prores_ks", "-profile:v", "0", "-pix_fmt", "yuv422p10le", "-an",
                    "-timecode", "00:00:00:00", path]
        else:
            args = [ffmpeg, "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
                    "-t", dur, "-ac", str(s.get("channels") or 2), "-c:a", "pcm_s24le", path]
        subprocess.run(args, check=True)
    return manifest


def frames(t):
    """FCPXML rational time ("1001/30000s", "3s", "0s") as a Fraction of seconds."""
    return Fraction(t[:-1]) if t.endswith("s") else Fraction(t)


def expected_premiere(path):
    """Per track kind: list of (start, duration) in frames, from the FCP7 XML."""
    root = ET.parse(path).getroot()
    out = {"video": [], "audio": []}
    for kind in ("video", "audio"):
        for track in root.find("sequence/media/" + kind).findall("track"):
            for c in track.findall("clipitem"):
                out[kind].append((int(c.findtext("start")), int(c.findtext("out")) - int(c.findtext("in"))))
    return out, int(root.findtext("sequence/duration"))


def expected_fcpxml(path):
    """The multicam clip (start, duration) and each connected microphone's
    exact position (offset minus head trim, fractional frames) and duration."""
    root = ET.parse(path).getroot()
    seq = root.find("library/event/project/sequence")
    spine = seq.find("spine")
    primary = spine[0]
    mc = None
    if primary.tag == "mc-clip":
        mc = (0, frames(primary.get("duration")) * FPS)
    mics = []
    for clip in primary.findall("asset-clip"):
        at = (frames(clip.get("offset")) - frames(clip.get("start"))) * FPS
        mics.append((at, frames(clip.get("duration")) * FPS))
    return mc, mics, frames(seq.get("duration")) * FPS


def connect():
    try:
        return resolve  # noqa: F821 - provided when run from Workspace > Scripts
    except NameError:
        pass
    api = os.environ.get("RESOLVE_SCRIPT_API", "/Library/Application Support/Blackmagic Design/DaVinci Resolve/Developer/Scripting")
    os.environ.setdefault("RESOLVE_SCRIPT_LIB", "/Applications/DaVinci Resolve/DaVinci Resolve.app/Contents/Libraries/Fusion/fusionscript.so")
    sys.path.append(os.path.join(api, "Modules"))
    try:
        import DaVinciResolveScript as dvr  # type: ignore
    except ImportError as e:
        return "cannot load DaVinciResolveScript: %s" % e
    app = dvr.scriptapp("Resolve")
    return app or ("Resolve is not reachable: start DaVinci Resolve and allow scripting "
                   "(Studio: Preferences > System > General > External scripting using = Local; "
                   "free edition: run this script from Workspace > Scripts)")


def item_rows(tl, kind):
    rows = []
    start = tl.GetStartFrame()
    for i in range(1, tl.GetTrackCount(kind) + 1):
        for it in tl.GetItemListInTrack(kind, i) or []:
            rows.append({"track": i, "name": it.GetName(),
                         "start": it.GetStart(True) - start, "duration": it.GetDuration(True)})
    return rows


def check(report, name, ok, detail):
    report["checks"].append({"check": name, "ok": bool(ok), "detail": detail})
    print(("PASS " if ok else "FAIL ") + name + ": " + json.dumps(detail, default=str))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--package", help="existing package directory (default: build a synthetic one)")
    ap.add_argument("--report", help="JSON report path")
    ap.add_argument("--keep-project", action="store_true", help="leave the throwaway project for inspection")
    ap.add_argument("--allow-switch", action="store_true", help="close the open project (unsaved changes are lost)")
    ap.add_argument("--build-only", metavar="DIR", help="only build the synthetic package into DIR (no Resolve)")
    args = ap.parse_args()
    if args.build_only:
        build_package(os.path.abspath(args.build_only))
        print(os.path.abspath(args.build_only))
        return 0

    app = connect()
    report = {"when": datetime.datetime.now().isoformat(timespec="seconds"), "checks": []}
    if isinstance(app, str):
        print(app, file=sys.stderr)
        report["error"] = app
        if args.report:
            with open(args.report, "w") as f:
                json.dump(report, f, indent=1)
        return 2
    report["resolve"] = "%s %s" % (app.GetProductName(), app.GetVersionString())

    pkg = args.package or tempfile.mkdtemp(prefix="studiolink-acceptance-")
    if not args.package:
        build_package(pkg)
    report["package"] = pkg
    xmeml = next(os.path.join(pkg, f) for f in os.listdir(pkg) if f.endswith("_premiere.xml"))
    fcpxml = next(os.path.join(pkg, f) for f in os.listdir(pkg) if f.endswith(".fcpxml"))

    pm = app.GetProjectManager()
    previous = pm.GetCurrentProject()
    prev_name = previous.GetName() if previous else None
    if prev_name and prev_name != "Untitled Project" and not args.allow_switch:
        msg = "project %r is open; switching would close it without saving. Save and close it, or pass --allow-switch" % prev_name
        print(msg, file=sys.stderr)
        report["error"] = msg
        return 2
    name = "studiolink-acceptance-" + datetime.date.today().isoformat()
    project = pm.CreateProject(name)
    if not project:
        msg = "could not create %r (a project with that name exists?); not touching it" % name
        print(msg, file=sys.stderr)
        report["error"] = msg
        return 2
    report["project"] = name
    failed = False
    try:
        project.SetSetting("timelineFrameRate", str(FPS))
        pool = project.GetMediaPool()

        tl = pool.ImportTimelineFromFile(xmeml, {"timelineName": "premiere", "importSourceClips": True, "sourceClipsPath": pkg})
        want, seq_frames = expected_premiere(xmeml)
        check(report, "FCP7 XML imports", tl is not None, xmeml)
        if tl:
            v, a = item_rows(tl, "video"), item_rows(tl, "audio")
            check(report, "FCP7 video tracks/clips", tl.GetTrackCount("video") == len(want["video"]) and len(v) == len(want["video"]), {"tracks": tl.GetTrackCount("video"), "items": v})
            check(report, "FCP7 audio tracks/clips", tl.GetTrackCount("audio") == len(want["audio"]) and len(a) == len(want["audio"]), {"tracks": tl.GetTrackCount("audio"), "items": a})
            for kind, got in (("video", v), ("audio", a)):
                pairs = sorted((round(r["start"]), round(r["duration"])) for r in got)
                check(report, "FCP7 %s positions and durations" % kind, pairs == sorted(want[kind]), {"got": pairs, "want": sorted(want[kind])})
            check(report, "FCP7 timeline length", tl.GetEndFrame() - tl.GetStartFrame() in (seq_frames, seq_frames + 1), {"got": tl.GetEndFrame() - tl.GetStartFrame(), "want": seq_frames})
            report["premiere"] = {"video": v, "audio": a}

        tl2 = pool.ImportTimelineFromFile(fcpxml, {"timelineName": "multicam", "importSourceClips": True, "sourceClipsPath": pkg})
        mc, mics, seq_len = expected_fcpxml(fcpxml)
        check(report, "FCPXML imports", tl2 is not None, fcpxml)
        if tl2:
            v, a = item_rows(tl2, "video"), item_rows(tl2, "audio")
            check(report, "FCPXML multicam clip", mc is None or (len(v) == 1 and abs(v[0]["duration"] - mc[1]) <= 1), {"items": v, "want": str(mc)})
            check(report, "FCPXML every microphone on its own track", len(a) == len(mics) and len({r["track"] for r in a}) == len(mics), {"items": a})
            got = sorted((r["start"], r["duration"]) for r in a)
            want_m = sorted((float(s), float(d)) for s, d in mics)
            frame_ok = len(got) == len(want_m) and all(abs(g[0] - w[0]) <= 0.5 and abs(g[1] - w[1]) <= 1 for g, w in zip(got, want_m))
            subframe_ok = frame_ok and all(abs(g[0] - w[0]) <= 0.01 for g, w in zip(got, want_m))
            check(report, "FCPXML microphone positions (within half a frame)", frame_ok, {"got": got, "want": want_m})
            report["fcpxmlSubframePreserved"] = subframe_ok
            print("INFO sub-frame microphone placement preserved by Resolve: %s" % subframe_ok)
            report["multicam"] = {"video": v, "audio": a, "sequenceFrames": float(seq_len)}
        failed = any(not c["ok"] for c in report["checks"])
    finally:
        if not args.keep_project:
            pm.CloseProject(project)
            report["deleted"] = bool(pm.DeleteProject(name))
            if prev_name:
                pm.LoadProject(prev_name)
        if args.report:
            with open(args.report, "w") as f:
                json.dump(report, f, indent=1, default=str)
    print("RESULT", "FAIL" if failed else "PASS")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
