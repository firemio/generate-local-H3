"""aitv: a fully local, always-on AI TV station.

  local LLM (Ollama / LM Studio)  -> writes the programme (H3-formatted prompts, Japanese dialogue)
  MiniMax H3 via ComfyUI (h3gen)  -> renders each segment as video + native speech/sound
  aitv.server                     -> web player with a rolling playlist (+ optional HLS via ffmpeg)
"""

__version__ = "0.1.0"
