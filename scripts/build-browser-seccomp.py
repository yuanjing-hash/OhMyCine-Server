"""Generate the complete pinned Moby policy with Chromium namespace additions."""
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
BASE = ROOT / "deploy/seccomp/moby-default.json"
OUTPUT = ROOT / "deploy/seccomp/browser.json"
# Chromium NamespaceSandbox creates USER/PID/NET namespaces. Keep all other
# namespace flags forbidden; ordinary clone remains covered by the baseline.
NAMESPACE_MASK = 0x7E020000
CHROMIUM_NAMESPACES = 0x70000000
ADDITIONS = [
    {
        "names": ["clone"], "action": "SCMP_ACT_ALLOW",
        "args": [{"index": 0, "value": NAMESPACE_MASK & ~CHROMIUM_NAMESPACES,
                  "valueTwo": 0, "op": "SCMP_CMP_MASKED_EQ"}],
        "includes": {"arches": ["amd64", "arm64"]},
        "comment": "Chromium USER/PID/NET namespace clone; kernel ownership checks remain authoritative",
    },
    {
        "names": ["unshare"], "action": "SCMP_ACT_ALLOW",
        "args": [{"index": 0, "value": 0x10000000, "op": "SCMP_CMP_EQ"}],
        "includes": {"arches": ["amd64", "arm64"]},
        "comment": "Chromium Credentials user-namespace probe and entry only",
    },
]

def profile():
    result = json.loads(BASE.read_bytes())
    result["syscalls"].extend(ADDITIONS)
    return result

if __name__ == "__main__":
    OUTPUT.write_bytes((json.dumps(profile(), indent=2) + "\n").encode("utf-8"))
