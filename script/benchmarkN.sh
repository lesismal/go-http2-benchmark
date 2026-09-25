#!/bin/bash

# Every framework through the Connections x BodySize x BenchTime matrix in
# script/config.sh, one report per combination, each named by its suffix:
# output/report/BenchEcho_<conns>_<payload>_<times>.md and so on.

. ./script/env.sh || { return 1 2>/dev/null || exit 1; }

echo $line
. ./script/clean.sh

echo $line

print_env

echo $line

. ./script/build.sh || { return 1 2>/dev/null || exit 1; }

echo $line

. ./script/killall.sh
sleep 1
# The servers and the benchmark client take different flags, and this script
# takes the client's. Forward only what a server actually defines.
server_flags=""
for arg in "$@"; do
    case "$arg" in
        -nodelay=*|-reuseport=*|-b=*|-m=*|-maxstreams=*) server_flags="${server_flags} ${arg}" ;;
    esac
done

# A server node starts every server now and leaves them up for the client
# node. On one machine each server runs only for its own framework's turn,
# below.
if ! bench_runs_clients; then
    if ! . ./script/servers.sh; then
        echo "not every server started, see above. Stop the others with: bash script/killall.sh"
        return 1 2>/dev/null || exit 1
    fi
    echo $line
    echo "servers are up and left running. On the client node:"
    echo "  BENCH_ROLE=client BENCH_SERVER_HOST=<this host> bash script/benchmarkN.sh"
    echo "Stop them here afterwards with: bash script/killall.sh"
    return 0 2>/dev/null || exit 0
fi
echo $line

benchN_status=0
framework_index=0
for f in ${frameworks[@]}; do
    framework_index=$((framework_index + 1))
    # Started once it is listening, and stopped as soon as its whole matrix
    # has run; see script/clients.sh.
    if bench_owns_servers; then
        if ! bench_start_server "$f"; then
            echo "skip ${f}: its server did not start"
            benchN_status=1
            continue
        fi
        sleep 1
    fi
    first_run=true
    for c in ${Connections[@]}; do
        for b in ${BodySize[@]}; do
            for n in ${BenchTime[@]}; do
                # Time for the sockets of one run to wind down before the next.
                if [ "$first_run" != true ]; then
                    sleep $SleepTime
                fi
                first_run=false
                suffix="_${c}_${b}_${n}"
                echo "run client to ${f} at ${BENCH_SERVER_HOST}: ${c} connections, ${b} payload, ${n} times"
                if ! . ./script/client.sh -f=$f -ip=${BENCH_SERVER_HOST} -c=$c -b=$b -en=$n -suffix=${suffix} -rate=true "$@"; then
                    bench_owns_servers && bench_stop_server "$f"
                    return 1 2>/dev/null || exit 1
                fi
            done
        done
    done
    if bench_owns_servers; then
        bench_stop_server "$f"
    fi
    # Nothing follows the last framework.
    if [ "$framework_index" -lt "${#frameworks[@]}" ]; then
        sleep $SleepTime
    fi
done

# The report step reads BENCH_REPORT_SORT for the row order of its tables; see
# script/config.sh.
for c in ${Connections[@]}; do
    for b in ${BodySize[@]}; do
        for n in ${BenchTime[@]}; do
            suffix="_${c}_${b}_${n}"
            . ./script/report.sh -suffix=${suffix} "$@"
        done
    done
done

return "$benchN_status" 2>/dev/null || exit "$benchN_status"
