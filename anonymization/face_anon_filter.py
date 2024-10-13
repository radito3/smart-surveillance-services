import cv2
import torch
from facenet_pytorch import MTCNN


class FaceAnonymizer:

    # https://medium.com/@danushidk507/facenet-pytorch-pretrained-pytorch-face-detection-mtcnn-and-facial-recognition-b20af8771144
    def __init__(self):
        self.detector = MTCNN(keep_all=True, device=self.__get_device())
        self.current_faces = []

    # FIXME: this still drops the rectangles sometimes, leading to periods of non-anonymized faces
    def update_faces(self, new_faces):
        if new_faces is not None and len(new_faces) > 0:
            self.current_faces = new_faces

    @staticmethod
    def __get_device() -> torch.device:
        device = torch.device('cpu')
        if torch.cuda.is_available():
            device = torch.device('cuda')
        elif torch.backends.mps.is_available():
            device = torch.device('mps')
        return device

    @staticmethod
    def blur_face(image, x1, y1, x2, y2):
        # Extract the region of interest (face)
        roi = image[y1:y2, x1:x2]
        blurred_roi = cv2.GaussianBlur(roi, (99, 99), 30)
        image[y1:y2, x1:x2] = blurred_roi
        return image

    @staticmethod
    def pixelate_face(image, x1, y1, x2, y2):
        face = image[y1:y2, x1:x2]
        height, width = face.shape[:2]
        temp = cv2.resize(face, (width // 10, height // 10), interpolation=cv2.INTER_LINEAR)
        pixelated_face = cv2.resize(temp, (width, height), interpolation=cv2.INTER_NEAREST)
        image[y1:y2, x1:x2] = pixelated_face
        return image

    def __call__(self, image, effect='blur'):
        boxes, _ = self.detector.detect(image)
        self.update_faces(boxes)

        for box in self.current_faces:
            x1, y1, x2, y2 = [int(b) for b in box]
            match effect:
                case 'blur':
                    image = self.blur_face(image, x1, y1, x2, y2)
                case 'pixelate':
                    image = self.pixelate_face(image, x1, y1, x2, y2)

        return image
