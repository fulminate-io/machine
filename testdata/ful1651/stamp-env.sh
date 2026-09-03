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
# THREE CHECKS, AND THE HEADER NAMES WHAT THE CODE DOES. An earlier version of
# this comment claimed a per-name check for surviving values that was never
# implemented, while the only second side present was the placeholder guard.
# The checks are:
#
#   PRE, PER NAME: the name appears EXACTLY ONCE as a container env name. Zero
#   is an operator error rather than a no-op, because silently stamping nothing
#   is how a gate varies an input it never set; more than one is ambiguous about
#   which entry was meant.
#
#   POST, PER NAME AND LITERAL: the line immediately following that name is a
#   value line carrying EXACTLY the requested value, compared as a string. This
#   is scoped to the subject rather than to the document, and the difference is
#   not academic: a whole-render search for the requested value passes when that
#   value happens to appear under some OTHER name, so stamping a valueFrom-backed
#   entry — which this tool must never rewrite — exited 0 on a byte-identical
#   file whenever the value collided with a neighbour's. Literal comparison also
#   removes the mirror-image defect, a value containing regex metacharacters
#   being interpolated as a pattern and reported absent when it was written
#   correctly.
#
#   POST, WHOLE RENDER: no placeholder token survives. The token is gone from
#   the manifest today, so this guards against its return rather than clearing
#   one now.
#
# THE NAME AND THE VALUE REACH awk THROUGH THE ENVIRONMENT, never through -v.
# awk processes escape sequences in a -v assignment, so a value containing a
# backslash arrived at the rewrite already mangled and the post-check then
# reported it absent — a refusal whose message blamed the wrong thing.
#
# Usage: stamp-env.sh <manifest> NAME=VALUE [NAME=VALUE ...]
set -euo pipefail

manifest=${1:?usage: stamp-env.sh <manifest> NAME=VALUE [NAME=VALUE ...]}
shift
test -f "$manifest" || { echo "stamp-env: no such manifest: $manifest" >&2; exit 1; }
test "$#" -gt 0 || { echo "stamp-env: no NAME=VALUE pairs given, so this would emit the manifest unchanged" >&2; exit 1; }

# awk_common normalizes a line's whitespace so every comparison below is a
# LITERAL string equality rather than a pattern match. Nothing the caller
# supplies is ever interpolated into a regular expression.
awk_common='
	function norm(s) {
		gsub(/[[:space:]]+/, " ", s)
		sub(/^ /, "", s)
		sub(/ $/, "", s)
		return s
	}
	function isname(s) { return norm(s) == "- name: " ENVIRON["STAMP_NAME"] }
'

rendered=$(cat "$manifest")

# A CRLF MANIFEST IS REFUSED EXPLICITLY, and the guard is here rather than
# emergent. The earlier version refused one only as a side effect of matching
# names with a regex that a trailing carriage return defeated, which read as
# "the name is absent". Normalizing whitespace to make the comparisons literal
# removed that accident and would have let a CRLF manifest through — writing LF
# lines into a CRLF file and emitting a mixed-ending manifest, which is worse
# than either refusing or handling it. It is refused by its own name instead.
if printf '%s\n' "$rendered" | grep -q $'\r'; then
	echo "stamp-env: $manifest has CRLF line endings; this tool emits LF and would produce a mixed-ending manifest" >&2
	exit 1
fi

for pair in "$@"; do
	name=${pair%%=*}
	value=${pair#*=}
	test -n "$name" || { echo "stamp-env: empty env name in $pair" >&2; exit 1; }
	test "$name" != "$pair" || { echo "stamp-env: $pair is not NAME=VALUE" >&2; exit 1; }

	# A VALUE CONTAINING A DOUBLE QUOTE IS REFUSED BY NAME, BEFORE ANY WRITE.
	# This tool emits the value as a double-quoted YAML scalar, so an embedded
	# quote closes that scalar early: the value is written faithfully and the
	# manifest stops parsing, which is the worst of the two failures. Refusing
	# beats escaping — no epoch or nonce needs a quote, and a tool that quietly
	# rewrote the value would be varying an input its caller did not choose.
	case $value in
	*'"'*)
		echo "stamp-env: the value for $name contains a double quote, which cannot be written as a" \
			"double-quoted YAML scalar unchanged; refusing rather than emitting a manifest that does not parse" >&2
		exit 1
		;;
	esac

	before=$(printf '%s\n' "$rendered" | STAMP_NAME="$name" awk "$awk_common"'
		isname($0) { c++ }
		END { print c + 0 }
	')
	test "$before" -eq 1 || {
		echo "stamp-env: $name appears $before times as a container env name in $manifest, want exactly 1" >&2
		exit 1
	}

	rendered=$(printf '%s\n' "$rendered" | STAMP_NAME="$name" STAMP_VALUE="$value" awk "$awk_common"'
		isname($0) { hit = 1; print; next }
		hit && norm($0) ~ /^value:/ {
			match($0, /^[[:space:]]*/)
			printf "%svalue: \"%s\"\n", substr($0, 1, RLENGTH), ENVIRON["STAMP_VALUE"]
			hit = 0
			next
		}
		# A name whose next line is not a value: is a valueFrom entry or a
		# malformed one; either way this tool must not rewrite it silently. The
		# post-check below is what turns that into a refusal.
		hit { hit = 0 }
		{ print }
	')

	# THE POST-CHECK READS THE LINE UNDER THIS NAME AND NOTHING ELSE.
	got=$(printf '%s\n' "$rendered" | STAMP_NAME="$name" awk "$awk_common"'
		isname($0) { hit = 1; next }
		hit { print norm($0); exit }
	')
	if test "$got" != "value: \"$value\""; then
		echo "stamp-env: $name was not stamped: the line under it reads [$got], want [value: \"$value\"]." \
			"An entry backed by valueFrom has no value line to rewrite and is refused rather than altered." >&2
		exit 1
	fi
done

if printf '%s\n' "$rendered" | grep -q 'REPLACED_AT_APPLY'; then
	echo "stamp-env: a REPLACED_AT_APPLY placeholder survived the render" >&2
	exit 1
fi

printf '%s\n' "$rendered"
