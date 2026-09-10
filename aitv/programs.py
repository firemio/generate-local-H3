"""Programme formats for the station. Each format tells the LLM what to write; the LLM returns
H3-formatted prompts (integrated_multimodal_description / overall_soundscape / non_diegetic_music)
with Japanese dialogue in <d>[Japanese] ...</d> tags, one prompt per segment."""
from __future__ import annotations

import datetime as _dt
import random

STATION = {
    "name": "EVO-X2 TV",
    "tagline": "ローカルAIが24時間つくり続ける放送局",
    "lang": "Japanese",
}

H3_FORMAT_RULES = """You write prompts for MiniMax H3, a video+audio generation model. Follow this exact structure for every prompt:

integrated_multimodal_description: [Shot 1] <style words>, <shot size> frames <subject and setting>. <camera motion written as 'The camera pushes in with small amplitude at slow speed'>. <who speaks (Sx) and what they say>. [Shot 2] At 00:0X.000, the camera cuts to ... (optional, timestamps must be inside the clip length)
overall_soundscape: 1-3 sentences of ambient and action sounds only (no dialogue, no music).
non_diegetic_music: 1-2 sentences describing background score in instrumental terms, or N/A.

Dialogue rules: EVERY <d> tag MUST start with the language tag exactly like <d>[Japanese] ...</d> (never omit it). On-screen speech is written as `The <person description> with a <voice description> (S1) says: <d>[Japanese] 日本語のセリフ。</d>`. Narration is `... says in an off-screen voiceover: <d>[Japanese] ...</d>` and note that lips stay closed. Keep speech short: at most ~4 seconds of Japanese speech per 6-second segment (about 25-35 Japanese characters). Never put text, captions, logos or subtitles on screen. No real people, brands or copyrighted characters.
Camera vocabulary: Zoom In/Out, Push In/Out, Pan Left/Right, Truck Left/Right, Tilt Up/Down, Arc Shot, Tracking Shot, Static Shot, with small/large amplitude, at slow/fast speed."""

FORMATS = {
    "news": {
        "title": "AIニュース",
        "segments": 3,
        "seconds": 6,
        "brief": "A Japanese TV news bulletin. Segment 1: a calm anchor at a modern news desk greets viewers and reads the top headline. Segments 2-3: one fictional, harmless, upbeat news item each (technology, science, local events, weather), delivered by the same anchor or as a voiceover over a related scene. Invent plausible but clearly fictional news; no real politicians, companies or disasters.",
    },
    "weather": {
        "title": "お天気",
        "segments": 2,
        "seconds": 6,
        "brief": "A cheerful Japanese weather forecast. Segment 1: a presenter in front of a large weather map speaks about tomorrow. Segment 2: an outdoor scene matching the forecast with an off-screen voiceover.",
    },
    "nature": {
        "title": "自然ドキュメンタリー",
        "segments": 3,
        "seconds": 8,
        "brief": "A nature documentary with a warm, slow Japanese narration (off-screen voiceover). Pick one ecosystem (deep sea, rainforest canopy, arctic tundra, desert night, mountain river...) and show three shots of animals and landscape with rich ambient sound.",
    },
    "cm": {
        "title": "CM",
        "segments": 1,
        "seconds": 6,
        "brief": "A 6-second TV commercial for a fictional, harmless product (a drink, a gadget, a travel destination, a snack). Playful and visual, with a short Japanese slogan spoken by a narrator or a character. No on-screen text.",
    },
    "cooking": {
        "title": "3分クッキング",
        "segments": 2,
        "seconds": 6,
        "brief": "A cooking show. Segment 1: a friendly chef in a bright kitchen introduces a simple dish in Japanese. Segment 2: close-up of the key cooking action with sizzling sounds and the chef's short voiceover tip.",
    },
    "music": {
        "title": "ミュージックブレイク",
        "segments": 1,
        "seconds": 8,
        "brief": "An atmospheric music interlude: one beautiful cinematic scene (city at dusk, ocean, mountain, night train) with a specific instrumental style described in non_diegetic_music. No dialogue at all.",
    },
    "ident": {
        "title": "ステーションID",
        "segments": 1,
        "seconds": 4,
        "brief": "A 4-second station ident for the channel: abstract, colourful motion graphics or a whimsical mascot creature, with a short spoken channel jingle in Japanese by a bright voice, e.g. the station name and 'ローカルAI放送局'. No on-screen text.",
    },
}

# a broadcast day: weights for random schedule
SCHEDULE_WEIGHTS = {"news": 3, "weather": 1, "nature": 2, "cm": 3, "cooking": 1, "music": 2, "ident": 2}


def pick_format(rng: random.Random | None = None, exclude: str | None = None) -> str:
    rng = rng or random
    keys = [k for k in SCHEDULE_WEIGHTS if k != exclude]
    return rng.choices(keys, weights=[SCHEDULE_WEIGHTS[k] for k in keys])[0]


def writer_prompt(fmt_key: str, now: _dt.datetime | None = None, seed_topic: str | None = None) -> tuple[str, str]:
    fmt = FORMATS[fmt_key]
    now = now or _dt.datetime.now()
    system = (f"You are the head writer of '{STATION['name']}', {STATION['tagline']}. "
              + H3_FORMAT_RULES
              + "\nReturn ONLY JSON: {\"title\": str, \"segments\": [{\"caption\": str (Japanese, one line for the ticker), "
                "\"seconds\": int, \"prompt\": str (the full H3 prompt, English descriptions, Japanese dialogue)}]}")
    user = (f"Programme format: {fmt['title']} ({fmt_key}). Date/time: {now:%Y-%m-%d %H:%M} (mention only if natural). "
            f"Write exactly {fmt['segments']} segment(s), each {fmt['seconds']} seconds. Brief: {fmt['brief']}"
            + (f" Topic hint: {seed_topic}." if seed_topic else "")
            + " Keep every segment self-contained (a new clip starts each segment; the same character must be re-described each time).")
    return system, user


def fallback_programme(fmt_key: str) -> dict:
    """Used when the LLM is unavailable: one hard-coded segment per format."""
    fmt = FORMATS[fmt_key]
    base = {
        "news": ("Live-action, broadcast look, a medium shot frames a composed Japanese woman anchor in a navy jacket at a sleek glass news desk, soft studio lights, a blurred blue cityscape wall behind. The camera pushes in with small amplitude at slow speed. The anchor with a clear, warm voice (S1) says: <d>[Japanese] こんばんは、AIニュースの時間です。</d>",
                 "A quiet studio hum, a faint page turn, and the soft click of a desk microphone.",
                 "A restrained electronic news sting with soft synth pads, fading out quickly."),
        "weather": ("Live-action, bright studio, a medium shot of a smiling young presenter in a yellow cardigan standing beside a large stylised weather map of an island nation. The camera stays static. The presenter with a lively voice (S1) says: <d>[Japanese] 明日は全国的に晴れるでしょう。</d>",
                    "Soft studio ambience and the presenter's light footsteps.",
                    "A gentle ukulele loop at a relaxed tempo."),
        "nature": ("Live-action, wildlife documentary, a wide shot of a misty rainforest canopy at dawn, sunbeams through fog, a toucan landing on a branch. The camera tilts up with small amplitude at slow speed. A narrator with a deep, calm voice says in an off-screen voiceover, lips never shown: <d>[Japanese] 夜明けの森が、静かに目を覚まします。</d>",
                   "Distant birdsong, dripping water from leaves, insects, a soft breeze through the canopy.",
                   "Sparse piano notes with a warm string pad, slow tempo."),
        "cm": ("Live-action, glossy commercial look, a close-up of a frosted glass bottle of sparkling citrus soda on a sunlit wooden table, condensation running, slices of lemon. The camera arcs around with small amplitude at slow speed. A narrator with a bright, playful voice says in an off-screen voiceover: <d>[Japanese] シュワッと、はじける夏。</d>",
               "Fizzing bubbles, a bottle cap pop, ice clinking in a glass.",
               "An upbeat ukulele and handclap jingle at a fast tempo."),
        "cooking": ("Live-action, bright home kitchen, a medium shot of a cheerful chef in a white apron holding a pan over the stove, vegetables sizzling. The camera pushes in with small amplitude at slow speed. The chef with a friendly voice (S1) says: <d>[Japanese] 今日は簡単な野菜炒めを作ります。</d>",
                    "Oil sizzling in a hot pan, a wooden spatula scraping, a kitchen fan hum.",
                    "A light bossa nova guitar at a moderate tempo."),
        "music": ("Live-action, cinematic, a wide shot of a neon-lit city street in the rain at dusk, umbrellas, reflections on wet asphalt, a tram passing. The camera trucks right with small amplitude at slow speed. No dialogue.",
                  "Rain on pavement, a tram bell, distant traffic, footsteps under umbrellas.",
                  "A mellow lo-fi hip hop beat with a warm electric piano, moderate tempo."),
        "ident": ("3D animation, glossy and colourful, a round fluffy blue mascot creature with big eyes hops onto a floating cloud and waves at the camera against a pastel gradient background. The camera zooms in with small amplitude at fast speed. The mascot with a bright, squeaky voice (S1) says: <d>[Japanese] イーボエックスツーテレビ！</d>",
                  "A soft boing as the mascot lands, a whoosh of the cloud.",
                  "A three-note marimba jingle."),
    }
    d, s, m = base[fmt_key]
    prompt = f"integrated_multimodal_description: [Shot 1] {d}\n\noverall_soundscape: {s}\n\nnon_diegetic_music: {m}"
    return {"title": fmt["title"], "segments": [{"caption": fmt["title"], "seconds": fmt["seconds"], "prompt": prompt}]}
