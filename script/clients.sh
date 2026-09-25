#!/bin/bash

# . ./script/env.sh

clients_status=0

if ! bench_owns_servers; then
    echo "servers are on ${BENCH_SERVER_HOST} and stay up for the whole run;"
    echo "stop them there with script/killall.sh when it is done"
fi

framework_index=0
for f in ${frameworks[@]}; do
    framework_index=$((framework_index + 1))
    echo
    # On one machine each server runs only for its own turn: started just
    # before its client, once it is listening, and stopped as soon as the
    # client is done, so that no other framework's server holds CPU, memory
    # or sockets while one is measured. Only the machine that started a server
    # can stop it, and only it should: killing by process name on a shared
    # client node would reach whatever else is running there.
    if bench_owns_servers; then
        if ! bench_start_server "$f"; then
            echo "skip ${f}: its server did not start"
            clients_status=1
            continue
        fi
        sleep 1
    fi
    # echo "start bench ${f}" "$@"
    echo "run client to ${f} at ${BENCH_SERVER_HOST}, on cpu ${client_cpu_list:-unbound}"
    # -ip first, so a host given on the command line still wins: both clients
    # take the last value of a repeated flag.
    . ./script/client.sh -f=$f -ip=${BENCH_SERVER_HOST} "$@" || clients_status=$?
    if bench_owns_servers; then
        bench_stop_server "$f"
    fi
    # Time for the sockets of this run to wind down before the next one;
    # nothing follows the last.
    if [ "$framework_index" -lt "${#frameworks[@]}" ]; then
        for ((i = 1; i <= $SleepTime; i++)); do
            echo "sleep $i ..."
            sleep 1
        done
    fi
done

if bench_owns_servers; then
    . ./script/killall9.sh
fi

return "$clients_status" 2>/dev/null || exit "$clients_status"
