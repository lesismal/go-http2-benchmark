#!/bin/bash

# . ./script/env.sh
# . ./script/config.sh

# Every server at once, for BENCH_ROLE=server: the client node runs against
# them for the whole run. On one machine script/clients.sh starts each server
# for its own turn instead.

# Flags for every server, set by the driver that sources this: the ones the
# servers define, such as -nodelay, with the benchmark client's own filtered
# out. Read from a variable rather than from "$@" because `source file` with no
# arguments leaves the caller's positional parameters in place, which is how
# the client's flags would otherwise reach the servers.
if [ -z "${server_flags+set}" ]; then
    # Invoked directly rather than sourced: take our own arguments.
    server_flags="$*"
fi

servers_status=0
for f in ${frameworks[@]}; do
    echo
    bench_start_server "$f" || servers_status=1
done

return "$servers_status" 2>/dev/null || exit "$servers_status"
