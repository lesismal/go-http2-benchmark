#!/bin/bash

. ./script/env.sh

framework=$1
shift

echo "run ${framework} server on cpu ${server_cpu_list:-unbound}"
# "$@" rather than $2 through $9: the taskpool flags alone are four of them.
nohup $limit_cpu_server "./output/bin/${framework}.server" "$@" \
    >"$(bench_server_log "$framework")" 2>&1 &
# nohup and taskset each exec the next, so this is the server's own pid, which
# bench_start_server and bench_stop_server in env.sh watch it by.
mkdir -p ./output/run
echo $! >"./output/run/${framework}.pid"
