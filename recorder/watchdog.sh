#!/bin/bash

WATCH_DIR=$(pwd)
LOG_FILE="${WATCH_DIR}/file_log.txt"

touch "$LOG_FILE"

# Watch the directory for new files
inotifywait -m -e create --format '%f' "$WATCH_DIR" | while read NEW_FILE; do
    if [[ "$NEW_FILE" != "$(basename "$LOG_FILE")" ]]; then
        echo "file $NEW_FILE" >> "$LOG_FILE"
    fi
done
