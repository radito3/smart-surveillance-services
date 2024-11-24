FROM ubuntu:24.04

RUN apt-get update && apt-get install -y \
    ffmpeg \
    nginx \
    libnginx-mod-rtmp \
    curl \
    && rm -rf /var/lib/apt/lists/*

COPY nginx.conf /etc/nginx/nginx.conf

RUN mkdir -p /var/www/hls /var/www/dash

# Expose the necessary ports (RTMP, HLS, DASH, RTSP, WebRTC)
EXPOSE 1935 8080 8554 8004

COPY start.sh /start.sh
RUN chmod +x /start.sh

ENTRYPOINT ["/start.sh"]
