#!/bin/sh

set -eu
set -x

mkdir -p /tmp/extracted
wget -O /tmp/out.zip "$1"
unzip -d /tmp/extracted /tmp/out.zip
unset dir
# GitHub zip files always have an inner directory with a name based on repo /
# ref. Assume it's the only one and give it a known name.
dir="$(ls /tmp/extracted)"
mv "/tmp/extracted/$dir" /workspace/src
