#!/usr/bin/env bash
# Run inside a fresh guest as its unprivileged runner user. The runner package
# and OS dependencies must already be installed. Never run this on Metal itself.
set -euo pipefail
set +x
if [[ $EUID == 0 ]]; then
  echo 'Run as the guest runner user, not root.' >&2
  exit 1
fi
if [[ $# != 2 ]]; then
  echo 'Usage: guest-runner.sh RUNNER_DIRECTORY JIT_FILE' >&2
  exit 1
fi
runner_dir=$(realpath "$1")
jit_file=$(realpath "$2")
[[ -x "$runner_dir/run.sh" && -f "$jit_file" && ! -L "$2" ]] || exit 1
[[ $(stat -c %u "$jit_file") == "$EUID" ]] || { echo 'JIT file must belong to runner user.' >&2; exit 1; }
[[ $(stat -c %a "$jit_file") == 600 ]] || { echo 'JIT file must have mode 600.' >&2; exit 1; }
# Consume the transport file before invoking runner. The runner consumes this
# official environment input; its JIT secret does not become a command argument.
IFS= read -r jit_config < "$jit_file"
[[ -n "$jit_config" ]] || exit 1
rm -- "$jit_file"
cd "$runner_dir"
# Adopt a baked tool cache when the prepared image provides one. The runner reads
# RUNNER_TOOL_CACHE ahead of every other tool directory variable and otherwise
# defaults to <work>/_tool, which a baked image never populates. An inherited
# value wins: the runner supports one already. Only adopt the baked default when
# none was supplied and the prepared directory already exists and is writable, so
# an original bare image keeps the runner's own default. Absent is not an error.
if [[ -z ${RUNNER_TOOL_CACHE:-} && -d /opt/hostedtoolcache && -w /opt/hostedtoolcache ]]; then
  export RUNNER_TOOL_CACHE=/opt/hostedtoolcache
fi
export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit_config"
unset jit_config
exec ./run.sh
