FROM python:3.12

RUN apt-get update && \
    apt-get install -y ffmpeg && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY app.py requirements.txt face_anon_filter.py /app/

RUN pip install -r requirements.txt

EXPOSE 8554

CMD ["/bin/bash", "-c", "ffmpeg -i input.mp4 -vf fifo -f rawvideo pipe:1 | python3 app.py | ffmpeg -f rawvideo -pixel_format rgb24 -video_size 1920x1080 -i pipe:0 -c:v libx264 -f rtsp rtsp://127.0.0.1:8554/live/stream"]
