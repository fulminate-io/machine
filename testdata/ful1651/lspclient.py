#!/usr/bin/env python3
"""FUL-1651 smoke recorder for flowlsp.

Opens one .flow document over the LSP base protocol on the server's standard
streams, collects the published diagnostics and one completion result, prints
both verbatim, and EXITS NON-ZERO when an expectation is unmet. The exit status
is the observation; the printed JSON is the reproduction record.

Usage:
  lspclient.py SERVER FILE LINE CHAR [--expect-diag SUBSTR]... [--expect-item LABEL]...
               [--expect-no-error] [--expect-no-items]
"""
import argparse, json, subprocess, sys, time

ap = argparse.ArgumentParser()
ap.add_argument("server"); ap.add_argument("file")
ap.add_argument("line", type=int); ap.add_argument("char", type=int)
ap.add_argument("--expect-diag", action="append", default=[])
ap.add_argument("--expect-item", action="append", default=[])
ap.add_argument("--expect-no-error", action="store_true")
ap.add_argument("--expect-no-items", action="store_true")
a = ap.parse_args()

src = open(a.file).read()
uri = "file://" + a.file
p = subprocess.Popen([a.server], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

def send(o):
    b = json.dumps(o).encode()
    p.stdin.write(b"Content-Length: %d\r\n\r\n" % len(b) + b); p.stdin.flush()

def read():
    h = b""
    while not h.endswith(b"\r\n\r\n"):
        c = p.stdout.read(1)
        if not c: return None
        h += c
    n = int([l for l in h.decode().split("\r\n") if l.lower().startswith("content-length")][0].split(":")[1])
    return json.loads(p.stdout.read(n))

send({"jsonrpc":"2.0","id":1,"method":"initialize","params":{"processId":None,"rootUri":None,"capabilities":{}}})
send({"jsonrpc":"2.0","method":"initialized","params":{}})
send({"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":uri,"languageId":"flow","version":1,"text":src}}})

# THE COMPLETION REQUEST WAITS FOR THE DIAGNOSTICS. The server builds its
# guidance table when a document changes and answers completion out of that
# table; asking before the publish arrives races the build and gets a truthful
# empty list, which would read as a completion defect and is not one.
diags = completion = None
deadline = time.time() + 20
while time.time() < deadline and diags is None:
    m = read()
    if m is None: break
    if m.get("method") == "textDocument/publishDiagnostics": diags = m["params"].get("diagnostics", [])

send({"jsonrpc":"2.0","id":2,"method":"textDocument/completion","params":{"textDocument":{"uri":uri},"position":{"line":a.line,"character":a.char}}})
while time.time() < deadline and completion is None:
    m = read()
    if m is None: break
    if m.get("id") == 2: completion = m.get("result")
p.kill()

print("DIAGNOSTICS", json.dumps(diags))
print("COMPLETION", json.dumps(completion))
sys.stdout.flush()

if diags is None or completion is None:
    print("RECORDER FAILED: the server did not answer within the deadline", file=sys.stderr); sys.exit(2)

blob = json.dumps(diags)
labels = [i.get("label") for i in (completion or [])]
bad = []
for s in a.expect_diag:
    if s not in blob: bad.append("no diagnostic containing %r" % s)
for s in a.expect_item:
    if s not in labels: bad.append("no completion item labelled %r" % s)
if a.expect_no_error and any(d.get("severity") == 1 for d in diags):
    bad.append("an error-severity diagnostic was published")
if a.expect_no_items and labels:
    bad.append("completion returned %d items" % len(labels))
if bad:
    for b in bad: print("UNMET:", b, file=sys.stderr)
    sys.exit(1)
