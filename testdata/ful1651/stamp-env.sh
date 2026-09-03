#!/usr/bin/env bash
# stamp-env.sh rewrites the VALUE of named container env entries in a manifest,
# on stdout, and refuses rather than emitting a manifest it did not fully stamp.
#
# IT KEYS ON THE ENV NAME AND NEVER ON THE VALUE TEXT, and that is the whole
# point rather than a style choice. The obvious implementation is a substitution
# on a placeholder token, and it works exactly until the manifest stops carrying
# the token — which it now must, because a token is not a parseable epoch and
# every gate that applies this manifest WITHOUT substituting has to get a
# runnable pod. A value-keyed substituter would then match nothing, emit the
# manifest unchanged, and the deployment would silently run at the baseline
# epoch while the gate above it believed it had stamped a fresh one. That is a
# false green on the one input the gate exists to vary.
#
# IT IS TWO-SIDED. Stamping is not enough on its own: the check that the target
# value is PRESENT afterwards and the check that NO OTHER value survives for
# that name are different failures, and only both together distinguish "stamped"
# from "stamped one of two occurrences" or from "matched a name that is not
# there at all".
#
# Usage: stamp-env.sh <manifest> NAME=VALUE [NAME=VALUE ...]
set -euo pipefail

manifest=${1:?usage: stamp-env.sh <manifest> NAME=VALUE [NAME=VALUE ...]}
shift
test -f "$manifest" || { echo "stamp-env: no such manifest: $manifest" >&2; exit 1; }
test "$#" -gt 0 || { echo "stamp-env: no NAME=VALUE pairs given, so this would emit the manifest unchanged" >&2; exit 1; }

rendered=$(cat "$manifest")
for pair in "$@"; do
	name=${pair%%=*}
	value=${pair#*=}
	test -n "$name" || { echo "stamp-env: empty env name in $pair" >&2; exit 1; }
	test "$name" != "$pair" || { echo "stamp-env: $pair is not NAME=VALUE" >&2; exit 1; }

	# THE NAME MUST BE THERE BEFORE ANYTHING IS REWRITTEN. A name absent from the
	# manifest is an operator error, not a no-op: silently stamping nothing is
	# how a gate ends up varying an input it never set.
	before=$(printf '%s\n' "$rendered" | grep -c "^[[:space:]]*-[[:space:]]*name:[[:space:]]*$name\$" || true)
	test "$before" -eq 1 || {
		echo "stamp-env: $name appears $before times as a container env name in $manifest, want exactly 1" >&2
		exit 1
	}

	rendered=$(printf '%s\n' "$rendered" | awk -v n="$name" -v v="$value" '
		$0 ~ "^[[:space:]]*-[[:space:]]*name:[[:space:]]*" n "$" { hit = 1; print; next }
		hit && /^[[:space:]]*value:/ {
			match($0, /^[[:space:]]*/)
			printf "%svalue: \"%s\"\n", substr($0, 1, RLENGTH), v
			hit = 0
			next
		}
		# A name whose next line is not a value: is a valueFrom entry or a
		# malformed one; either way this tool must not rewrite it silently.
		hit && /^[[:space:]]*-[[:space:]]*name:/ { hit = 0 }
		{ print }
	')

	# SIDE ONE: the value this run was asked to write is present.
	printf '%s\n' "$rendered" | grep -q "^[[:space:]]*value:[[:space:]]*\"$value\"\$" || {
		echo "stamp-env: after stamping $name the value \"$value\" is absent, so nothing was written" >&2
		exit 1
	}
done

# SIDE TWO, ACROSS THE WHOLE RENDER: no placeholder token survives anywhere. The
# token is gone from the manifest today, so this is a guard against it coming
# back rather than a check that today's render cleared one.
if printf '%s\n' "$rendered" | grep -q 'REPLACED_AT_APPLY'; then
	echo "stamp-env: a REPLACED_AT_APPLY placeholder survived the render" >&2
	exit 1
fi

printf '%s\n' "$rendered"
