#!/bin/bash

./watchdog.sh & 

trap 'kill %%' EXIT

python3 app.py "$1"
