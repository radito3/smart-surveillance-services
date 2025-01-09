#!/bin/bash

# BASEPATH="<repo dir>"
# python3 -m venv ${BASEPATH}/venv
# source ${BASEPATH}/venv/bin/activate
# python3 -m pip install -r ${BASEPATH}/requirements.txt

export PYTHONUNBUFFERED=1

python3 app.py $@
