#!/bin/bash

set -e

dims=$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height \
    -of compact=print_section=0:nokey=1:item_sep=x "$1")

export DIMS=${dims}

# BASEPATH="<repo dir>"
# python3 -m venv ${BASEPATH}/venv
# source ${BASEPATH}/venv/bin/activate
# python3 -m pip install -r ${BASEPATH}/requirements.txt

ffmpeg -i "$1" -f rawvideo pipe:1 | \
    python3 app.py | \
    ffmpeg -i pipe:0 -f rawvideo -pixel_format rgb24 -video_size "${dims}" -c:v libx264 -f rtsp ${2}
