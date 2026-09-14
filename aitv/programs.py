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
        "title": "ニュース",
        "segments": 3,
        "seconds": 6,
        "needs_material": True,
        "brief": "A Japanese TV news bulletin built ONLY from the real headlines provided below. Segment 1: a calm anchor at a modern news desk greets viewers and reads the top headline. Segments 2-3: one more headline each, read by the same anchor or as a voiceover over a generic, symbolic scene that fits the subject (a city skyline, a stadium, a laboratory, a harbour, a field...). Condense each headline to one spoken sentence of at most ~30 Japanese characters; the caption is the headline itself. Title: a short Japanese programme name with the date (e.g. 'ニュース 9月14日'); the material comes from a news agency but this channel is not that agency, so never put its name in the title or the speech. Never show real people, real logos, flags or recognisable landmarks; never add facts that are not in the headline.",
    },
    "weather": {
        "title": "お天気",
        "segments": 2,
        "seconds": 6,
        "needs_material": True,
        "brief": "A cheerful Japanese weather forecast based ONLY on the real JMA forecast provided below. Segment 1: a presenter in front of a large stylised weather map says what today and tomorrow will be like in the named area (weather, temperature, rain chance). Segment 2: an outdoor scene matching tomorrow's forecast with an off-screen voiceover giving one practical tip (umbrella, sunscreen, a light jacket...).",
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
    "music_show": {
        "title": "ミュージックステージ",
        "segments": 3,
        "seconds": 8,
        "brief": "A live music show. Invent a fictional artist or band (a made-up Japanese or English name; never a real artist) and a song title. Segment 1: a stylish host on a neon-lit stage introduces the artist and song in Japanese. Segments 2-3: two performance shots (wide shot of the band, close-up of the singer or an instrument) in the genre you chose (city pop, jazz trio, idol pop, rock, acoustic folk, electronic...). The music is the performance itself: describe it precisely in non_diegetic_music (instruments, tempo, mood); a short sung Japanese line inside <d>[Japanese] ...</d> is allowed in one performance shot, otherwise no speech. Stage lights, audience silhouettes and applause in the soundscape.",
    },
    "variety": {
        "title": "バラエティー",
        "segments": 3,
        "seconds": 6,
        "needs_material": True,
        "brief": "A Japanese TV variety show in a bright, colourful studio with two hosts (one lively, one deadpan; fictional people). Segment 1: the hosts open the show and announce today's topic, taken from the real light news items provided below (sports, science, culture) — refer to it by subject, never by a real person's name. Segment 2: a quiz question or a small challenge about that topic, with a big reaction. Segment 3: the answer or punchline, studio laughter and applause, and a one-line sign-off. Keep it warm and silly, not mean.",
    },
    "ident": {
        "title": "ステーションID",
        "segments": 1,
        "seconds": 4,
        "brief": "A 4-second station ident for the channel: abstract, colourful motion graphics or a whimsical mascot creature, with a short spoken channel jingle in Japanese by a bright voice, e.g. the station name and 'ローカルAI放送局'. No on-screen text.",
    },
}

# a broadcast day: weights for random schedule
SCHEDULE_WEIGHTS = {"news": 3, "weather": 1, "nature": 2, "cm": 2, "cooking": 1, "music": 1,
                    "music_show": 2, "variety": 2, "ident": 2}


def pick_format(rng: random.Random | None = None, exclude: str | set[str] | None = None) -> str:
    rng = rng or random
    excluded = {exclude} if isinstance(exclude, str) else set(exclude or ())
    keys = [k for k in SCHEDULE_WEIGHTS if k not in excluded]
    return rng.choices(keys, weights=[SCHEDULE_WEIGHTS[k] for k in keys])[0]


def writer_prompt(fmt_key: str, now: _dt.datetime | None = None, material: str | None = None) -> tuple[str, str]:
    fmt = FORMATS[fmt_key]
    now = now or _dt.datetime.now()
    system = (f"You are the head writer of '{STATION['name']}', {STATION['tagline']}. "
              + H3_FORMAT_RULES
              + "\nReturn ONLY JSON: {\"title\": str, \"segments\": [{\"caption\": str (Japanese, one line for the ticker), "
                "\"seconds\": int, \"prompt\": str (the full H3 prompt, English descriptions, Japanese dialogue)}]}")
    user = (f"Programme format: {fmt['title']} ({fmt_key}). Date/time: {now:%Y-%m-%d %H:%M} (mention only if natural). "
            f"Write exactly {fmt['segments']} segment(s), each {fmt['seconds']} seconds. Brief: {fmt['brief']}"
            + " Keep every segment self-contained (a new clip starts each segment; the same character must be re-described each time)."
            + (f"\n\nMaterial:\n{material}" if material else ""))
    return system, user


def _speech(text: str, limit: int = 34) -> str:
    """Trim a headline to what fits in a 6-second segment (H3 speaks ~30 Japanese characters in ~4 s)."""
    text = text.strip()
    return text if len(text) <= limit else text[:limit - 1] + "…"


def _prompt(d: str, s: str, m: str) -> str:
    return f"integrated_multimodal_description: [Shot 1] {d}\n\noverall_soundscape: {s}\n\nnon_diegetic_music: {m}"


ANCHOR = ("Live-action, broadcast look, a medium shot frames a composed Japanese woman anchor in a navy jacket at a sleek glass "
          "news desk, soft studio lights, a blurred blue cityscape wall behind. The camera pushes in with small amplitude at slow speed. "
          "The anchor with a clear, warm voice (S1) says: ")
STUDIO_SOUND = "A quiet studio hum, a faint page turn, and the soft click of a desk microphone."
NEWS_STING = "A restrained electronic news sting with soft synth pads, fading out quickly."


def template_programme(fmt_key: str, items: list[dict]) -> dict | None:
    """Real material read straight from a fixed template, for when the LLM is down. Only the formats whose
    material maps onto a single spoken line have one."""
    fmt = FORMATS[fmt_key]
    if fmt_key == "news" and items:
        segs = []
        for i, it in enumerate(items[:fmt["segments"]]):
            lead = "こんばんは、ニュースです。" if i == 0 else ""
            line = _speech(it["title"], 34 - len(lead))
            segs.append({"caption": it["title"], "seconds": fmt["seconds"],
                         "prompt": _prompt(ANCHOR + f"<d>[Japanese] {lead}{line}</d>", STUDIO_SOUND, NEWS_STING)})
        return {"title": "ニュース", "segments": segs}
    if fmt_key == "weather" and items:
        d = items[min(1, len(items) - 1)]
        weather = d["weather"].replace(" ", "")
        line = _speech(f"明日の{d['area']}は、{weather}でしょう。", 40)
        return {"title": "お天気", "segments": [{"caption": f"{d['area']}の天気 {weather}", "seconds": fmt["seconds"], "prompt": _prompt(
            "Live-action, bright studio, a medium shot of a smiling young presenter in a yellow cardigan standing beside a large stylised "
            "weather map of an island nation. The camera stays static. The presenter with a lively voice (S1) says: "
            f"<d>[Japanese] {line}</d>", "Soft studio ambience and the presenter's light footsteps.", "A gentle ukulele loop at a relaxed tempo.")}]}
    return None


def fallback_programme(fmt_key: str) -> dict:
    """Used when the LLM is unavailable and there is no material to template: one hard-coded segment per format."""
    fmt = FORMATS[fmt_key]
    base = {
        "news": (ANCHOR + "<d>[Japanese] こんばんは、ニュースの時間です。</d>", STUDIO_SOUND, NEWS_STING),
        "weather": ("Live-action, bright studio, a medium shot of a smiling young presenter in a yellow cardigan standing beside a large stylised weather map of an island nation. The camera stays static. The presenter with a lively voice (S1) says: <d>[Japanese] お天気の時間です。</d>",
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
        "music_show": ("Live-action, concert film look, a wide shot of a four-piece band on a small neon-lit club stage, a woman singer at a vintage microphone, guitarist, bassist and drummer behind her, haze and coloured spotlights, audience silhouettes in the foreground. The camera arcs around with small amplitude at slow speed. No speech.",
                       "A packed club: crowd murmur, whistles and applause under the music, a snare hit echoing off the walls.",
                       "Upbeat Japanese city pop: bright electric piano chords, a funky bass line, tight drums and a soaring synth lead at a moderate-fast tempo."),
        "variety": ("Live-action, TV variety look, a medium shot of two hosts at a glossy desk in a colourful studio with big pastel panels and balloons, one in a sparkly jacket grinning, one in a grey suit unimpressed. The camera pushes in with small amplitude at slow speed. The host in the sparkly jacket with an energetic voice (S1) says: <d>[Japanese] さあ、今日のクイズです！</d>",
                    "Studio laughter, a drum-roll sting, applause from a small audience.",
                    "A bouncy brass and xylophone jingle at a fast tempo."),
        "ident": ("3D animation, glossy and colourful, a round fluffy blue mascot creature with big eyes hops onto a floating cloud and waves at the camera against a pastel gradient background. The camera zooms in with small amplitude at fast speed. The mascot with a bright, squeaky voice (S1) says: <d>[Japanese] イーボエックスツーテレビ！</d>",
                  "A soft boing as the mascot lands, a whoosh of the cloud.",
                  "A three-note marimba jingle."),
    }
    d, s, m = base[fmt_key]
    return {"title": fmt["title"], "segments": [{"caption": fmt["title"], "seconds": fmt["seconds"], "prompt": _prompt(d, s, m)}]}
