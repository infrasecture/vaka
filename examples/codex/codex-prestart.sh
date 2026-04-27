#!/usr/bin/env bash
set -euo pipefail

codex_home="${CODEX_HOME:-/root/.codex}"
config_file="${codex_home}/config.toml"
tmp_file="${config_file}.tmp.$$"

mkdir -p "${codex_home}"
touch "${config_file}"

awk '
  BEGIN {
    print "model_provider = \"litellm\""
  }

  function is_table(line) {
    return line ~ /^[[:space:]]*\[[^]]+\][[:space:]]*([#].*)?$/
  }

  /^[[:space:]]*\[model_providers\.litellm\][[:space:]]*([#].*)?$/ ||
  /^[[:space:]]*\[projects\."\/workspace"\][[:space:]]*([#].*)?$/ {
    skip_managed_block = 1
    next
  }

  skip_managed_block && is_table($0) {
    skip_managed_block = 0
  }

  skip_managed_block {
    next
  }

  is_table($0) {
    in_table = 1
  }

  !in_table && /^[[:space:]]*(model_provider|openai_base_url)[[:space:]]*=/ {
    next
  }

  {
    print
  }

  END {
    print ""
    print "[projects.\"/workspace\"]"
    print "trust_level = \"trusted\""
    print ""
    print "[model_providers.litellm]"
    print "name = \"LiteLLM local proxy\""
    print "base_url = \"http://litellm:4000/v1\""
    print "wire_api = \"responses\""
    print "env_key = \"OPENAI_API_KEY\""
    print "requires_openai_auth = false"
    print "supports_websockets = false"
  }
' "${config_file}" > "${tmp_file}"

mv "${tmp_file}" "${config_file}"

# Optional interactive attach (disabled by default; enable with CODEX_AUTO_ATTACH=1)
if [[ -t 0 && -t 1 && "${CODEX_AUTO_ATTACH:-0}" == "1" ]]; then
  exec byobu -r "${SESSION}" 2>/dev/null || exec byobu -r
fi

# Keep container alive for later exec/attach
exec sleep infinity
