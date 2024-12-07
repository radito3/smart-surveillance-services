import logging
import os
import time
import cv2
import subprocess
import sys

from face_anon_filter import FaceAnonymizer


def start_ffmpeg_subprocess(width: int, height: int, target: str):
    args = [
        "ffmpeg", "-f", "rawvideo", "-vcodec", "rawvideo", "-pix_fmt", "bgr24", "-s", "640x360",
        "-i", "pipe:0",
        "-vf", f"scale={str(width)}:{str(height)}",
        "-c:v", "libx264",
        "-profile:v", "high", "-level:v", "4.0", "-pix_fmt", "yuv420p",
        "-preset", "ultrafast", "-tune", "zerolatency",
        "-f", "rtsp", "-rtsp_transport", "tcp",
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

    # introduce a framerate upper bound to accommodate the difference in input and output framerates
    target_fps = 12
    frame_interval = 1 / target_fps
    last_frame_time = time.time()
    while video_capture.isOpened():
        ok, frame = video_capture.read()
        if not ok:
            logging.error("Could not read frame from source. Exiting...")
            break

        current_time = time.time()
        if current_time - last_frame_time < frame_interval:
            continue  # skip frames

        last_frame_time = current_time
        resized_frame = cv2.resize(frame, (640, 360))
        processed_frame = processor(resized_frame)
        ffmpeg_process.stdin.write(processed_frame.tobytes())

    video_capture.release()
    ffmpeg_process.stdin.close()
    ffmpeg_process.wait()


if __name__ == '__main__':
    if len(sys.argv) != 3:
        logging.error("Invalid command-line arguments. Required <source_url> <target_url>")
        sys.exit(1)

    main(sys.argv[1], sys.argv[2])
