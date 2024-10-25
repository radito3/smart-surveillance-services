import os
import cv2
import subprocess
import sys
import errno
import numpy as np

from face_anon_filter import FaceAnonymizer


processor = FaceAnonymizer()
# TODO: extract from env
width, height = 1920, 1080

while True:
    in_bytes = sys.stdin.buffer.read(width * height * 3)
    if not in_bytes:
        break

    frame = np.frombuffer(in_bytes, np.uint8).reshape((height, width, 3))

    processed_frame = processor(frame)

    sys.stdout.buffer.write(processed_frame.tobytes())
