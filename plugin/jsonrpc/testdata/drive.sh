#!/usr/bin/env bash
# Drives a provider from a shell script with no client but jq: two requests on
# standard input, two replies and the streamed resources on standard output.
set -euo pipefail

provider=$1

{
  echo '{"jsonrpc":"2.0","id":1,"method":"configure","params":{"expectAccount":"demo"}}'
  echo '{"jsonrpc":"2.0","id":2,"method":"list","params":{"scope":{"ProjectID":"demo"}}}'
} | "$provider" 2>/dev/null | jq -c '
    if .method == "resource" then {streamed: .params.displayName}
    elif .id == 1 then {granted: .result.granted, account: .result.account}
    elif .id == 2 then {asked: .result.asked, gaps: (.result.gaps // [] | length)}
    else empty end'
