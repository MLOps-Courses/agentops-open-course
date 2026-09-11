#!/usr/bin/env bash

# Git exports GIT_DIR, GIT_INDEX_FILE, and friends to every hook it runs, and those
# variables outrank `git -C`: with GIT_DIR set, `git -C "${tmp_dir}/plugin" commit`
# creates the throwaway repository's commit in the *contributor's* checkout, from the
# contributor's staged index. `check:shell` runs this file from lefthook's pre-commit,
# so that is not a hypothetical — it hijacks the commit being made. Clearing the
# inherited repository is the whole guard; the temp repositories below then discover
# themselves from their own directories, exactly as they do outside a hook.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
	GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR GIT_PREFIX GIT_CONFIG
# Configuration is a separate inheritance, and it needs the opposite treatment:
# *unsetting* GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM hands the throwaway repositories
# the contributor's real ~/.gitconfig, which is precisely what a harness that sets
# them to /dev/null was keeping out. Point them at an empty file instead. Without
# this, a contributor with `commit.gpgsign = true` cannot commit at all: the commit
# below fails to sign, `check:shell` fails, and lefthook's pre-commit refuses the
# commit that started it.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

tmp_dir=$(mktemp -d)
trap 'rm -r -- "${tmp_dir}"' EXIT

mkdir "${tmp_dir}/v1" "${tmp_dir}/v2"
touch "${tmp_dir}/v2/cgroup.controllers"

require_cgroup_v2 "${tmp_dir}/v2"
if (require_cgroup_v2 "${tmp_dir}/v1") 2>"${tmp_dir}/error"; then
	fail "require_cgroup_v2 accepted a cgroup v1 hierarchy"
fi
grep -Fqx \
	"cgroup v2 required for pinned Kubernetes; enable the unified cgroup hierarchy before running local k3d" \
	"${tmp_dir}/error"

log "test progress" 2>"${tmp_dir}/log"
grep -Fqx "test progress" "${tmp_dir}/log"

if (fail "expected failure") 2>"${tmp_dir}/fail"; then
	fail "fail returned successfully"
fi
grep -Fqx "expected failure" "${tmp_dir}/fail"

require_cmd sh
if (PATH="${tmp_dir}" require_cmd definitely-not-a-command validation) 2>"${tmp_dir}/require"; then
	fail "require_cmd accepted a missing command"
fi
grep -Fqx \
	"missing definitely-not-a-command: run 'mise run install:validation', then 'mise run doctor:validation' to check the whole tier" \
	"${tmp_dir}/require"

# Every profile has to name the tier that actually ships its tools, not a generic one.
for profile_case in "gateway:mise run install:platform" "platform:mise run install:platform" \
	"gcp:mise run install:platform" "base:mise run install"; do
	profile="${profile_case%%:*}"
	expected_tier="${profile_case#*:}"
	if (PATH="${tmp_dir}" require_cmd definitely-not-a-command "${profile}") 2>"${tmp_dir}/require-${profile}"; then
		fail "require_cmd accepted a missing command for the ${profile} profile"
	fi
	grep -Fqx \
		"missing definitely-not-a-command: run '${expected_tier}', then 'mise run doctor:${profile}' to check the whole tier" \
		"${tmp_dir}/require-${profile}"
done

if (PATH="${tmp_dir}" require_host_cmd definitely-not-a-command "install it from a reviewed host package source") \
	2>"${tmp_dir}/require-host"; then
	fail "require_host_cmd accepted a missing command"
fi
grep -Fqx \
	"missing definitely-not-a-command: install it from a reviewed host package source" \
	"${tmp_dir}/require-host"

printf 'verified bytes' >"${tmp_dir}/artifact"
artifact_sha256="$(sha256sum "${tmp_dir}/artifact" | awk '{ print $1 }')"
verify_sha256 "${tmp_dir}/artifact" "${artifact_sha256}" "test artifact"
if (verify_sha256 "${tmp_dir}/artifact" "$(printf '0%.0s' {1..64})" "test artifact") \
	2>"${tmp_dir}/checksum"; then
	fail "verify_sha256 accepted a mismatched digest"
fi
grep -Fq "test artifact checksum mismatch" "${tmp_dir}/checksum"

mkdir "${tmp_dir}/plugin"
git -C "${tmp_dir}/plugin" init -q
# The regression guard for the unset above, and for any GIT_* variable a future git
# adds to it: `git init` succeeds under an inherited GIT_DIR without creating
# anything here, and every command after it would then address the caller's
# repository instead. Assert the throwaway repository is the one in play.
plugin_git_dir="$(git -C "${tmp_dir}/plugin" rev-parse --absolute-git-dir)"
[[ ${plugin_git_dir} == "${tmp_dir}/plugin/.git" ]] ||
	fail "test plugin git dir is ${plugin_git_dir}, want ${tmp_dir}/plugin/.git; a git environment variable leaked in"
printf 'reviewed executable' >"${tmp_dir}/plugin/tool"
printf 'command: tool\n' >"${tmp_dir}/plugin/plugin.yaml"
printf 'tool\n' >"${tmp_dir}/plugin/.gitignore"
git -C "${tmp_dir}/plugin" add .gitignore plugin.yaml
git -C "${tmp_dir}/plugin" -c user.name=test -c user.email=test@example.test \
	commit -qm "test plugin"
plugin_commit="$(git -C "${tmp_dir}/plugin" rev-parse HEAD)"
plugin_sha256="$(sha256_file "${tmp_dir}/plugin/tool")"
verify_git_binary_install \
	"${tmp_dir}/plugin" "${plugin_commit}" "tool" "${plugin_sha256}" "test plugin"
printf 'command: bin/diff\n' >"${tmp_dir}/plugin/plugin.yaml"
if (verify_git_binary_install \
	"${tmp_dir}/plugin" "${plugin_commit}" "tool" "${plugin_sha256}" "test plugin") \
	2>"${tmp_dir}/plugin-dirty"; then
	fail "verify_git_binary_install accepted modified plugin metadata"
fi
grep -Fq "test plugin source checkout is dirty" "${tmp_dir}/plugin-dirty"
printf 'command: tool\n' >"${tmp_dir}/plugin/plugin.yaml"
printf 'tampered' >>"${tmp_dir}/plugin/tool"
if (verify_git_binary_install \
	"${tmp_dir}/plugin" "${plugin_commit}" "tool" "${plugin_sha256}" "test plugin") \
	2>"${tmp_dir}/plugin-checksum"; then
	fail "verify_git_binary_install accepted a modified executable"
fi
grep -Fq "test plugin executable checksum mismatch" "${tmp_dir}/plugin-checksum"

assert_eq "matching invariant" "value" "value"
if (assert_eq "named invariant" "actual" "expected") 2>"${tmp_dir}/assert"; then
	fail "assert_eq accepted different values"
fi
grep -Fqx "named invariant: got 'actual', want 'expected'" "${tmp_dir}/assert"

# The false case is the regression that matters: every clean checkout reports dirty=false,
# and a `jq -e` read of it aborted check:infra, the image builds, and the backup drill.
flag="$(json_flag '.dirty' '{"dirty":false}')"
assert_eq "false flag" "${flag}" "false"
flag="$(json_flag '.dirty' '{"dirty":true}')"
assert_eq "true flag" "${flag}" "true"
for invalid in '{}' '{"dirty":null}' '{"dirty":"false"}' '{"dirty":0}'; do
	if (json_flag '.dirty' "${invalid}") >/dev/null 2>"${tmp_dir}/flag"; then
		fail "json_flag accepted a non-boolean: ${invalid}"
	fi
	grep -Fqx "expected a JSON boolean at .dirty" "${tmp_dir}/flag"
done
