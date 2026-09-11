#!/usr/bin/env bash

# Enforce a per-package line-coverage floor from a Go coverage profile.
#
#     ./scripts/check-coverage.sh <profile> <floor-percent> [module-label]
#
# Why per package rather than one repository total: a total hides a package. A module
# can read 88% overall while its guardrail package sits at 40%, because a large,
# well-tested package pays for a small, untested one. The floor is a promise about
# every package, so it is checked against every package.
#
# `cmd/` packages are excluded by kind, not by name. They are `package main` composition
# wiring — flag parsing, dependency construction, process lifecycle — that this project
# has chosen not to hold to the floor. Their coverage is measured like every other
# package's and simply sits below it. Every other package is in.
#
# A package with no test file is *not* a blind spot here, which is worth writing down
# because it looks like one. `go test -coverprofile ./...` compiles every package for
# coverage and emits its blocks at count 0 whether or not a test file exists, so such a
# package arrives in the profile reading 0.0% and fails the floor by the ordinary path.
# Verified against Go 1.26.5 on 13 August 2026 by adding a test-free package and running
# the module's own test task; it was named and it failed. The only package this profile
# cannot see is one with no executable statement at all — a file of types and constants —
# and there is nothing to cover there, so its absence is correct rather than missing.
# Cross-checking against `go list ./...` was considered and rejected for that reason: it
# would fail exactly the packages that are right to be silent.

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

profile="${1:-}"
floor="${2:-}"
label="${3:-module}"

[[ -n ${profile} && -n ${floor} ]] || fail "usage: check-coverage.sh <profile> <floor-percent> [module-label]"
[[ -r ${profile} ]] || fail "coverage profile ${profile} is unreadable; run the test task first"

# The profile's body is `<file>:<start>,<end> <statements> <count>`, one block per
# basic block. Coverage is covered statements over total statements, summed per
# package, which is exactly how `go test -cover` computes the number it prints.
report="$(awk -v floor="${floor}" '
	NR == 1 { next }                       # the "mode:" header
	NF != 3 { next }
	{
		file = $1
		sub(/:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/, "", file)
		package_path = file
		sub(/\/[^\/]*$/, "", package_path)
		total[package_path] += $2
		if ($3 > 0) { covered[package_path] += $2 }
	}
	END {
		for (package_path in total) {
			short_name = package_path
			sub(/^[^\/]*\/[^\/]*\/[^\/]*\//, "", short_name)   # drop the module path
			if (short_name ~ /(^|\/)cmd\//) { continue }
			percent = total[package_path] ? covered[package_path] * 100 / total[package_path] : 0
			printf "%s\t%.1f\t%s\n", (percent + 0.05 < floor ? "BELOW" : "ok"), percent, short_name
		}
	}
' "${profile}" | sort -k3)"

printf '%s\n' "${report}" | awk -F'\t' '{ printf "  %-5s %6.1f%%  %s\n", $1, $2, $3 }'

below="$(printf '%s\n' "${report}" | awk -F'\t' '$1 == "BELOW" { print $3 }')"
if [[ -n ${below} ]]; then
	printf '\n%s coverage floor is %s%% per package; below it:\n' "${label}" "${floor}" >&2
	printf '%s\n' "${below}" | sed 's/^/  /' >&2
	fail "add tests for those packages, or make the case for moving the floor — do not exclude a path"
fi

log "${label} meets the ${floor}% per-package coverage floor"
