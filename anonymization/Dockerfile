FROM python:3.12-bookworm

WORKDIR /app

RUN apt-get update && \
    apt-get install -y ffmpeg && \
    apt-get clean

COPY start.sh app.py requirements.txt face_anon_filter.py /app/

RUN pip install -r requirements.txt && \
    chmod +x start.sh

ENTRYPOINT [ "./start.sh" ]
