FROM python:3.12-bookworm

RUN apt-get update && \
    apt-get install -y ffmpeg && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY start.sh app.py requirements.txt face_anon_filter.py /app/

RUN pip install -r requirements.txt && \
    chmod +x start.sh

EXPOSE 8554

ENTRYPOINT [ "./start.sh" ]
