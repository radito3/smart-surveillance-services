import logging
import os
import cv2
import subprocess
import sys
import numpy as np

from face_anon_filter import FaceAnonymizer


def start_ffmpeg_subprocess(width: int, height: int, target: str):
    args = [
        "ffmpeg", "-f", "rawvideo", "-pix_fmt", "rgb24", "-s", str(width) + "x" + str(height),
        "-i", "pipe:0",
        "-c:v", "libx264", "-f", "rtsp", "-rtsp_transport", "tcp",
        target
    ]
    return subprocess.Popen(args, stdin=subprocess.PIPE)


def main(source: str, target: str):
    os.environ["OPENCV_FFMPEG_CAPTURE_OPTIONS"] = "rtsp_transport;tcp"
    video_capture = cv2.VideoCapture(source, cv2.CAP_FFMPEG)
    if not video_capture.isOpened():
        logging.error(f"Could not open video stream for: {source}")
        return

    width = video_capture.get(cv2.CAP_PROP_FRAME_WIDTH)
    height = video_capture.get(cv2.CAP_PROP_FRAME_HEIGHT)

    processor = FaceAnonymizer()
    ffmpeg_process = start_ffmpeg_subprocess(int(width), int(height), target)

    while video_capture.isOpened():
        ok, frame = video_capture.read()
        if not ok:
            logging.error("Could not read frame from source. Exiting...")
            break
        processed_frame = processor(frame)
        ffmpeg_process.stdin.write(processed_frame.astype(np.uint8).tobytes())

    video_capture.release()
    ffmpeg_process.stdin.close()
    ffmpeg_process.wait()


if __name__ == '__main__':
    if len(sys.argv) != 3:
        logging.error("Invalid command-line arguments. Required <source_url> <target_url>")
        sys.exit(1)

    main(sys.argv[1], sys.argv[2])
