#!/bin/bash

nginx &

ffmpeg -i "$1" -filter_complex \
"[0:v]split=5[v1][v2][v3][v4][v5]; \
 [v1]scale=640:-2[v1out]; \
 [v2]scale=854:-2[v2out]; \
 [v3]scale=1280:-2[v3out]; \
 [v4]scale=1920:-2[v4out]; \
 [v5]scale=1920:-2[v5out]" \
-map "[v1out]" -b:v:0 800k -g 48 -keyint_min 48 -sc_threshold 0 -hls_time 4 -hls_playlist_type vod \
  -hls_segment_filename /var/www/hls/640p_%03d.ts -f hls -master_pl_name master.m3u8 -var_stream_map "v:0,name:640p v:1,name:854p v:2,name:1280p" /var/www/hls/640p.m3u8 \
-map "[v2out]" -b:v:1 1400k -g 48 -keyint_min 48 -sc_threshold 0 -hls_time 4 \
  -hls_segment_filename /var/www/hls/854p_%03d.ts -f hls /var/www/hls/854p.m3u8 \
-map "[v3out]" -b:v:2 2500k -g 48 -keyint_min 48 -sc_threshold 0 -hls_time 4 \
  -hls_segment_filename /var/www/hls/1280p_%03d.ts -f hls /var/www/hls/1280p.m3u8 \
-map "[v1out]" -b:v:0 800k -g 48 -keyint_min 48 -f dash /var/www/dash/dash_640p.mpd \
-map "[v2out]" -b:v:1 1400k -g 48 -keyint_min 48 -f dash /var/www/dash/dash_854p.mpd \
-map "[v3out]" -b:v:2 2500k -g 48 -keyint_min 48 -f dash /var/www/dash/dash_1280p.mpd \
-map "[v4out]" -b:v:3 3000k -f rtsp rtsp://localhost:8554/live.sdp \
-map "[v5out]" -b:v:4 3000k -f flv rtmp://localhost/live/stream
