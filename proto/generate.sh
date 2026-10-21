#!/usr/bin/env bash
# Regenerates the Go code for every .proto file under proto.
# Requirements: protoc and protoc-gen-go (see README.md in this directory).
set -euo pipefail
cd "$(dirname "$0")"
for dir in */; do
	dir="${dir%/}"
	shopt -s nullglob
	files=("$dir"/*.proto)
	shopt -u nullglob
	[ ${#files[@]} -eq 0 ] && continue
	protoc --proto_path=. --go_out=. --go_opt=paths=source_relative "${files[@]}"
done
