import cv2
import sys
import numpy as np


def compare_frames(frame1, frame2, threshold=30):
    diff = cv2.absdiff(frame1, frame2)
    gray = cv2.cvtColor(diff, cv2.COLOR_BGR2GRAY)
    _, thresh = cv2.threshold(gray, threshold, 255, cv2.THRESH_BINARY)
    non_zero_count = np.count_nonzero(thresh)
    return non_zero_count > 0


def process_video(video_url: str, recoding_dir: str, idle_time: int = 30, low_fps: int = 1, high_fps: int = 24):
    cap = cv2.VideoCapture(video_url)
    width = int(cap.get(cv2.CAP_PROP_FRAME_WIDTH))
    height = int(cap.get(cv2.CAP_PROP_FRAME_HEIGHT))
    fourcc = cv2.VideoWriter_fourcc(*'mp4v')

    # TODO: if this script fails and is restarted, check the highest index of video file currently present and overwrite it
    current_segment = 0
    path_template = recoding_dir + "recording_{0}.mp4"

    out = cv2.VideoWriter(path_template.format(current_segment), fourcc, high_fps, (width, height))

    idle_frames = 0
    current_fps = high_fps

    ret, prev_frame = cap.read()
    if not ret:
        print("Error: Unable to read the video file.")
        return

    while cap.isOpened():
        ret, frame = cap.read()
        if not ret:
            break

        if compare_frames(prev_frame, frame):
            idle_frames = 0
            if current_fps != high_fps:
                current_fps = high_fps
                out.release()
                current_segment += 1
                out = cv2.VideoWriter(path_template.format(current_segment), fourcc, high_fps, (width, height))
        else:
            idle_frames += 1
            if idle_frames >= idle_time * current_fps:
                current_fps = low_fps
                out.release()
                current_segment += 1
                out = cv2.VideoWriter(path_template.format(current_segment), fourcc, low_fps, (width, height))

        out.write(frame)
        prev_frame = frame

    cap.release()
    out.release()

if __name__ == "__main__":
    video_url = sys.argv[1]
    process_video(video_url, "/app")
