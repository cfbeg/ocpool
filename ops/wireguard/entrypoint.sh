#!/bin/sh
set -eu
umask 077
mkdir -p /run/ocpool
# Go reads the read-only base config and public catalog, writes its controlled
# private runtime template, and initializes wg0. Config hooks are never copied.
exec ocpool-egress
