# Scores pictures for nudity with NudeNet, for scripts/scan-nudity. Reads one picture path per line on stdin and
# answers each with one JSON line on stdout: {"score": 0.93, "found": ["FEMALE_BREAST_EXPOSED"]}, or {"error": "..."}.
# The score is the detector's highest confidence in any exposed part (0 when none is found).
import json
import sys

from nudenet import NudeDetector

EXPOSED = {
    "FEMALE_BREAST_EXPOSED",
    "FEMALE_GENITALIA_EXPOSED",
    "MALE_GENITALIA_EXPOSED",
    "BUTTOCKS_EXPOSED",
    "ANUS_EXPOSED",
}

detector = NudeDetector()
print(json.dumps({"ready": True}), flush=True)
for line in sys.stdin:
    path = line.rstrip("\r\n")
    if not path:
        continue
    try:
        found = [d for d in detector.detect(path) if d["class"] in EXPOSED]
        answer = {
            "score": max((float(d["score"]) for d in found), default=0.0),
            "found": sorted({d["class"] for d in found}),
        }
    except Exception as e:  # one bad picture must not stop the scan
        answer = {"error": str(e)}
    print(json.dumps(answer), flush=True)
