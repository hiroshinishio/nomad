#!/bin/sh

set -e

generate_file() {
    cat <<EOF
// +build ent

// Code generated by go generate; DO NOT EDIT.
package raftutil

import "github.com/hashicorp/nomad/nomad/structs"

func init() {
EOF

    cat ../../nomad/structs/structs_ent.go \
        | grep -A500 'MessageType =' \
        | grep -v -e '//' \
        | awk '/^\)$/ { exit; } /.*/ { printf "  msgTypeNames[structs.%s] = \"%s\"\n", $1, $1}'

    echo '}'
}

generate_file > msgtypes_ent.go

gofmt -w msgtypes_ent.go