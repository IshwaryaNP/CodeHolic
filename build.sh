#!/bin/sh
set -e

echo "Building kvdb..."
go build -o kvdb ./cmd/kvdb

echo "Building torture..."
go build -o torture ./cmd/torture

echo "Successfully built kvdb and torture"

