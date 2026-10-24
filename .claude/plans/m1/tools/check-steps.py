#!/usr/bin/env python3
# Plan checks for steps.md, run from the repository root by every step:
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
rows = [l for l in md.splitlines() if re.match(r"^\| (\d+\.\d+[a-z]?|[RXC]\.\d+[a-z]?) \|", l)]
ids = [l.split("|")[1].strip() for l in rows]
dups = sorted(i for i, c in collections.Counter(ids).items() if c > 1)
check(not dups, "step IDs are unique %s" % (dups or ""))

per_stage = collections.Counter(re.sub(r"\..*", "", i) for i in ids)
table = {}
for l in md.splitlines():
    m = re.match(r"^\| (\d+|R|X|C) [^|]*\| (\d+) \| (\d+) \|", l)
    if m:
        table[m.group(1)] = int(m.group(2))
diff = {k: (table.get(k), per_stage.get(k)) for k in set(table) | set(per_stage) if table.get(k) != per_stage.get(k)}
check(not diff, "status table counts match the step rows %s" % (diff or ""))

status = {l.split("|")[1].strip(): l.rstrip().rstrip("|").split("|")[-1].strip() for l in rows}
done_rows = collections.Counter(re.sub(r"\..*", "", i) for i in ids if status[i].startswith("Done"))
done_table = {}
for l in md.splitlines():
    m = re.match(r"^\| (\d+|R|X|C) [^|]*\| (\d+) \| (\d+) \|", l)
    if m:
        done_table[m.group(1)] = int(m.group(3))
ddiff = {k: (done_table.get(k), done_rows.get(k, 0)) for k in done_table if done_table.get(k) != done_rows.get(k, 0)}
check(not ddiff, "status table Done counts match the rows marked Done %s" % (ddiff or ""))
done_total = re.search(r"\*\*All\*\* \| \*\*\d+\*\* \| \*\*(\d+)\*\*", md)
check(done_total is not None and int(done_total.group(1)) == sum(done_rows.values()), "status Done total equals %d rows marked Done" % sum(done_rows.values()))
first_open = next((i for i in ids if not status[i].startswith(("Done", "Blocked"))), None)
next_step = re.search(r"^Next step: (\S+?)\.?$", md, re.M)
check(next_step is not None and next_step.group(1) == (first_open or "none"), "\"Next step\" names the first step neither done nor blocked (%s)" % (first_open or "none"))
total = re.search(r"\*\*All\*\* \| \*\*(\d+)\*\*", md)
check(total is not None and int(total.group(1)) == len(ids), "status total equals %d step rows" % len(ids))

bad = [i for i in ids if subprocess.run(["git", "check-ref-format", "--branch", "step/" + i], capture_output=True).returncode]
check(not bad, "every step/<id> is a valid branch name %s" % (bad or ""))

w = json.load(open(M1 + "arch/wps.json", encoding="utf-8"))
w = w if isinstance(w, list) else next(v for v in w.values() if isinstance(v, list))
body = "\n".join(rows)
missing = [p["id"] for p in w if not re.search(r"\| %s[ |]" % re.escape(p["id"]), body)]
check(not missing, "every work package in arch/wps.json has a step %s" % (missing or ""))
r_body = "\n".join(r for r in rows if r.startswith("| R."))
no_review = [p["id"] for p in w if p["wave"] <= 9 and p["id"] != "WP-31" and not re.search(r"\| %s[ |]" % re.escape(p["id"]), r_body)]
check(not no_review, "every work package built by wave 9 has a final-review step in stage R, WP-31's documents excepted %s" % (no_review or ""))
last_step = {}
for r in rows:
    pkg = re.match(r"WP-\d+\b", r.split("|")[2].strip())
    if pkg and re.match(r"\d", r.split("|")[1].strip()):
        last_step[pkg.group(0)] = r
unchecked = sorted(k for k, r in last_step.items() if "package check" not in r.split("|")[3])
check(not unchecked, "the last step of every package ends with its package check %s" % (unchecked or ""))
scripted = [r.split("|")[1].strip() for r in rows if re.search(r"\bm1-[a-z0-9-]+\.js\b", r)]
check(not scripted, "no step names a workflow run script (decision 40) %s" % (scripted or ""))


def finding_numbers(step):
    cell = next(r for r in rows if r.startswith("| %s |" % step)).split("|")[3]
    m = re.search(r"Findings ((?:\d+(?: to \d+)?(?:,? and |, )?)+)", cell)
    seg = m.group(1) if m else ""
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
multi = [r.split("|")[1].strip() for r in rows if len(re.findall(r"Applies `wip/", r)) > 1]
check(not multi, "no step applies more than one patch %s" % (multi or ""))
ref = os.path.exists(M1 + "wip/WP-51-attempt3-reference.patch")
check(ref == (not status.get("3.01", "").startswith("Done")), "the attempt-3 reference patch exists exactly while step 3.01 is not done")

backlog = json.load(open(M1 + "runs/s8-backlog.json", encoding="utf-8"))["entries"]
pending = {e["id"] for e in backlog if e["class"] == "C"}
asked = {e["id"] for e in backlog if e["class"] == "C" and e.get("needsDecision")}
apply_rows = [r for r in rows if re.match(r"^\| 3\.69[a-z]? \|", r)]
applied_ids = collections.Counter(x for r in apply_rows for x in set(re.findall(r"\bE\d\d\b", r)))
check(set(applied_ids) == pending and all(n == 1 for n in applied_ids.values()), "steps 3.69 to 3.69k apply each of the %d pending backlog entries exactly once" % len(pending))
row300 = next((r for r in rows if r.startswith("| 3.00 |")), "")
row368 = next((r for r in rows if r.startswith("| 3.68 |")), "")
ask300, ask368 = set(re.findall(r"\bE\d\d\b", row300)), set(re.findall(r"\bE\d\d\b", row368))
check(ask300 | ask368 == asked and not ask300 & ask368, "steps 3.00 and 3.68 together ask each of the %d backlog entries that need a decision exactly once" % len(asked))

meta_counts = json.load(open(M1 + "runs/s8-backlog.json", encoding="utf-8"))["meta"].get("classCounts", {})
real_counts = {c: sum(1 for e in backlog if e["class"] == c) for c in "ABC"}
check(meta_counts == real_counts, "s8-backlog.json meta.classCounts matches its entries %s" % real_counts)
prose = re.search(r"(\d+) were already applied and (\d+) are applied by the package steps .*?leaves the (\d+) below", md)
check(prose is not None and tuple(int(x) for x in prose.groups()) == (real_counts["A"], real_counts["B"], real_counts["C"]), "the S8 section's counts match the backlog file")
bmeta = json.load(open(M1 + "runs/s8-backlog.json", encoding="utf-8"))["meta"]
nsrc = sum(len(e.get("sources", [])) for e in backlog)
check(bmeta.get("mergedEntries") == len(backlog) and bmeta.get("classCNeedsDecision") == len(asked) and bmeta.get("requests") == nsrc, "s8-backlog.json meta counts requests, entries and decisions correctly")
merge = re.search(r"the S1 and S2 runs' (\d+) requests merge into (\d+) entries", md)
check(merge is not None and int(merge.group(1)) == bmeta.get("requests") and int(merge.group(2)) == len(backlog), "the S8 section's request and entry counts match the backlog file")
n300 = re.search(r"\| 3\.00 \| [^|]*\| The (\d+) backlog entries", md)
n368 = re.search(r"\| 3\.68 \| [^|]*\| The (\d+) remaining backlog entr(?:y|ies)", md)
check(n300 is not None and n368 is not None and int(n300.group(1)) == len(ask300) and int(n368.group(1)) == len(ask368), "the counts in steps 3.00 and 3.68 match the entries they ask")
undecided = [r.split("|")[1].strip() for r in apply_rows if (set(re.findall(r"\bE\d\d\b", r)) & ask300 and "3.00" not in r) or (set(re.findall(r"\bE\d\d\b", r)) & ask368 and "3.68" not in r)]
check(not undecided, "every step applying a decided backlog entry names the step that decides it %s" % (undecided or ""))

missing_settle = []
for e in backlog:
    for sid in e.get("settledBy", []):
        row = next((r for r in rows if r.startswith("| %s |" % sid)), "")
        if not re.search(r"\b%s\b" % e["id"], row):
            missing_settle.append("%s@%s" % (e["id"], sid))
check(not missing_settle, "every step a backlog entry's settledBy names cites that entry %s" % (missing_settle or ""))
need_settle = sorted(i for i in ask300 if not next((e for e in backlog if e["id"] == i), {}).get("settledBy"))
check(not need_settle, "every entry decided at 3.00 names the steps that settle it %s" % (need_settle or ""))
intro = re.search(r"the steps that own that code settle it \(([^)]*)\)", md)
union300 = {s for e in backlog if e["id"] in ask300 for s in e.get("settledBy", [])}
check(intro is not None and set(intro.group(1).split(", ")) == union300, "the S1 and S2 backlog section lists exactly the steps that settle the entries decided at 3.00")

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
