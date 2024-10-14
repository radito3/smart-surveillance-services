import os
import cv2
import subprocess
import sys
import errno

from face_anon_filter import FaceAnonymizer

input_fifo = '/tmp/video_input_fifo'
output_fifo = '/tmp/video_output_fifo'

if not os.path.exists(input_fifo):
    os.mkfifo(input_fifo)
if not os.path.exists(output_fifo):
    os.mkfifo(output_fifo)

# Start ffmpeg subprocess to handle stdin -> FIFO
ffmpeg_read_cmd = [
    'ffmpeg',
    '-i', '-',              # Read from stdin
    '-f', 'rawvideo',       # Output raw video
    '-pix_fmt', 'bgr24',    # Format OpenCV expects
    input_fifo              # Output to FIFO
]
ffmpeg_read_process = subprocess.Popen(ffmpeg_read_cmd, stdin=subprocess.PIPE)

# Start ffmpeg subprocess to handle FIFO -> stdout
ffmpeg_write_cmd = [
    'ffmpeg',
    '-f', 'rawvideo',
    '-pix_fmt', 'bgr24',
    '-s', '640x480',        # Adjust based on input resolution
    '-r', '30',             # Frame rate
    '-i', output_fifo,      # Read from output FIFO
    '-vcodec', 'libx264',
    '-f', 'mp4',
    '-'  # Write to stdout
]
ffmpeg_write_process = subprocess.Popen(ffmpeg_write_cmd, stdin=subprocess.PIPE, stdout=sys.stdout)

# OpenCV Capture and Processing
cap = cv2.VideoCapture(input_fifo)  # Use FIFO for input

# Initialize VideoWriter for the output FIFO
fourcc = cv2.VideoWriter_fourcc(*'XVID')
out = cv2.VideoWriter(output_fifo, fourcc, 30, (640, 480))  # Adjust FPS and resolution
video_filter = FaceAnonymizer()

while cap.isOpened():
    try:
        ret, frame = cap.read()
        if not ret:
            break

        processed_frame = video_filter(frame)

        # Write processed frame to the output FIFO (non-blocking)
        out.write(processed_frame)

    except IOError as e:
        if e.errno == errno.EAGAIN:
            # If FIFO is not ready, continue looping without blocking
            continue

# Cleanup
cap.release()
out.release()

ffmpeg_read_process.stdin.close()
ffmpeg_write_process.stdin.close()
ffmpeg_read_process.wait()
ffmpeg_write_process.wait()
