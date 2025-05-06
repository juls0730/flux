#!/usr/bin/env bash

# Basic test for flux

set -ex

Flux_Database_Dir=$(mktemp -d)c

zqdgr build:all

tmux new-session -d -s daemon
# start daemon
tmux send-keys -t daemon "export FLUXD_ROOT_DIR=$Flux_Database_Dir" C-m
tmux send-keys -t daemon "DEBUG=true zqdgr run:daemon" C-m


# test daemon with the cli
tmux split-window -h

export FLUX_CLI_PATH=$PWD/flux

tmux send-keys -t daemon:0.1 "cd \$(mktemp -d)" C-m
tmux send-keys -t daemon:0.1 "cat << EOF > test.sh
#!/usr/bin/env bash

set -xe

# wait for the daemon to initialize
sleep 2

go mod init testApp
$FLUX_CLI_PATH init --host-url testApp --project-port 8080 testApp

cat << ELOF > main.go
package main

import (
    \"fmt\"
    \"net/http\"
)

func main() {
    http.HandleFunc(\"/\", func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprintf(w, \"Hello World\\n\")
    })
    http.ListenAndServe(\":8080\", nil)
}
ELOF

$FLUX_CLI_PATH deploy -q

curl -H \"Host: testApp\" localhost:7465

$FLUX_CLI_PATH stop

curl -H \"Host: testApp\" localhost:7465

$FLUX_CLI_PATH start

curl -H \"Host: testApp\" localhost:7465

sed -i 's/Hello World/Hello World 2/' main.go

$FLUX_CLI_PATH deploy -q

curl -H \"Host: testApp\" localhost:7465

$FLUX_CLI_PATH delete --no-confirm
EOF
"


tmux send-keys -t daemon:0.1 "chmod +x test.sh" C-m
tmux send-keys -t daemon:0.1 "./test.sh" C-m
tmux attach-session -d