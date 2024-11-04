import os
import cv2
import subprocess
import sys
import errno
import numpy as np

from face_anon_filter import FaceAnonymizer


processor = FaceAnonymizer()
width, height = os.environ['DIMS'].split('x')

while True:
    in_bytes = sys.stdin.buffer.read(int(width) * int(height) * 3)
    if not in_bytes:
        break

    frame = np.frombuffer(in_bytes, np.uint8).reshape((int(height), int(width), 3))

    processed_frame = processor(frame)

    sys.stdout.buffer.write(processed_frame.tobytes())
