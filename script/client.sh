#!/bin/bash

. ./script/env.sh || { return 1 2>/dev/null || exit 1; }

# echo "run client on cpu ${client_cpu_num}-$((total_cpu_num - 1))"
# The client BENCH_CLIENT picks (see script/config.sh); all take these flags.
if [ "$BENCH_CLIENT" = go ]; then
    $limit_cpu_client ./output/bin/bench.client "$@"
else
    $limit_cpu_client "./output/bin/${BENCH_CLIENT}.client" "$@"
fi
