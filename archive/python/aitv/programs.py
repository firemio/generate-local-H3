"""Programme formats and the writer prompts.

A programme is written in two passes by the local LLM: an outline (title, regular cast, sections with a
time budget) and then, section by section, the H3 prompts for each 6-10 second clip
(integrated_multimodal_description / overall_soundscape / non_diegetic_music, Japanese dialogue in
<d>[Japanese] ...</d> tags). The cast descriptions from the outline are repeated verbatim in every
clip so the same host looks and sounds the same across the whole programme."""
from __future__ import annotations

import datetime as _dt

STATION = {
    "name": "EVO-X2 TV",
    "tagline": "ローカルAIが24時間つくり続ける放送局",
    "lang": "Japanese",
}

H3_FORMAT_RULES = """You write prompts for MiniMax H3, a video+audio generation model. Follow this exact structure for every prompt:

integrated_multimodal_description: [Shot 1] <style words>, <shot size> frames <subject and setting>. <camera motion written as 'The camera pushes in with small amplitude at slow speed'>. <who speaks (Sx) and what they say>. [Shot 2] At 00:0X.000, the camera cuts to ... (optional, timestamps must be inside the clip length)
overall_soundscape: 1-3 sentences of ambient and action sounds only (no dialogue, no music).
non_diegetic_music: 1-2 sentences describing background score in instrumental terms, or N/A.

Dialogue rules: EVERY <d> tag MUST start with the language tag exactly like <d>[Japanese] ...</d> (never omit it). On-screen speech is written as `The <person description> with a <voice description> (S1) says: <d>[Japanese] 日本語のセリフ。</d>`. Narration is `... says in an off-screen voiceover: <d>[Japanese] ...</d>` and note that lips stay closed. Speech takes about 6 Japanese characters per second: a 10-second clip holds at most ~50 characters, an 8-second clip ~40. Never put text, captions, logos or subtitles on screen. No real people, brands or copyrighted characters.
Camera vocabulary: Zoom In/Out, Push In/Out, Pan Left/Right, Truck Left/Right, Tilt Up/Down, Arc Shot, Tracking Shot, Static Shot, with small/large amplitude, at slow/fast speed."""

FORMATS = {
    "news": {
        "title": "ニュース",
        "minutes": 6,
        "needs_material": True,
        "min_gap_hours": 5,   # two bulletins closer than this would read the same headlines
        "brief": "A Japanese TV news programme built ONLY from the real headlines provided. One anchor at a modern news desk carries the whole programme; sections follow the material's categories (top stories, society, economy, world, sports, science & culture) and end with a short sign-off. The viewer must always know WHEN this is: the title starts with the date and the edition (pattern: '10月3日(土) 昼のニュース'), the anchor's very first sentence states the date, the day of the week and the edition exactly as given in the plan line above (pattern: '10月3日、土曜日、昼のニュースです' — use the real date and weekday, never the pattern's), stories are placed in time ('きょう午前', 'きのう'), and the sign-off repeats the date. Each story: the anchor reads it in one or two sentences, or a voiceover reads it over a generic symbolic scene that fits the subject (a city skyline, a stadium, a laboratory, a harbour, a field, a trading floor...). Condense; never add facts, names or numbers that are not in the material. Never show real people, real logos, flags or recognisable landmarks. The material comes from a news agency but this channel is not that agency: never put its name in the title or the speech.",
    },
    "weather": {
        "title": "お天気",
        "minutes": 4,
        "needs_material": True,
        "brief": "A friendly Japanese weather programme built ONLY from the real JMA forecast provided: today, tonight, tomorrow, the week ahead, temperatures and rain chance for the named area, plus practical advice (umbrella, heat, clothing). The title starts with the date (pattern: '10月3日(土) お天気'), and the presenter opens by saying the date, the day of the week and the time of day exactly as given in the plan line above (use the real date and weekday), then names each day explicitly ('きょう14日', 'あす15日'). One presenter in front of a large stylised weather map, cut with outdoor scenes that match the forecast under a voiceover. State only what the material says.",
    },
    "variety": {
        "title": "バラエティー",
        "minutes": 6,
        "needs_material": True,
        "brief": "A Japanese TV variety show in a bright, colourful studio with two regular hosts (one lively, one deadpan; fictional people) and one guest. Corners: an opening chat about today's real light topics from the material (sports, science, culture — refer to them by subject, never by a real person's name), a quiz corner with questions built from those topics, a small physical challenge or game with big reactions, and a warm sign-off. Studio laughter and applause; silly but never mean.",
    },
    "music_show": {
        "title": "ミュージックステージ",
        "minutes": 6,
        "needs_material": False,
        "brief": "A live music show on a neon-lit stage with one stylish host and three fictional acts (invented Japanese or English names, never a real artist), each in a different genre (city pop, jazz trio, idol pop, rock, acoustic folk, electronic...). For each act: the host's short introduction, then several performance shots (wide shot of the band, close-up of the singer, an instrument, the crowd). The music is the performance itself: describe it precisely in non_diegetic_music (instruments, tempo, mood) and keep it consistent across the shots of one act; one or two short sung Japanese lines per act inside <d>[Japanese] ...</d>, otherwise no speech during performances. Applause and crowd in the soundscape.",
    },
    "nature": {
        "title": "自然ドキュメンタリー",
        "minutes": 6,
        "needs_material": False,
        "brief": "A nature documentary with a warm, slow Japanese narration (off-screen voiceover, one narrator voice for the whole programme). Pick one ecosystem (deep sea, rainforest canopy, arctic tundra, desert night, mountain river, mangrove, alpine meadow...) and tell it as a day or a season: sections for dawn, the hunt, the young, weather arriving, dusk... Rich ambient sound, sparse music. No people.",
    },
    "cooking": {
        "title": "3分クッキング",
        "minutes": 5,
        "needs_material": False,
        "brief": "A cooking show in a bright kitchen with one cheerful chef who carries the whole programme. Pick one simple dish and go step by step: ingredients, prep, the key cooking action with sizzling close-ups, plating, tasting, and a closing tip. The chef talks to the viewer in short friendly Japanese sentences.",
    },
    "travel": {
        "title": "ぶらり旅",
        "minutes": 6,
        "needs_material": False,
        "brief": "A relaxed travel programme: one reporter (fictional) wanders through an invented but believable place of a chosen type (a harbour town, a mountain village, a hot-spring town, an old merchant street, an island...). Sections: arrival, a walk, a local craft or shop, food, a viewpoint, and a farewell. The reporter speaks in short warm Japanese sentences, sometimes as a voiceover over scenery. Never a real named place or landmark; no on-screen text.",
    },
    "cm": {
        "title": "CM",
        "minutes": 0.1,
        "needs_material": False,
        "brief": "A 6-second TV commercial for a fictional, harmless product (a drink, a gadget, a travel destination, a snack). Playful and visual, with a short Japanese slogan spoken by a narrator or a character. No on-screen text.",
    },
    "ident": {
        "title": "ステーションID",
        "minutes": 0.07,
        "needs_material": False,
        "brief": "A 4-second station ident for the channel: abstract, colourful motion graphics or a whimsical mascot creature, with a short spoken channel jingle in Japanese by a bright voice, e.g. the station name and 'ローカルAI放送局'. No on-screen text.",
    },
}

# one broadcast day: eight programmes, produced in this order and aired in this order, every day anew
LINEUP = ["news", "weather", "variety", "music_show", "nature", "cooking", "travel", "news"]
BREAK_FORMATS = ("cm", "ident")

SEGMENT_MIN_S, SEGMENT_MAX_S = 6, 10

# subjects rotated day by day so a format does not settle on its favourite (the least recently used
# ones are suggested; the writer may still choose otherwise)
THEMES = {
    "nature": ["深海", "熱帯雨林の樹冠", "北極のツンドラ", "砂漠の夜", "山の渓流", "マングローブ", "高山の草原", "サンゴ礁",
               "サバンナ", "竹林", "湿原", "火山島", "氷河", "干潟", "夜の森", "海氷の縁"],
    "cooking": ["和食の煮物", "麺料理", "丼もの", "パンとサンドイッチ", "スープ", "サラダ", "スイーツ", "イタリアン", "中華",
                "エスニック", "朝ごはん", "お弁当", "鍋料理", "揚げ物", "焼き菓子", "魚料理"],
    "travel": ["港町", "山あいの村", "温泉街", "古い商人の町並み", "離島", "湖畔の町", "雪国の集落", "棚田の里", "運河の町",
               "高原の町", "城下町", "漁村", "茶畑の丘", "夜の屋台街", "灯台のある岬"],
    "music_show": ["シティポップ", "ジャズトリオ", "アイドルポップ", "ロック", "アコースティックフォーク", "エレクトロニカ", "R&B",
                   "ボサノバ", "ヒップホップ", "ブラスバンド", "エレクトロスウィング", "演歌風バラード", "ファンク", "アンビエント",
                   "パンク", "ピアノソロ"],
    "variety": ["早押しクイズ", "ジェスチャーゲーム", "利き味対決", "なぞなぞ", "○×クイズ", "記憶力ゲーム", "お絵かき当てゲーム",
                "早口言葉", "バランスゲーム", "百人一首風かるた", "しりとり", "モノマネ"],
}
THEME_COUNT = {"music_show": 3}   # how many themes to suggest at once (three acts)
# how a theme shows up in a written script (titles and section plans mix Japanese and English)
THEME_KEYS = {
    "シティポップ": ("シティポップ", "シティ・ポップ", "city pop", "city-pop"), "ジャズトリオ": ("ジャズ", "jazz"),
    "アイドルポップ": ("アイドル", "idol"), "ロック": ("ロック", "rock"), "アコースティックフォーク": ("フォーク", "folk", "アコースティック", "acoustic"),
    "エレクトロニカ": ("エレクトロニカ", "electronic", "electronica", "synth"), "R&B": ("r&b", "rnb", "ソウル", "soul"),
    "ボサノバ": ("ボサノバ", "bossa"), "ヒップホップ": ("ヒップホップ", "hip hop", "hip-hop", "rap"), "ブラスバンド": ("ブラス", "brass"),
    "エレクトロスウィング": ("スウィング", "swing"), "演歌風バラード": ("演歌", "enka", "バラード", "ballad"), "ファンク": ("ファンク", "funk"),
    "アンビエント": ("アンビエント", "ambient"), "パンク": ("パンク", "punk"), "ピアノソロ": ("ピアノ", "piano"),
    "深海": ("深海", "deep sea", "deep-sea"), "熱帯雨林の樹冠": ("熱帯雨林", "rainforest", "ジャングル"), "北極のツンドラ": ("北極", "ツンドラ", "arctic", "tundra"),
    "砂漠の夜": ("砂漠", "desert"), "山の渓流": ("渓流", "mountain river", "清流"), "マングローブ": ("マングローブ", "mangrove"),
    "高山の草原": ("高山", "alpine"), "サンゴ礁": ("サンゴ", "coral"), "サバンナ": ("サバンナ", "savanna"), "竹林": ("竹林", "bamboo"),
    "湿原": ("湿原", "wetland", "marsh"), "火山島": ("火山", "volcan"), "氷河": ("氷河", "glacier"), "干潟": ("干潟", "tidal flat", "mudflat"),
    "夜の森": ("夜の森", "night forest"), "海氷の縁": ("海氷", "sea ice"),
    "港町": ("港町", "harbour", "harbor", "港"), "山あいの村": ("山あい", "山里", "mountain village"), "温泉街": ("温泉", "hot spring", "onsen"),
    "古い商人の町並み": ("商人", "merchant", "町並み"), "離島": ("離島", "island", "島"), "湖畔の町": ("湖", "lake"), "雪国の集落": ("雪国", "snow"),
    "棚田の里": ("棚田", "rice terrace"), "運河の町": ("運河", "canal"), "高原の町": ("高原", "highland", "plateau"), "城下町": ("城下町", "castle town"),
    "漁村": ("漁村", "漁港", "fishing"), "茶畑の丘": ("茶畑", "tea"), "夜の屋台街": ("屋台", "street food", "stall"), "灯台のある岬": ("灯台", "lighthouse", "岬", "cape"),
}
WEEKDAYS_JA = "月火水木金土日"


def edition(now: _dt.datetime) -> str:
    """Which edition of the day a programme written now belongs to (said on air: 朝のニュース...)."""
    h = now.hour
    return "朝" if h < 11 else "昼" if h < 15 else "夕方" if h < 19 else "夜"


def _system() -> str:
    return (f"You are the head writer of '{STATION['name']}', {STATION['tagline']}. " + H3_FORMAT_RULES
            + "\nAll titles, captions, names and dialogue are Japanese. Return ONLY JSON.")


def outline_prompt(fmt_key: str, minutes: float, material: str | None, now: _dt.datetime | None = None,
                   regulars: list[dict] | None = None, recent: list[dict] | None = None, themes: list[str] | None = None,
                   reported: list[str] | None = None) -> tuple[str, str]:
    """The outline request. `regulars` are the fixed presenters carried over from the last episode, `recent`
    the last episodes (title, theme, summary, next_time), `themes` today's rotation suggestion, `reported`
    headlines already read in recent news programmes."""
    fmt = FORMATS[fmt_key]
    now = now or _dt.datetime.now()
    total = int(minutes * 60)
    parts = [f"Plan a {minutes:g}-minute {fmt['title']} programme ({fmt_key}). Now: {date_line(now)}. "
             "Mention the date/time only if natural — except where the concept requires it.",
             f"Concept: {fmt['brief']}"]
    if regulars:
        parts.append("Regular cast — this programme's fixed people. Keep their names, looks and voices EXACTLY as written; add today's guests or acts as new cast entries with \"regular\": false:\n"
                     + "\n".join(f"- {c.get('role')}: {c.get('name')} — {c.get('look')} Voice: {c.get('voice')}" for c in regulars))
    if recent:
        parts.append("Recent episodes of this programme (do NOT repeat their subjects or titles; if the last one left a thread or a teaser, pick it up naturally):\n"
                     + "\n".join(f"- {e.get('written', '')[:10]} 『{e.get('title')}』 theme: {e.get('theme') or '-'} — {e.get('summary') or ''}"
                                 + (f" / next time: {e['next_time']}" if e.get("next_time") else "") for e in recent))
    if themes:
        parts.append("Today's theme — the episode MUST be built on this (it has not been used recently; the recent episodes' "
                     "subjects are off-limits): " + " / ".join(themes)
                     + (". These are the genres of the three acts, one each." if fmt_key == "music_show" else ""))
    if reported:
        parts.append("Already reported in recent programmes — skip these unless the material shows a new development, and lead with what is new:\n"
                     + "\n".join(f"- {t}" for t in reported))
    if material:
        parts.append(f"Material:\n{material}")
    parts.append(
        "Return JSON: {\"title\": str (Japanese, distinctive for today), \"theme\": str (Japanese, the subject of today's episode in a few words), "
        "\"cast\": [{\"role\": str, \"name\": str (Japanese), \"look\": str (English, one precise sentence: age, hair, clothes, setting), "
        "\"voice\": str (English, a few words), \"regular\": bool (true for the fixed presenter/narrator/chef/reporter, false for today's guests or acts)}], "
        "\"sections\": [{\"name\": str (Japanese), \"seconds\": int, \"plan\": str (English, 2-3 sentences: what happens, which material items are used, how it ends)}], "
        "\"summary\": str (Japanese, one line: what this episode was about), \"next_time\": str (Japanese, one line said at the end as a teaser for the next episode, or empty)}\n"
        f"4 to 7 sections including an opening and a closing; the seconds must add up to about {total}. "
        "Cast descriptions will be pasted verbatim into every shot, so make them specific enough to keep the same person recognisable.")
    return _system(), "\n\n".join(parts)


def date_line(now: _dt.datetime | None = None) -> str:
    now = now or _dt.datetime.now()
    return f"{now:%Y-%m-%d %H:%M} = {now.month}月{now.day}日 {WEEKDAYS_JA[now.weekday()]}曜日 {edition(now)}"


def section_prompt(fmt_key: str, outline: dict, section: dict, prev_caption: str | None, material: str | None,
                   now: _dt.datetime | None = None) -> tuple[str, str]:
    fmt = FORMATS[fmt_key]
    secs = int(section.get("seconds") or 60)
    n = max(1, round(secs / 8))
    cast = "\n".join(f"- {c.get('role')}: {c.get('name')} — {c.get('look')} Voice: {c.get('voice')}" for c in outline.get("cast", []))
    is_last = bool(outline.get("sections")) and section.get("name") == outline["sections"][-1].get("name")
    user = (f"Programme: {outline.get('title')} ({fmt['title']}, {fmt_key}). Concept: {fmt['brief']}\n"
            f"Now: {date_line(now)} — whenever the date or weekday is spoken, it is this one.\n"
            + (f"Today's theme: {outline['theme']}\n" if outline.get("theme") else "")
            + f"Cast (repeat the look and voice verbatim whenever they appear; give each speaker a stable Sx number):\n{cast}\n"
            f"Sections in order: " + " / ".join(s.get("name", "") for s in outline.get("sections", [])) + "\n"
            + (f"Material:\n{material}\n" if material else "")
            + f"\nWrite section '{section.get('name')}' ({secs} s): {section.get('plan')}\n"
            + (f"The previous clip ended with: {prev_caption}\n" if prev_caption else "This is the first clip of the programme.\n")
            + (f"This is the closing section: its last clip ends with this teaser, spoken by a regular: {outline['next_time']}\n"
               if is_last and outline.get("next_time") else "")
            + f"Return JSON: {{\"segments\": [{{\"caption\": str (Japanese, one short ticker line), \"seconds\": int ({SEGMENT_MIN_S}-{SEGMENT_MAX_S}), "
              f"\"prompt\": str (the full H3 prompt)}}]}} with about {n} segments whose seconds add up to about {secs}. "
              "Every clip is generated on its own: re-describe the setting and the cast in each prompt and keep continuity of action. "
              "Speech: ONE spoken sentence per clip, at most 45 Japanese characters (a 10-second clip cannot hold more); "
              "if a story needs more words, spread it over more clips instead of writing longer lines.")
    return _system(), user


def break_prompt(fmt_key: str, recent_titles: list[str] | None = None) -> tuple[str, str]:
    fmt = FORMATS[fmt_key]
    seconds = 4 if fmt_key == "ident" else 6
    user = (f"Write one {seconds}-second {fmt['title']} ({fmt_key}). Concept: {fmt['brief']}\n"
            + (f"Recent ones (make something clearly different): " + " / ".join(recent_titles) + "\n" if recent_titles else "")
            + f"Return JSON: {{\"title\": str (Japanese), \"segments\": [{{\"caption\": str (Japanese), \"seconds\": {seconds}, \"prompt\": str (the full H3 prompt)}}]}} with exactly one segment.")
    return _system(), user
