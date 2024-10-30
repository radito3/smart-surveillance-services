#!/bin/bash

dims=$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height \
    -of compact=print_section=0:nokey=1:item_sep=x "$1")

export DIMS=${dims}

ffmpeg -i "$1" -vf fifo -f rawvideo pipe:1 | \
    python3 app.py | \
    ffmpeg -f rawvideo -pixel_format rgb24 -video_size "${dims}" -i pipe:0 -c:v libx264 -f rtsp rtsp://127.0.0.1:8554/${2}
