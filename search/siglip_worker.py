# Reads pictures and words for the server's search (package search) with a SigLIP 2 model, so that a picture and the
# words that describe it get vectors pointing the same way. Started by the server with the model's Hugging Face name;
# answers one JSON line on stdout for each JSON line on stdin:
#
#   (on start)                       {"ready": true, "dims": 768, "device": "cpu"}, or {"error": "..."}
#   {"images": ["/a.jpg", "/b.jpg"]} {"vectors": ["<base64>", null], "errors": [null, "cannot identify image"]}
#   {"text": "dog on the beach"}     {"vector": "<base64>"}, or {"error": "..."}
#
# Vectors are unit length, as little-endian float32, in base64.
import base64
import json
import os
import sys

os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")
os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")

import torch
from PIL import Image
from transformers import AutoModel, AutoProcessor


def answer(value):
    print(json.dumps(value), flush=True)


def features(output):
    # transformers 4 returns the vectors; later versions may return an output with them in pooler_output.
    if not torch.is_tensor(output):
        output = output.pooler_output
    output = output / output.norm(dim=-1, keepdim=True)
    return output.float().cpu().numpy()


def encode(vector):
    return base64.b64encode(vector.astype("<f4").tobytes()).decode("ascii")


def main():
    torch.set_grad_enabled(False)
    # Leave half the processor to the server; a GPU (Apple's or NVIDIA's) is used when there is one.
    torch.set_num_threads(max(1, (os.cpu_count() or 2) // 2))
    device = "cpu"
    if torch.backends.mps.is_available():
        device = "mps"
    elif torch.cuda.is_available():
        device = "cuda"
    try:
        model = AutoModel.from_pretrained(sys.argv[1]).to(device).eval()
        processor = AutoProcessor.from_pretrained(sys.argv[1])
    except Exception as e:
        answer({"error": "loading %s: %s" % (sys.argv[1], e)})
        return

    def text_vector(text):
        # SigLIP 2 was trained on lowercase text padded to 64 tokens.
        inputs = processor(text=[text.lower()], padding="max_length", max_length=64, truncation=True, return_tensors="pt")
        return features(model.get_text_features(**inputs.to(device)))[0]

    answer({"ready": True, "dims": int(text_vector("a photo").shape[0]), "device": device})
    for line in sys.stdin:
        if not line.strip():
            continue
        try:
            request = json.loads(line)
            if "text" in request:
                answer({"vector": encode(text_vector(request["text"]))})
                continue
            paths = request["images"]
            pictures, errors = [], [None] * len(paths)
            for i, path in enumerate(paths):
                try:  # one bad picture must not fail the others
                    with Image.open(path) as picture:
                        pictures.append((i, picture.convert("RGB")))
                except Exception as e:
                    errors[i] = str(e)
            vectors = [None] * len(paths)
            if pictures:
                inputs = processor(images=[p for _, p in pictures], return_tensors="pt")
                for (i, _), vector in zip(pictures, features(model.get_image_features(**inputs.to(device)))):
                    vectors[i] = encode(vector)
            answer({"vectors": vectors, "errors": errors})
        except Exception as e:
            answer({"error": str(e)})


main()
