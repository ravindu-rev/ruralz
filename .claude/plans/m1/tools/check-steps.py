#!/usr/bin/env python3
# Plan checks for steps.md, run from the repository root by every plan-only step:
#   python3 -I .claude/plans/m1/tools/check-steps.py
# Exits 1 and prints FAIL lines when a check fails.
import collections
import glob
import json
import os
import re
import subprocess
import sys

M1 = ".claude/plans/m1/"
failures = []


def check(ok, what):
    print(("ok    " if ok else "FAIL  ") + what)
    if not ok:
        failures.append(what)


md = open(M1 + "steps.md", encoding="utf-8").read()
rows = [l for l in md.splitlines() if re.match(r"^\| (\d+\.\d+[a-z]?|[RX]\.\d+[a-z]?) \|", l)]
ids = [l.split("|")[1].strip() for l in rows]
dups = sorted(i for i, c in collections.Counter(ids).items() if c > 1)
check(not dups, "step IDs are unique %s" % (dups or ""))

per_stage = collections.Counter(re.sub(r"\..*", "", i) for i in ids)
table = {}
for l in md.splitlines():
    m = re.match(r"^\| (\d+|R|X) [^|]*\| (\d+) \| (\d+) \|", l)
    if m:
        table[m.group(1)] = int(m.group(2))
diff = {k: (table.get(k), per_stage.get(k)) for k in set(table) | set(per_stage) if table.get(k) != per_stage.get(k)}
check(not diff, "status table counts match the step rows %s" % (diff or ""))

status = {l.split("|")[1].strip(): l.rstrip().rstrip("|").split("|")[-1].strip() for l in rows}
done_rows = collections.Counter(re.sub(r"\..*", "", i) for i in ids if status[i].startswith("Done"))
done_table = {}
for l in md.splitlines():
    m = re.match(r"^\| (\d+|R|X) [^|]*\| (\d+) \| (\d+) \|", l)
    if m:
        done_table[m.group(1)] = int(m.group(3))
ddiff = {k: (done_table.get(k), done_rows.get(k, 0)) for k in done_table if done_table.get(k) != done_rows.get(k, 0)}
check(not ddiff, "status table Done counts match the rows marked Done %s" % (ddiff or ""))
done_total = re.search(r"\*\*All\*\* \| \*\*\d+\*\* \| \*\*(\d+)\*\*", md)
check(done_total is not None and int(done_total.group(1)) == sum(done_rows.values()), "status Done total equals %d rows marked Done" % sum(done_rows.values()))
first_open = next((i for i in ids if not status[i].startswith("Done")), None)
next_step = re.search(r"^Next step: (\S+?)\.?$", md, re.M)
check(next_step is not None and next_step.group(1) == first_open, "\"Next step\" names the first step not done (%s)" % first_open)
total = re.search(r"\*\*All\*\* \| \*\*(\d+)\*\*", md)
check(total is not None and int(total.group(1)) == len(ids), "status total equals %d step rows" % len(ids))

bad = [i for i in ids if subprocess.run(["git", "check-ref-format", "--branch", "step/" + i], capture_output=True).returncode]
check(not bad, "every step/<id> is a valid branch name %s" % (bad or ""))

w = json.load(open(M1 + "arch/wps.json", encoding="utf-8"))
w = w if isinstance(w, list) else next(v for v in w.values() if isinstance(v, list))
body = "\n".join(rows)
missing = [p["id"] for p in w if not re.search(r"\| %s[ |]" % re.escape(p["id"]), body)]
check(not missing, "every work package in arch/wps.json has a step %s" % (missing or ""))


def finding_numbers(step):
    cell = next(r for r in rows if r.startswith("| %s |" % step)).split("|")[3]
    seg = cell[cell.index("Findings"):].split(":")[0].split(";")[0]
    out = set()
    for a, b in re.findall(r"(\d+) to (\d+)", seg):
        out |= set(range(int(a), int(b) + 1))
    out |= {int(x) for x in re.findall(r"\d+", re.sub(r"\d+ to \d+", "", seg))}
    return out


minors = json.load(open(M1 + "runs/w3-s2-minors-continue3.json", encoding="utf-8"))
for wp, steps in (("WP-36", ("3.14", "3.15")), ("WP-37", ("3.16", "3.17")), ("WP-39", ("3.19", "3.20"))):
    a, b = (finding_numbers(s) for s in steps)
    n = len(minors[wp]["findings"])
    check(a | b == set(range(1, n + 1)) and not a & b, "%s's %d findings split exactly once over steps %s and %s" % (wp, n, steps[0], steps[1]))

open_body = "\n".join(r for r in rows if not status[r.split("|")[1].strip()].startswith("Done"))
applied = collections.Counter(re.findall(r"Applies `wip/([^`]+\.patch)`", open_body))
parked = {os.path.basename(f) for f in glob.glob(M1 + "wip/*.patch")} - {"WP-51-attempt3-reference.patch"}
check(set(applied) <= parked, "every patch a step not yet done applies exists in wip/ %s" % (sorted(set(applied) - parked) or ""))
check(all(applied[f] == 1 for f in parked), "every parked patch is applied by exactly one step not yet done %s" % (sorted(f for f in parked if applied[f] != 1) or ""))
ref = os.path.exists(M1 + "wip/WP-51-attempt3-reference.patch")
check(ref == (not status.get("3.01", "").startswith("Done")), "the attempt-3 reference patch exists exactly while step 3.01 is not done")

wp51 = "\n".join(r for r in rows if re.match(r"^\| 3\.(0\d[a-z]?|1[0-2]) \|", r))
named = set(re.findall(r"GC-(\d\d)", wp51))
landed = {"21", "01", "02"}  # G01 to G03, finished before the step plan; step 3.01b checks them again
left = sorted({"%02d" % i for i in range(1, 22)} - named - landed)
check(not left, "steps 3.01 to 3.12 name every open WP-51 gap class %s" % (left or ""))

for name in ("steps.md", "README.md", "user-decisions.md", "wave3-plan.md"):
    text = open(M1 + name, encoding="utf-8").read()
    check("—" not in text, name + " has no em dash")
    check(all(x == x.rstrip() for x in text.splitlines()), name + " has no trailing spaces")
    check(text.endswith("\n") and not text.endswith("\n\n"), name + " ends with one newline")

sys.exit(1 if failures else 0)
