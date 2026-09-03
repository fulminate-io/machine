#!/bin/bash
# The step 4.4 recovery recorder: carry-in (e), a checkpointed flow
# surviving a REAL POD DELETION.
#
# WHY THIS IS A SCRIPT AND NOT CRITERION BYTES. The sequence has to release a
# parked node function, delete the pod that owns a datum, wait for a survivor to
# pick it up, and then kill THAT pod at the moment it is mid-recovery. A shell
# one-liner cannot express the ordering, and a criterion that tried would be
# unreadable and unprovable.
#
# IT CANNOT GREEN ITSELF, and that is the property the audit's quiet half taught
# on step 3.5. The completion token it looks for is printed by the FIXTURE'S OWN
# func body (testdata/ful1651/recovery/feed.go, func Done) and this script never
# prints it. The claimant it reads comes out of the ledger through
# Ledger.Claimant, served by the harness's /recovery endpoint, and never from
# anything this script wrote.
#
# IT EXITS NON-ZERO NAMING WHICH OBSERVATION WENT UNMET, so a partial recovery is
# distinguishable from a total failure and from a dead instrument.
#
# --skip-delete is the RED PROOF MODE. It runs the whole sequence except the pod
# deletion, so no datum is ever orphaned and observations 1, 2 and 3 must go
# unmet. A recorder that still reported PASS there would be reporting on nothing.
set -u

CTX=""
NS=""
SKIP_DELETE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --context)      CTX="$2"; shift 2 ;;
    --namespace)    NS="$2"; shift 2 ;;
    --skip-delete)  SKIP_DELETE=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done
[ -n "$CTX" ] || { echo "--context is required" >&2; exit 64; }
[ -n "$NS" ]  || { echo "--namespace is required" >&2; exit 64; }

K() { kubectl --context "$CTX" -n "$NS" "$@"; }

PORT=18080
cleanup() { pkill -f "port-forward pod/.* $PORT:8080" 2>/dev/null; return 0; }
trap cleanup EXIT

# pf opens a port-forward to one pod and waits for it to answer. Port-forward is
# used rather than exec because the image is distroless and has no shell.
#
# IT USES ONE FIXED PORT AND KILLS BY PATTERN, and that is a repair rather than a
# style choice. The first draft recorded the forward's pid in a variable and
# incremented the port per call. Both are SUBSHELL-LOCAL, so every pf reached
# from inside a command substitution — which is how the leader is found — left an
# ORPHANED forward the parent could neither see nor kill, on a port the parent
# then reused. The parent's later curls could reach a stale forward pointing at a
# different pod, and the run hung. Observed directly: the probe sat for six
# minutes having ingested nothing while /health answered correctly when queried
# by hand. One fixed port plus a pattern kill is subshell-proof because it keeps
# no state in a variable at all.
pf() {
  cleanup
  sleep 0.3
  K port-forward "pod/$1" "$PORT:8080" >/dev/null 2>&1 &
  for _ in $(seq 1 40); do
    if curl -sf -m 3 "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then return 0; fi
    sleep 0.25
  done
  echo "could not port-forward to $1" >&2
  return 1
}

pods()   { K get pods -l app=smokehost -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' | grep -v '^$'; }
ready()  { K get pods -l app=smokehost -o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready}{"\n"}{end}' | grep -c true; }

wait_for_three() {
  for _ in $(seq 1 60); do
    if [ "$(pods | wc -l | tr -d ' ')" = "3" ] && [ "$(ready)" = "3" ]; then return 0; fi
    sleep 5
  done
  return 1
}

# find_leader sets the global LEADER_FOUND and is NEVER called in a command
# substitution, because it opens port-forwards and a subshell cannot clean them
# up for the parent. That is the same defect the pf comment records.
LEADER_FOUND=""
find_leader() {
  LEADER_FOUND=""
  for p in $(pods); do
    pf "$p" || continue
    if curl -sf -m 5 "http://127.0.0.1:$PORT/health" 2>/dev/null | grep -q 'state=Leader'; then
      LEADER_FOUND="$p"
      return 0
    fi
  done
  return 1
}

# saw looks for a literal string in any CURRENTLY EXISTING pod's log and names
# the pod that printed it. It never searches a deleted pod, which is the point:
# a token found here was printed by a SURVIVOR.
saw() {
  for p in $(pods); do
    if K logs "$p" 2>/dev/null | grep -Fq "$1"; then echo "$p"; return 0; fi
  done
  return 1
}

wait_saw() {
  local needle="$1" limit="$2" p
  for _ in $(seq 1 "$limit"); do
    if p=$(saw "$needle"); then echo "$p"; return 0; fi
    sleep 2
  done
  return 1
}

UNMET=""
note() { echo "OBSERVATION UNMET: $1"; UNMET="$UNMET|$1"; }

echo "===== recovery recorder starting; skip_delete=$SKIP_DELETE ====="
date -u +%H:%M:%SZ
wait_for_three || { echo "the fleet never reached three ready pods; nothing to observe" >&2; exit 3; }

find_leader || { echo "no pod reported state=Leader; the group has no leader to detect orphans" >&2; exit 3; }
LEADER="$LEADER_FOUND"
echo "leader: $LEADER"

# The owner is a FOLLOWER, deliberately. The detector runs on the leader only, so
# deleting the leader would remove the very node that must notice the orphan and
# would confound a recovery observation with a leadership election.
OWNER=""
for p in $(pods); do [ "$p" != "$LEADER" ] && OWNER="$p" && break; done
[ -n "$OWNER" ] || { echo "no follower pod to own the datum" >&2; exit 3; }
echo "owner (a follower): $OWNER"

########## PHASE A — observations 1, 2 and 3 ##########
echo "===== phase A: park a datum on $OWNER, then delete that pod ====="
pf "$OWNER" || exit 3
curl -sf -X POST "http://127.0.0.1:$PORT/hold" || { echo "could not set the hold" >&2; exit 3; }
curl -sf -X POST "http://127.0.0.1:$PORT/ingest?id=d1" || { echo "could not ingest d1" >&2; exit 3; }

if ! K logs "$OWNER" 2>/dev/null | grep -Fq 'flow-recover: entered=d1'; then
  for _ in $(seq 1 30); do
    K logs "$OWNER" 2>/dev/null | grep -Fq 'flow-recover: entered=d1' && break
    sleep 2
  done
fi
K logs "$OWNER" 2>/dev/null | grep -F 'flow-recover: entered=d1' || {
  echo "d1 never reached the checkpointed node on $OWNER; there is no journaled record to orphan" >&2; exit 3; }

pf "$LEADER" || exit 3
echo "--- journal before the deletion (checkpoint datums and their claimants):"
BEFORE=$(curl -sf "http://127.0.0.1:$PORT/recovery" || echo '[]')
echo "$BEFORE"
if [ "$BEFORE" = "[]" ]; then
  echo "the journal holds no checkpoint at all, so nothing could be orphaned" >&2; exit 3
fi

if [ "$SKIP_DELETE" = "1" ]; then
  echo "--- RED PROOF MODE: the owning pod is NOT deleted, so nothing is orphaned"
else
  echo "--- deleting the owning pod $OWNER"
  K delete pod "$OWNER" --wait=true >/dev/null
fi

echo "--- waiting for a survivor to complete d1"
COMPLETER=$(wait_saw 'flow-recover: completed=d1' 60) || COMPLETER=""

pf "$LEADER" >/dev/null 2>&1 || true
AFTER=$(curl -sf "http://127.0.0.1:$PORT/recovery" 2>/dev/null || echo '[]')
echo "--- journal after: $AFTER"

# OBSERVATION 1: the leader detected the orphan, evidenced by a claim naming an
# owner. The detector emits no log line of its own, so the ledger IS the
# observable.
CLAIMANTS=$(printf '%s' "$BEFORE$AFTER" | tr ',' '\n' | grep -o '"claimant":"[^"]*"' | sed 's/.*:"//;s/"//' | grep -v '^$' | sort -u)
echo "--- claimants seen across the run: [$(printf '%s' "$CLAIMANTS" | tr '\n' ' ')]"
if [ -z "$CLAIMANTS" ] && [ -z "$COMPLETER" ]; then
  note "1: no claim ever named an owner and no survivor completed d1, so the orphan was never detected"
fi

# OBSERVATION 2: the claim was won by a SURVIVOR, never by the deleted pod.
if printf '%s' "$CLAIMANTS" | grep -Fq "$OWNER"; then
  note "2: the claim names the DELETED pod $OWNER rather than a survivor"
fi

# OBSERVATION 3: the datum RESUMED from its checkpointed bytes and completed —
# proven by the token the fixture's own sink body prints, on a pod that is not
# the deleted one.
if [ -z "$COMPLETER" ]; then
  note "3: no surviving pod printed the completion token for d1, so the datum did not resume"
elif [ "$COMPLETER" = "$OWNER" ]; then
  note "3: the completion token for d1 came from the deleted pod, not a survivor"
else
  echo "d1 completed on survivor $COMPLETER"
fi

########## PHASE B — observation 4, the retire-claim ##########
echo "===== phase B: kill the RECOVERING pod mid-recovery ====="
wait_for_three || { note "4: the fleet never returned to three pods, so the retire-claim leg could not run"; }

if [ "$SKIP_DELETE" = "1" ]; then
  note "4: red proof mode never deleted anything, so no claim was ever stranded"
else
  find_leader || LEADER_FOUND="$LEADER"
  LEADER2="$LEADER_FOUND"
  echo "leader: $LEADER2"
  # EVERY pod holds, so whichever one wins the recovery claim parks mid-recovery
  # and can be killed at that moment.
  for p in $(pods); do pf "$p" >/dev/null 2>&1 && curl -sf -X POST "http://127.0.0.1:$PORT/hold" >/dev/null; done

  OWNER2=""
  for p in $(pods); do [ "$p" != "$LEADER2" ] && OWNER2="$p" && break; done
  pf "$OWNER2" || exit 3
  curl -sf -X POST "http://127.0.0.1:$PORT/ingest?id=d2" >/dev/null
  for _ in $(seq 1 30); do K logs "$OWNER2" 2>/dev/null | grep -Fq 'flow-recover: entered=d2' && break; sleep 2; done
  echo "--- d2 parked on $OWNER2; deleting it"
  K delete pod "$OWNER2" --wait=true >/dev/null

  echo "--- waiting for a survivor to pick d2 up and park mid-recovery"
  RECOVERER=$(wait_saw 'flow-recover: entered=d2' 60) || RECOVERER=""
  if [ -z "$RECOVERER" ]; then
    note "4: no survivor ever picked d2 up, so there was no mid-recovery holder to kill"
  else
    echo "--- d2 is mid-recovery on $RECOVERER; killing it"
    K delete pod "$RECOVERER" --wait=true >/dev/null
    wait_for_three >/dev/null 2>&1
    # Release every pod so the re-offered datum can run to completion; a datum
    # that is re-offered but never released would read as stranded and is not.
    for p in $(pods); do pf "$p" >/dev/null 2>&1 && curl -sf -X POST "http://127.0.0.1:$PORT/release" >/dev/null; done
    FINISHER=$(wait_saw 'flow-recover: completed=d2' 90) || FINISHER=""
    if [ -z "$FINISHER" ]; then
      note "4: d2 was never re-offered after its recovering pod was killed; it is STRANDED"
    else
      echo "d2 was re-offered after the retire-claim and completed on $FINISHER"
    fi
  fi
fi

echo "===== per-pod recovery-relevant log lines (untruncated) ====="
for p in $(pods); do
  echo "--- $p"
  K logs "$p" 2>/dev/null | grep -E 'flow-recover:|SMOKEHOST-FLOW' || true
done

date -u +%H:%M:%SZ
if [ -n "$UNMET" ]; then
  echo "RECOVERY PROBE FAILED. Unmet observations:"
  printf '%s' "$UNMET" | tr '|' '\n' | grep -v '^$' | sed 's/^/  - /'
  exit 1
fi
echo "RECOVERY PROBE PASSED: all four observations met"
