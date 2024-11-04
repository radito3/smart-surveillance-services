FROM python:3.12-bookworm

WORKDIR /app

COPY app.py requirements.txt watchdog.sh start.sh /app/

RUN apt-get update && \
    apt-get install -y inotify-tools && \   
    pip install -r requirements.txt && \
    chmod +x start.sh watchdog.sh

EXPOSE 8554

ENTRYPOINT [ "./start.sh" ]
