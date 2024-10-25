FROM python:3.12

WORKDIR /app

COPY app.py requirements.txt watchdog.sh /app/

RUN pip install -r requirements.txt

EXPOSE 8554

CMD ["/bin/bash", "-c", "watchdog.sh & ; python3 app.py"]
