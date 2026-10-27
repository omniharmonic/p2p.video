#!/bin/sh
# Import a p2p.video export package into an ave (agentic-video-editor) episode.
# docs/integrations/CLIP_EDITOR.md [SL-P4-4]
#
#   scripts/ave-import.sh <take folder | editor ZIP> <episode folder> [ave checkout]
#
# The ave checkout defaults to $STUDIOLINK_AVE_DIR, then ~/dev/agentic-video-editor,
# and needs its own venv (run ./scripts/setup.sh there once). An ave with
# `ave import-studiolink` does the whole import. A stock ave gets the manual
# recipe: copy the package into raw/, place work/transcript.json,
# work/transcript.md and work/sync.json, merge the camera mapping into
# episode.json (keys already set are kept), then `ave ingest` and `ave audio`.
# Nothing is moved or deleted in the package.
set -eu
if [ $# -lt 2 ]; then
	sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
fi
PKG=$1
EP=$2
AVE_DIR=${3:-${STUDIOLINK_AVE_DIR:-$HOME/dev/agentic-video-editor}}
AVE=$AVE_DIR/.venv/bin/ave
PY=$AVE_DIR/.venv/bin/python
if [ ! -x "$AVE" ] || [ ! -x "$PY" ]; then
	echo "ave not found in $AVE_DIR (pass the checkout or set STUDIOLINK_AVE_DIR; set it up with ./scripts/setup.sh there)" >&2
	exit 1
fi

if "$AVE" import-studiolink --help >/dev/null 2>&1; then
	exec "$AVE" import-studiolink "$PKG" "$EP"
fi

echo "This ave has no import-studiolink; using the manual recipe." >&2
TMP=""
cleanup() { if [ -n "$TMP" ]; then rm -rf "$TMP"; fi; }
trap cleanup EXIT
case "$PKG" in
*.zip | *.ZIP)
	TMP=$(mktemp -d "${TMPDIR:-/tmp}/studiolink-ave.XXXXXX")
	unzip -q "$PKG" -d "$TMP"
	PKG=$TMP
	;;
esac
"$AVE" init-episode "$EP" >/dev/null
"$PY" -I - "$PKG" "$EP" <<'PY'
import json, shutil, sys
from pathlib import Path, PurePosixPath

pkg, ep = Path(sys.argv[1]).resolve(), Path(sys.argv[2]).resolve()
if not (pkg / "handoff/clip-editor.json").is_file():
    hits = list(pkg.glob("*/handoff/clip-editor.json"))
    if len(hits) != 1:
        sys.exit(f"{pkg} is not a p2p.video export package (no handoff/clip-editor.json)")
    pkg = hits[0].parent.parent
idx = json.loads((pkg / "handoff/clip-editor.json").read_text())
if idx.get("schema") != "studiolink.clip-editor-handoff/1":
    sys.exit(f"unsupported handoff schema {idx.get('schema')!r}")
ave = idx["ave"]

def safe(p):
    q = PurePosixPath(p)
    if q.is_absolute() or ".." in q.parts:
        sys.exit(f"unsafe path in the package: {p!r}")
    return str(q)

for dest, src in sorted(ave["raw"].items()):
    d = ep / safe(dest)
    if d.exists():
        if d.stat().st_size != (pkg / safe(src)).stat().st_size:
            sys.exit(f"{d} exists with different content; raw/ is read-only: use a new episode")
        continue
    d.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(pkg / safe(src), d)
work = ep / "work"
work.mkdir(exist_ok=True)
for key, name in (("sync", "sync.json"), ("transcript", "transcript.json"), ("transcriptMarkdown", "transcript.md")):
    if ave.get(key):
        shutil.copy2(pkg / safe(ave[key]), work / name)
    elif key != "transcriptMarkdown":
        print(f"note: the package has no {key} for ave yet", file=sys.stderr)
if ave.get("episode"):
    meta = json.loads((ep / "episode.json").read_text())
    mapping = json.loads((pkg / safe(ave["episode"])).read_text())
    for k in ("cameras", "speakers", "audio_source"):
        if mapping.get(k) and not meta.get(k):
            meta[k] = mapping[k]
    meta["studiolink"] = {"schema": idx["schema"], "package": str(pkg), "take": idx.get("take"), "mapping": mapping}
    (ep / "episode.json").write_text(json.dumps(meta, indent=2) + "\n")
PY
"$AVE" ingest "$EP" >/dev/null
"$AVE" audio "$EP" >/dev/null
cat <<EOF
Imported into $EP (manual recipe). Next, with this stock ave:
  confirm the camera mapping in episode.json with stills (skill ave-map-cameras)
  $AVE plan-cuts "$EP" --session s1     # the cameras start a few ms after take zero: plan the named session
  $AVE assemble "$EP" --session s1      # then --name s1 for cleanup, final, clips and shorts
EOF
