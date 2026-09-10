"""API-format ComfyUI graphs for MiniMax H3 (mirrors the official templates
video_minimax_h3_t2v / i2v / r2v shipped with ComfyUI 0.35).

All three modes share the same tail:
  UNETLoader -> (LoraLoaderModelOnly turbo) -> (MiniMaxH3SigmaShift) -> (ModelAttentionBackend)
  -> BasicGuider + BasicScheduler(res_multistep/simple) -> SamplerCustomAdvanced
  -> VAEDecode (video) + VAEDecodeAudio (audio) -> CreateVideo(24fps) -> SaveVideo
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Optional

FPS = 24

# Default model files (Comfy-Org/MiniMax-H3 on Hugging Face)
DEFAULT_FILES = {
    "fl2va": "minimax_h3_fl2va_pruned_int8_convrot.safetensors",
    "ref2va": "minimax_h3_ref2va_pruned_int8_convrot.safetensors",
    "clip": "qwen3vl_32b_minimax_h3_int8_convrot.safetensors",
    "vae_video": "minimax_h3_video_vae_fp16.safetensors",
    "vae_audio": "minimax_h3_audio_vae_fp32.safetensors",
    "lora_fl2v_8step": "minimax_h3_fl2v_turbo_8step_v1.0_comfyui_bf16.safetensors",
    "lora_fl2v_4step": "minimax_h3_fl2v_turbo_4step_v1.0_768p_comfyui_bf16.safetensors",
    "lora_ref2v_4step": "minimax_h3_ref2v_turbo_4step_v0.1_comfyui_bf16.safetensors",
}


def snap_frames(seconds: float) -> int:
    """Frame count on H3's 17k+5 grid at 24 fps (same expression as the official template)."""
    n = max(5, round(seconds * FPS))
    return n + (5 - (n % 17)) % 17


def canvas(aspect: str = "16:9", megapixels: float = 0.4) -> tuple[int, int]:
    """Resolution presets. H3's native canvas is 768 short edge, area <= 768*1344,
    multiples of 32. 0.4 MP (~768x512 class) is the fast draft size, 1.0 MP is native."""
    presets = {
        ("16:9", "native"): (1344, 768),
        ("9:16", "native"): (768, 1344),
        ("1:1", "native"): (1024, 1024),
        ("16:9", "draft"): (832, 480),
        ("9:16", "draft"): (480, 832),
        ("1:1", "draft"): (640, 640),
        ("16:9", "small"): (640, 352),
    }
    tier = "native" if megapixels >= 0.9 else ("draft" if megapixels >= 0.3 else "small")
    return presets.get((aspect, tier), presets[("16:9", "draft")])


@dataclass
class GenSpec:
    prompt: str
    mode: str = "t2v"  # t2v | i2v | r2v
    width: int = 832
    height: int = 480
    seconds: float = 5.0
    seed: int = 0
    steps: int = 8
    turbo: bool = True
    lora_strength: float = 1.0
    shift_video: Optional[float] = None  # None -> model default (12.0 / 3.0)
    shift_audio: Optional[float] = None
    attention: Optional[str] = None  # None | "pytorch attention" | "comfy kitchen attention"
    scheduler: str = "simple"
    sampler: str = "res_multistep"
    first_frame: Optional[str] = None  # uploaded image name (i2v)
    last_frame: Optional[str] = None
    ref_images: list[str] = field(default_factory=list)  # uploaded names (r2v)
    ref_audios: list[str] = field(default_factory=list)  # uploaded names (r2v)
    ref_image_size: str = "match"
    filename_prefix: str = "h3/h3"
    files: dict = field(default_factory=lambda: dict(DEFAULT_FILES))

    @property
    def length(self) -> int:
        return snap_frames(self.seconds)


def build(spec: GenSpec) -> dict:
    f = spec.files
    is_ref = spec.mode == "r2v"
    unet = f["ref2va"] if is_ref else f["fl2va"]
    g: dict[str, dict] = {}

    g["unet"] = {"class_type": "UNETLoader", "inputs": {"unet_name": unet, "weight_dtype": "default"}}
    g["clip"] = {"class_type": "CLIPLoader", "inputs": {"clip_name": f["clip"], "type": "minimax", "device": "default"}}
    g["vae_v"] = {"class_type": "VAELoader", "inputs": {"vae_name": f["vae_video"]}}
    g["vae_a"] = {"class_type": "VAELoader", "inputs": {"vae_name": f["vae_audio"]}}

    model_ref = ["unet", 0]
    if spec.turbo:
        if is_ref:
            lora = f["lora_ref2v_4step"]
        else:
            lora = f["lora_fl2v_4step"] if spec.steps <= 4 else f["lora_fl2v_8step"]
        g["lora"] = {"class_type": "LoraLoaderModelOnly",
                     "inputs": {"model": model_ref, "lora_name": lora, "strength_model": spec.lora_strength}}
        model_ref = ["lora", 0]
    if spec.shift_video is not None or spec.shift_audio is not None:
        g["shift"] = {"class_type": "MiniMaxH3SigmaShift",
                      "inputs": {"model": model_ref,
                                 "shift_video": spec.shift_video if spec.shift_video is not None else 12.0,
                                 "shift_audio": spec.shift_audio if spec.shift_audio is not None else 3.0}}
        model_ref = ["shift", 0]
    if spec.attention:
        g["attn"] = {"class_type": "ModelAttentionBackend", "inputs": {"model": model_ref, "attention": spec.attention}}
        model_ref = ["attn", 0]

    if is_ref:
        cond_inputs = {"clip": ["clip", 0], "vae": ["vae_v", 0], "audio_vae": ["vae_a", 0],
                       "prompt": spec.prompt, "width": spec.width, "height": spec.height,
                       "length": spec.length, "ref_image_size": spec.ref_image_size}
        for i, name in enumerate(spec.ref_images):
            nid = f"ref_img_{i}"
            g[nid] = {"class_type": "LoadImage", "inputs": {"image": name}}
            cond_inputs[f"ref_images.ref_image_{i}"] = [nid, 0]
        for i, name in enumerate(spec.ref_audios):
            nid = f"ref_aud_{i}"
            g[nid] = {"class_type": "LoadAudio", "inputs": {"audio": name}}
            cond_inputs[f"ref_audios.ref_audio_{i}"] = [nid, 0]
        g["cond"] = {"class_type": "MiniMaxH3ReferenceToVideo", "inputs": cond_inputs}
    else:
        cond_inputs = {"clip": ["clip", 0], "vae": ["vae_v", 0], "prompt": spec.prompt,
                       "width": spec.width, "height": spec.height, "length": spec.length}
        if spec.first_frame:
            g["first"] = {"class_type": "LoadImage", "inputs": {"image": spec.first_frame}}
            cond_inputs["first_frame"] = ["first", 0]
        if spec.last_frame:
            g["last"] = {"class_type": "LoadImage", "inputs": {"image": spec.last_frame}}
            cond_inputs["last_frame"] = ["last", 0]
        g["cond"] = {"class_type": "MiniMaxH3ImageToVideo", "inputs": cond_inputs}

    g["guider"] = {"class_type": "BasicGuider", "inputs": {"model": model_ref, "conditioning": ["cond", 0]}}
    g["sampler"] = {"class_type": "KSamplerSelect", "inputs": {"sampler_name": spec.sampler}}
    g["sigmas"] = {"class_type": "BasicScheduler",
                   "inputs": {"model": model_ref, "scheduler": spec.scheduler, "steps": spec.steps, "denoise": 1.0}}
    g["noise"] = {"class_type": "RandomNoise", "inputs": {"noise_seed": spec.seed}}
    g["sample"] = {"class_type": "SamplerCustomAdvanced",
                   "inputs": {"noise": ["noise", 0], "guider": ["guider", 0], "sampler": ["sampler", 0],
                              "sigmas": ["sigmas", 0], "latent_image": ["cond", 1]}}
    g["dec_v"] = {"class_type": "VAEDecode", "inputs": {"samples": ["sample", 0], "vae": ["vae_v", 0]}}
    g["dec_a"] = {"class_type": "VAEDecodeAudio", "inputs": {"samples": ["sample", 0], "vae": ["vae_a", 0]}}
    g["video"] = {"class_type": "CreateVideo", "inputs": {"images": ["dec_v", 0], "fps": FPS, "audio": ["dec_a", 0]}}
    g["save"] = {"class_type": "SaveVideo",
                 "inputs": {"video": ["video", 0], "filename_prefix": spec.filename_prefix,
                            "format": "mp4", "codec": "h264"}}
    return g
