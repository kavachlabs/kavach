#!/bin/bash
mkdir -p /logs/verifier
if [ "$(cat /app/hello.txt 2>/dev/null)" = hello ]; then echo 1; else echo 0; fi > /logs/verifier/reward.txt
