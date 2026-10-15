package comfy

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// Golden values below were produced by the Python implementation (archive/python/h3gen/graph.py) and
// embedded here so the tests keep running without it.

// snap_frames(seconds) for each input (round() is half-to-even: 0.9375*24 = 22.5 -> 22, 5.1875*24 = 124.5 -> 124).
func TestSnapFrames(t *testing.T) {
	cases := []struct {
		seconds float64
		want    int
	}{
		{4, 107},
		{5, 124},
		{6, 158},
		{8, 192},
		{9.4, 226},
		{10, 243},
		{7.3, 175},
		{0, 5},
		{0.1, 5},
		{-1, 5},
		{0.9375, 22},  // 22.5 rounds to even 22, already on the grid (half-up would give 39)
		{5.1875, 124}, // 124.5 rounds to even 124 (half-up would give 141)
		{2.5, 73},
		{1.0, 39},
		{12.0, 294},
		{60, 1450},
	}
	for _, c := range cases {
		if got := SnapFrames(c.seconds); got != c.want {
			t.Errorf("SnapFrames(%v) = %d, want %d", c.seconds, got, c.want)
		}
	}
	// Every result sits on the 17k+5 grid, absurd inputs included.
	for _, s := range []float64{math.NaN(), math.Inf(-1), math.Inf(1), -1e300, 1e300, 4e17} {
		if n := SnapFrames(s); n < 5 || (n-5)%17 != 0 {
			t.Errorf("SnapFrames(%v) = %d is not 17k+5", s, n)
		}
	}
	for s := 0.0; s <= 30; s += 0.05 {
		if n := SnapFrames(s); n < 5 || (n-5)%17 != 0 {
			t.Errorf("SnapFrames(%v) = %d is not 17k+5", s, n)
		}
	}
	if FramesSeconds(124) != 124.0/24 || FramesSeconds(0) != 0 {
		t.Errorf("FramesSeconds: %v", FramesSeconds(124))
	}
}

// canvas(aspect, megapixels) for every preset, the tier boundaries and the fallback.
func TestCanvas(t *testing.T) {
	cases := []struct {
		aspect string
		mp     float64
		w, h   int
	}{
		{"16:9", 1.0, 1344, 768},
		{"16:9", 0.9, 1344, 768},
		{"9:16", 0.9, 768, 1344},
		{"9:16", 1.5, 768, 1344},
		{"1:1", 2.0, 1024, 1024},
		{"1:1", 0.9, 1024, 1024},
		{"16:9", 0.4, 832, 480},
		{"16:9", 0.3, 832, 480},
		{"16:9", 0.8999, 832, 480},
		{"9:16", 0.3, 480, 832},
		{"9:16", 0.4, 480, 832},
		{"1:1", 0.5, 640, 640},
		{"1:1", 0.3, 640, 640},
		{"16:9", 0.1, 640, 352},
		{"16:9", 0.2999, 640, 352},
		{"16:9", 0.0, 640, 352},
		// fallback: no small preset for 9:16 / 1:1, unknown aspects
		{"9:16", 0.1, 832, 480},
		{"1:1", 0.2, 832, 480},
		{"4:3", 1.0, 832, 480},
		{"4:3", 0.4, 832, 480},
		{"", 0.4, 832, 480},
		{"16x9", 0.4, 832, 480},
	}
	for _, c := range cases {
		w, h := Canvas(c.aspect, c.mp)
		if w != c.w || h != c.h {
			t.Errorf("Canvas(%q, %v) = %dx%d, want %dx%d", c.aspect, c.mp, w, h, c.w, c.h)
		}
	}
}

func TestNewGenSpecDefaults(t *testing.T) {
	s := NewGenSpec("p")
	want := GenSpec{
		Prompt: "p", Mode: "t2v", Width: 832, Height: 480, Seconds: 5.0, Steps: 8, Turbo: true,
		LoraStrength: 1.0, Scheduler: "simple", Sampler: "res_multistep", RefImageSize: "match",
		FilenamePrefix: "h3/h3", Files: DefaultFiles,
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("NewGenSpec defaults differ:\n got %+v\nwant %+v", s, want)
	}
	if s.ShiftVideo != nil || s.ShiftAudio != nil || s.Attention != "" || s.FirstFrame != "" || s.LastFrame != "" ||
		len(s.RefImages) != 0 || len(s.RefAudios) != 0 {
		t.Errorf("optional fields must be unset: %+v", s)
	}
	if s.Length() != 124 {
		t.Errorf("Length() = %d, want 124", s.Length())
	}
	// Files is a copy: editing it must not touch DefaultFiles.
	s.Files["clip"] = "other.safetensors"
	if DefaultFiles["clip"] != "qwen3vl_32b_minimax_h3_int8_convrot.safetensors" {
		t.Fatalf("NewGenSpec shares DefaultFiles instead of copying it")
	}
}

func f64(v float64) *float64 { return &v }

// TestBuildParity compares Build(spec) with h3gen.graph.build(spec) for the same spec (Python golden dumped with
// json.dumps(sort_keys=True)). Both sides are decoded into map[string]any (every number becomes a float64, so
// Python's 1.0 and Go's 1 agree) and compared with reflect.DeepEqual.
func TestBuildParity(t *testing.T) {
	cases := []struct {
		name   string
		spec   func() GenSpec
		golden string
	}{
		{"t2v turbo 8 steps", func() GenSpec {
			s := NewGenSpec("A calm studio, a presenter says <d>こんにちは、ニュースの時間です。</d> & smiles \"warmly\"")
			s.Width, s.Height, s.Seconds, s.Seed = 832, 480, 5.0, 12345
			return s
		}, goldenT2VTurbo},
		{"t2v no-turbo shift attention", func() GenSpec {
			s := NewGenSpec("Night city, rain, neon 雨の夜")
			s.Mode = "t2v"
			s.Width, s.Height, s.Seconds, s.Seed, s.Steps = 1344, 768, 10, 7, 30
			s.Turbo = false
			s.ShiftVideo, s.ShiftAudio = f64(8.0), f64(2.5)
			s.Attention = "pytorch attention"
			s.Scheduler, s.Sampler = "beta", "euler"
			s.FilenamePrefix = "aitv/seg"
			return s
		}, goldenT2VNoTurboShiftAttn},
		{"i2v first+last frame 4 steps", func() GenSpec {
			s := NewGenSpec("Slow push-in on the mountain")
			s.Mode = "i2v"
			s.Width, s.Height, s.Seconds, s.Seed, s.Steps = 768, 1344, 9.4, 99, 4
			s.LoraStrength = 0.8
			s.FirstFrame, s.LastFrame = "h3gen/first.png", "h3gen/last.png"
			return s
		}, goldenI2VFirstLast},
		{"r2v two ref images one ref audio", func() GenSpec {
			s := NewGenSpec("The character waves and speaks")
			s.Mode = "r2v"
			s.Width, s.Height, s.Seconds, s.Seed, s.Steps = 1024, 1024, 7.3, 1<<31-1, 4
			s.LoraStrength = 0.75
			s.RefImages = []string{"h3gen/ref_a.png", "h3gen/ref_b.jpg"}
			s.RefAudios = []string{"h3gen/voice.wav"}
			s.RefImageSize = "max"
			s.FilenamePrefix = "h3/ref"
			return s
		}, goldenR2VRefs},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var want map[string]any
			if err := json.Unmarshal([]byte(c.golden), &want); err != nil {
				t.Fatalf("golden is not valid JSON: %v", err)
			}
			raw, err := json.Marshal(Build(c.spec()))
			if err != nil {
				t.Fatalf("json.Marshal(Build): %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("Build output is not valid JSON: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				// Re-marshal both (sorted keys, float64 numbers) for a readable diff.
				g, _ := json.MarshalIndent(got, "", " ")
				w, _ := json.MarshalIndent(want, "", " ")
				t.Errorf("Build differs from h3gen.graph.build\n--- got\n%s\n--- want\n%s", g, w)
			}
			// The comparison is not vacuous: a different seed must be detected.
			other := c.spec()
			other.Seed++
			raw, _ = json.Marshal(Build(other))
			var changed map[string]any
			_ = json.Unmarshal(raw, &changed)
			if reflect.DeepEqual(changed, want) {
				t.Errorf("golden comparison did not notice a changed seed")
			}
		})
	}
}

// Optional nodes appear only when the spec asks for them, and every link points at an existing node.
func TestBuildStructure(t *testing.T) {
	has := func(g Graph, ids ...string) []string {
		var missing []string
		for _, id := range ids {
			if !g.Has(id) {
				missing = append(missing, id)
			}
		}
		return missing
	}
	base := NewGenSpec("x")
	g := Build(base)
	if m := has(g, "unet", "clip", "vae_v", "vae_a", "lora", "cond", "guider", "sampler", "sigmas", "noise", "sample", "dec_v", "dec_a", "video", "save"); m != nil {
		t.Errorf("t2v turbo graph is missing %v", m)
	}
	if len(g) != 15 {
		t.Errorf("t2v turbo graph has %d nodes, want 15: %v", len(g), g.IDs())
	}
	for _, id := range []string{"shift", "attn", "first", "last", "ref_img_0", "ref_aud_0"} {
		if g.Has(id) {
			t.Errorf("t2v turbo graph must not contain %q", id)
		}
	}
	if g.Node("lora").Inputs.Get("lora_name") != DefaultFiles["lora_fl2v_8step"] {
		t.Errorf("8-step t2v picks %v", g.Node("lora").Inputs.Get("lora_name"))
	}

	// steps <= 4 selects the 4-step LoRA; steps 5 the 8-step one.
	s4 := base
	s4.Steps = 4
	if Build(s4).Node("lora").Inputs.Get("lora_name") != DefaultFiles["lora_fl2v_4step"] {
		t.Errorf("4-step t2v must use lora_fl2v_4step")
	}
	s5 := base
	s5.Steps = 5
	if Build(s5).Node("lora").Inputs.Get("lora_name") != DefaultFiles["lora_fl2v_8step"] {
		t.Errorf("5-step t2v must use lora_fl2v_8step")
	}
	// r2v always uses the ref2v LoRA and the ref2va UNET, whatever the step count.
	r := base
	r.Mode, r.Steps = "r2v", 8
	rg := Build(r)
	if rg.Node("lora").Inputs.Get("lora_name") != DefaultFiles["lora_ref2v_4step"] || rg.Node("unet").Inputs.Get("unet_name") != DefaultFiles["ref2va"] {
		t.Errorf("r2v must use ref2va + lora_ref2v_4step: %v / %v", rg.Node("unet").Inputs, rg.Node("lora").Inputs)
	}
	if rg.Node("cond").ClassType != "MiniMaxH3ReferenceToVideo" || rg.Node("cond").Inputs.Get("ref_image_size") != "match" {
		t.Errorf("r2v cond node: %+v", rg.Node("cond"))
	}

	// no turbo: no lora node, model links go straight to unet.
	nt := base
	nt.Turbo = false
	ng := Build(nt)
	if ng.Has("lora") {
		t.Errorf("no-turbo graph must not contain lora")
	}
	if !reflect.DeepEqual(ng.Node("guider").Inputs.Get("model"), []any{"unet", 0}) || !reflect.DeepEqual(ng.Node("sigmas").Inputs.Get("model"), []any{"unet", 0}) {
		t.Errorf("no-turbo model links: guider=%v sigmas=%v", ng.Node("guider").Inputs.Get("model"), ng.Node("sigmas").Inputs.Get("model"))
	}

	// shift with only one side set: the other takes the model default.
	sv := base
	sv.ShiftVideo = f64(9)
	if in := Build(sv).Node("shift").Inputs; in.Get("shift_video") != 9.0 || in.Get("shift_audio") != 3.0 {
		t.Errorf("shift_video only: %v", in)
	}
	sa := base
	sa.ShiftAudio = f64(1.5)
	if in := Build(sa).Node("shift").Inputs; in.Get("shift_video") != 12.0 || in.Get("shift_audio") != 1.5 {
		t.Errorf("shift_audio only: %v", in)
	}
	// Chain order: unet -> lora -> shift -> attn -> guider/sigmas.
	all := base
	all.ShiftVideo, all.Attention = f64(10), "comfy kitchen attention"
	ag := Build(all)
	chain := map[string]string{"lora": "unet", "shift": "lora", "attn": "shift", "guider": "attn", "sigmas": "attn"}
	for node, src := range chain {
		if !reflect.DeepEqual(ag.Node(node).Inputs.Get("model"), []any{src, 0}) {
			t.Errorf("%s.model = %v, want [%s 0]", node, ag.Node(node).Inputs.Get("model"), src)
		}
	}

	// i2v with only a first frame: no last node, no last_frame input.
	i := base
	i.Mode, i.FirstFrame = "i2v", "h3gen/a.png"
	ig := Build(i)
	if ig.Has("last") {
		t.Errorf("i2v with first frame only must not contain last")
	}
	if ig.Node("cond").Inputs.Has("last_frame") {
		t.Errorf("i2v with first frame only must not link last_frame")
	}
	if !reflect.DeepEqual(ig.Node("cond").Inputs.Get("first_frame"), []any{"first", 0}) {
		t.Errorf("first_frame link: %v", ig.Node("cond").Inputs.Get("first_frame"))
	}

	// Every link references an existing node.
	for _, g := range []Graph{g, rg, ng, ag, ig} {
		for _, n := range g {
			for _, in := range n.Inputs {
				l, ok := in.Value.([]any)
				if !ok {
					continue
				}
				if !g.Has(l[0].(string)) {
					t.Errorf("%s.%s links to missing node %v", n.ID, in.Key, l[0])
				}
			}
		}
	}
}

// A struct-literal spec without Files still resolves every model from DefaultFiles; explicit entries override.
func TestBuildFilesFallback(t *testing.T) {
	g := Build(GenSpec{Prompt: "p", Mode: "t2v", Steps: 8, Turbo: true})
	if g.Node("unet").Inputs.Get("unet_name") != DefaultFiles["fl2va"] || g.Node("clip").Inputs.Get("clip_name") != DefaultFiles["clip"] ||
		g.Node("vae_v").Inputs.Get("vae_name") != DefaultFiles["vae_video"] || g.Node("vae_a").Inputs.Get("vae_name") != DefaultFiles["vae_audio"] ||
		g.Node("lora").Inputs.Get("lora_name") != DefaultFiles["lora_fl2v_8step"] {
		t.Errorf("nil Files must fall back to DefaultFiles: %v", g)
	}
	s := NewGenSpec("p")
	s.Files["fl2va"] = "custom_unet.safetensors"
	s.Files["clip"] = "custom_clip.safetensors"
	g = Build(s)
	if g.Node("unet").Inputs.Get("unet_name") != "custom_unet.safetensors" || g.Node("clip").Inputs.Get("clip_name") != "custom_clip.safetensors" {
		t.Errorf("Files overrides not applied: %v %v", g.Node("unet").Inputs, g.Node("clip").Inputs)
	}
	if g.Node("vae_v").Inputs.Get("vae_name") != DefaultFiles["vae_video"] {
		t.Errorf("untouched Files entries must keep their defaults")
	}
}

// The graph must serialise the way ComfyUI expects: class_type / inputs keys, links as [id, index] arrays.
// TestBuildKeyOrder: the JSON sent to /prompt lists the nodes and each node's inputs in the order
// h3gen.graph.build inserted them (json.dumps of a dict). ComfyUI's planner (comfy_execution/graph.py) walks
// a node's inputs in JSON key order and picks ready nodes in insertion order, so a different key order runs
// the same graph in a different order (the UNET+LoRA before the text encoder) and changes the
// "[h3gen] executing" lines and runs.jsonl node_times. Goldens: ' '.join(g.keys()) and
// ' '.join(n['inputs'].keys()) from the Python build for the TestBuildParity specs.
func TestBuildKeyOrder(t *testing.T) {
	type want struct {
		nodes  string
		inputs map[string]string
	}
	common := map[string]string{
		"unet": "unet_name weight_dtype", "clip": "clip_name type device", "vae_v": "vae_name", "vae_a": "vae_name",
		"lora": "model lora_name strength_model", "shift": "model shift_video shift_audio", "attn": "model attention",
		"guider": "model conditioning", "sampler": "sampler_name", "sigmas": "model scheduler steps denoise",
		"noise": "noise_seed", "sample": "noise guider sampler sigmas latent_image", "dec_v": "samples vae",
		"dec_a": "samples vae", "video": "images fps audio", "save": "video filename_prefix format codec",
		"first": "image", "last": "image", "ref_img_0": "image", "ref_img_1": "image", "ref_aud_0": "audio",
	}
	cases := []struct {
		name string
		spec func() GenSpec
		want want
	}{
		{"t2v turbo", func() GenSpec { return NewGenSpec("p") }, want{
			"unet clip vae_v vae_a lora cond guider sampler sigmas noise sample dec_v dec_a video save",
			map[string]string{"cond": "clip vae prompt width height length"}}},
		{"t2v no-turbo shift attention", func() GenSpec {
			s := NewGenSpec("p")
			s.Turbo = false
			s.ShiftVideo, s.ShiftAudio = f64(8.0), f64(2.5)
			s.Attention = "pytorch attention"
			return s
		}, want{
			"unet clip vae_v vae_a shift attn cond guider sampler sigmas noise sample dec_v dec_a video save",
			map[string]string{"cond": "clip vae prompt width height length"}}},
		{"i2v first+last", func() GenSpec {
			s := NewGenSpec("p")
			s.Mode = "i2v"
			s.FirstFrame, s.LastFrame = "h3gen/first.png", "h3gen/last.png"
			return s
		}, want{
			"unet clip vae_v vae_a lora first last cond guider sampler sigmas noise sample dec_v dec_a video save",
			map[string]string{"cond": "clip vae prompt width height length first_frame last_frame"}}},
		{"r2v two images one audio", func() GenSpec {
			s := NewGenSpec("p")
			s.Mode = "r2v"
			s.RefImages = []string{"h3gen/ref_a.png", "h3gen/ref_b.jpg"}
			s.RefAudios = []string{"h3gen/voice.wav"}
			return s
		}, want{
			"unet clip vae_v vae_a lora ref_img_0 ref_img_1 ref_aud_0 cond guider sampler sigmas noise sample dec_v dec_a video save",
			map[string]string{"cond": "clip vae audio_vae prompt width height length ref_image_size ref_images.ref_image_0 ref_images.ref_image_1 ref_audios.ref_audio_0"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := Build(c.spec())
			if got := strings.Join(g.IDs(), " "); got != c.want.nodes {
				t.Errorf("node order\n got %s\nwant %s", got, c.want.nodes)
			}
			for _, n := range g {
				want, ok := c.want.inputs[n.ID]
				if !ok {
					want = common[n.ID]
				}
				var keys []string
				for _, in := range n.Inputs {
					keys = append(keys, in.Key)
				}
				if got := strings.Join(keys, " "); got != want {
					t.Errorf("%s inputs order\n got %s\nwant %s", n.ID, got, want)
				}
			}
			// The order survives serialisation: the wire form lists the keys in the same order.
			raw, err := json.Marshal(g)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(jsonKeyOrder(t, raw), " "); got != c.want.nodes {
				t.Errorf("JSON node order\n got %s\nwant %s", got, c.want.nodes)
			}
			var nodes map[string]json.RawMessage
			if err := json.Unmarshal(raw, &nodes); err != nil {
				t.Fatal(err)
			}
			var sample struct {
				Inputs json.RawMessage `json:"inputs"`
			}
			if err := json.Unmarshal(nodes["sample"], &sample); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(jsonKeyOrder(t, sample.Inputs), " "); got != common["sample"] {
				t.Errorf("JSON sample inputs order\n got %s\nwant %s", got, common["sample"])
			}
		})
	}
}

// jsonKeyOrder returns the top-level keys of a JSON object in document order.
func jsonKeyOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object: %v %s", err, raw)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// Set keeps a dict's semantics: re-assigning an existing key keeps its position, a new key is appended.
func TestOrderedSet(t *testing.T) {
	var in Inputs
	in.Set("a", 1)
	in.Set("b", 2)
	in.Set("a", 3)
	if !reflect.DeepEqual(in, Inputs{{"a", 3}, {"b", 2}}) || in.Get("a") != 3 || in.Get("c") != nil || in.Has("c") {
		t.Errorf("Inputs.Set: %v", in)
	}
	var g Graph
	g.Set("x", Node{"A", nil})
	g.Set("y", Node{"B", nil})
	g.Set("x", Node{"C", nil})
	if !reflect.DeepEqual(g.IDs(), []string{"x", "y"}) || g.Node("x").ClassType != "C" || g.Has("z") || g.Node("z").ClassType != "" {
		t.Errorf("Graph.Set: %v", g)
	}
	raw, err := json.Marshal(Graph{{"n", Node{"T", Inputs{{"text", "<d>a & b</d>"}, {"k", []any{"n", 0}}}}}})
	if err != nil || string(raw) != `{"n":{"class_type":"T","inputs":{"text":"\u003cd\u003ea \u0026 b\u003c/d\u003e","k":["n",0]}}}` {
		t.Errorf("json.Marshal: %s %v", raw, err)
	}
	// The station's encoder (SetEscapeHTML(false)) ships the <d> tags verbatim, like json.dumps.
	raw, err = encodeJSON(Graph{{"n", Node{"T", Inputs{{"text", "<d>a & b</d>"}}}}})
	if err != nil || string(raw) != `{"n":{"class_type":"T","inputs":{"text":"<d>a & b</d>"}}}` {
		t.Errorf("encodeJSON: %s %v", raw, err)
	}
	// An empty node has empty inputs, never null.
	raw, _ = json.Marshal(Node{ClassType: "E"})
	if string(raw) != `{"class_type":"E","inputs":{}}` {
		t.Errorf("empty Inputs: %s", raw)
	}
}

func TestGraphJSONShape(t *testing.T) {
	raw, err := json.Marshal(Build(NewGenSpec("p")))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, frag := range []string{`"class_type":"UNETLoader"`, `"inputs":{`, `["unet",0]`, `"fps":24`, `"noise_seed":0`, `"length":124`} {
		if !strings.Contains(s, frag) {
			t.Errorf("serialised graph lacks %s:\n%s", frag, s)
		}
	}
}

// Python goldens: json.dumps(h3gen.graph.build(spec), sort_keys=True, ensure_ascii=False, indent=1) for the specs
// in TestBuildParity (generated with .venv\Scripts\python.exe from the repo root; do not edit by hand).

const goldenT2VTurbo = `{
 "clip": {
  "class_type": "CLIPLoader",
  "inputs": {
   "clip_name": "qwen3vl_32b_minimax_h3_int8_convrot.safetensors",
   "device": "default",
   "type": "minimax"
  }
 },
 "cond": {
  "class_type": "MiniMaxH3ImageToVideo",
  "inputs": {
   "clip": [
    "clip",
    0
   ],
   "height": 480,
   "length": 124,
   "prompt": "A calm studio, a presenter says <d>こんにちは、ニュースの時間です。</d> & smiles \"warmly\"",
   "vae": [
    "vae_v",
    0
   ],
   "width": 832
  }
 },
 "dec_a": {
  "class_type": "VAEDecodeAudio",
  "inputs": {
   "samples": [
    "sample",
    0
   ],
   "vae": [
    "vae_a",
    0
   ]
  }
 },
 "dec_v": {
  "class_type": "VAEDecode",
  "inputs": {
   "samples": [
    "sample",
    0
   ],
   "vae": [
    "vae_v",
    0
   ]
  }
 },
 "guider": {
  "class_type": "BasicGuider",
  "inputs": {
   "conditioning": [
    "cond",
    0
   ],
   "model": [
    "lora",
    0
   ]
  }
 },
 "lora": {
  "class_type": "LoraLoaderModelOnly",
  "inputs": {
   "lora_name": "minimax_h3_fl2v_turbo_8step_v1.0_comfyui_bf16.safetensors",
   "model": [
    "unet",
    0
   ],
   "strength_model": 1.0
  }
 },
 "noise": {
  "class_type": "RandomNoise",
  "inputs": {
   "noise_seed": 12345
  }
 },
 "sample": {
  "class_type": "SamplerCustomAdvanced",
  "inputs": {
   "guider": [
    "guider",
    0
   ],
   "latent_image": [
    "cond",
    1
   ],
   "noise": [
    "noise",
    0
   ],
   "sampler": [
    "sampler",
    0
   ],
   "sigmas": [
    "sigmas",
    0
   ]
  }
 },
 "sampler": {
  "class_type": "KSamplerSelect",
  "inputs": {
   "sampler_name": "res_multistep"
  }
 },
 "save": {
  "class_type": "SaveVideo",
  "inputs": {
   "codec": "h264",
   "filename_prefix": "h3/h3",
   "format": "mp4",
   "video": [
    "video",
    0
   ]
  }
 },
 "sigmas": {
  "class_type": "BasicScheduler",
  "inputs": {
   "denoise": 1.0,
   "model": [
    "lora",
    0
   ],
   "scheduler": "simple",
   "steps": 8
  }
 },
 "unet": {
  "class_type": "UNETLoader",
  "inputs": {
   "unet_name": "minimax_h3_fl2va_pruned_int8_convrot.safetensors",
   "weight_dtype": "default"
  }
 },
 "vae_a": {
  "class_type": "VAELoader",
  "inputs": {
   "vae_name": "minimax_h3_audio_vae_fp32.safetensors"
  }
 },
 "vae_v": {
  "class_type": "VAELoader",
  "inputs": {
   "vae_name": "minimax_h3_video_vae_fp16.safetensors"
  }
 },
 "video": {
  "class_type": "CreateVideo",
  "inputs": {
   "audio": [
    "dec_a",
    0
   ],
   "fps": 24,
   "images": [
    "dec_v",
    0
   ]
  }
 }
}`

const goldenT2VNoTurboShiftAttn = `{
 "attn": {
  "class_type": "ModelAttentionBackend",
  "inputs": {
   "attention": "pytorch attention",
   "model": [
    "shift",
    0
   ]
  }
 },
 "clip": {
  "class_type": "CLIPLoader",
  "inputs": {
   "clip_name": "qwen3vl_32b_minimax_h3_int8_convrot.safetensors",
   "device": "default",
   "type": "minimax"
  }
 },
 "cond": {
  "class_type": "MiniMaxH3ImageToVideo",
  "inputs": {
   "clip": [
    "clip",
    0
   ],
   "height": 768,
   "length": 243,
   "prompt": "Night city, rain, neon 雨の夜",
   "vae": [
    "vae_v",
    0
   ],
   "width": 1344
  }
 },
 "dec_a": {
  "class_type": "VAEDecodeAudio",
  "inputs": {
   "samples": [
    "sample",
    0
   ],
   "vae": [
    "vae_a",
    0
   ]
  }
 },
 "dec_v": {
  "class_type": "VAEDecode",
  "inputs": {
   "samples": [
    "sample",
    0
   ],
   "vae": [
    "vae_v",
    0
   ]
  }
 },
 "guider": {
  "class_type": "BasicGuider",
  "inputs": {
   "conditioning": [
    "cond",
    0
   ],
   "model": [
    "attn",
    0
   ]
  }
 },
 "noise": {
  "class_type": "RandomNoise",
  "inputs": {
   "noise_seed": 7
  }
 },
 "sample": {
  "class_type": "SamplerCustomAdvanced",
  "inputs": {
   "guider": [
    "guider",
    0
   ],
   "latent_image": [
    "cond",
    1
   ],
   "noise": [
    "noise",
    0
   ],
   "sampler": [
    "sampler",
    0
   ],
   "sigmas": [
    "sigmas",
    0
   ]
  }
 },
 "sampler": {
  "class_type": "KSamplerSelect",
  "inputs": {
   "sampler_name": "euler"
  }
 },
 "save": {
  "class_type": "SaveVideo",
  "inputs": {
   "codec": "h264",
   "filename_prefix": "aitv/seg",
   "format": "mp4",
   "video": [
    "video",
    0
   ]
  }
 },
 "shift": {
  "class_type": "MiniMaxH3SigmaShift",
  "inputs": {
   "model": [
    "unet",
    0
   ],
   "shift_audio": 2.5,
   "shift_video": 8.0
  }
 },
 "sigmas": {
  "class_type": "BasicScheduler",
  "inputs": {
   "denoise": 1.0,
   "model": [
    "attn",
    0
   ],
   "scheduler": "beta",
   "steps": 30
  }
 },
 "unet": {
  "class_type": "UNETLoader",
  "inputs": {
   "unet_name": "minimax_h3_fl2va_pruned_int8_convrot.safetensors",
   "weight_dtype": "default"
  }
 },
 "vae_a": {
  "class_type": "VAELoader",
  "inputs": {
   "vae_name": "minimax_h3_audio_vae_fp32.safetensors"
  }
 },
 "vae_v": {
  "class_type": "VAELoader",
  "inputs": {
   "vae_name": "minimax_h3_video_vae_fp16.safetensors"
  }
 },
 "video": {
  "class_type": "CreateVideo",
  "inputs": {
   "audio": [
    "dec_a",
    0
   ],
   "fps": 24,
   "images": [
    "dec_v",
    0
   ]
  }
 }
}`

const goldenI2VFirstLast = `{
 "clip": {
  "class_type": "CLIPLoader",
  "inputs": {
   "clip_name": "qwen3vl_32b_minimax_h3_int8_convrot.safetensors",
   "device": "default",
   "type": "minimax"
  }
 },
 "cond": {
  "class_type": "MiniMaxH3ImageToVideo",
  "inputs": {
   "clip": [
    "clip",
    0
   ],
   "first_frame": [
    "first",
    0
   ],
   "height": 1344,
   "last_frame": [
    "last",
    0
   ],
   "length": 226,
   "prompt": "Slow push-in on the mountain",
   "vae": [
    "vae_v",
    0
   ],
   "width": 768
  }
 },
 "dec_a": {
  "class_type": "VAEDecodeAudio",
  "inputs": {
   "samples": [
    "sample",
    0
   ],
   "vae": [
    "vae_a",
    0
   ]
  }
 },
 "dec_v": {
  "class_type": "VAEDecode",
  "inputs": {
   "samples": [
    "sample",
    0
   ],
   "vae": [
    "vae_v",
    0
   ]
  }
 },
 "first": {
  "class_type": "LoadImage",
  "inputs": {
   "image": "h3gen/first.png"
  }
 },
 "guider": {
  "class_type": "BasicGuider",
  "inputs": {
   "conditioning": [
    "cond",
    0
   ],
   "model": [
    "lora",
    0
   ]
  }
 },
 "last": {
  "class_type": "LoadImage",
  "inputs": {
   "image": "h3gen/last.png"
  }
 },
 "lora": {
  "class_type": "LoraLoaderModelOnly",
  "inputs": {
   "lora_name": "minimax_h3_fl2v_turbo_4step_v1.0_768p_comfyui_bf16.safetensors",
   "model": [
    "unet",
    0
   ],
   "strength_model": 0.8
  }
 },
 "noise": {
  "class_type": "RandomNoise",
  "inputs": {
   "noise_seed": 99
  }
 },
 "sample": {
  "class_type": "SamplerCustomAdvanced",
  "inputs": {
   "guider": [
    "guider",
    0
   ],
   "latent_image": [
    "cond",
    1
   ],
   "noise": [
    "noise",
    0
   ],
   "sampler": [
    "sampler",
    0
   ],
   "sigmas": [
    "sigmas",
    0
   ]
  }
 },
 "sampler": {
  "class_type": "KSamplerSelect",
  "inputs": {
   "sampler_name": "res_multistep"
  }
 },
 "save": {
  "class_type": "SaveVideo",
  "inputs": {
   "codec": "h264",
   "filename_prefix": "h3/h3",
   "format": "mp4",
   "video": [
    "video",
    0
   ]
  }
 },
 "sigmas": {
  "class_type": "BasicScheduler",
  "inputs": {
   "denoise": 1.0,
   "model": [
    "lora",
    0
   ],
   "scheduler": "simple",
   "steps": 4
  }
 },
 "unet": {
  "class_type": "UNETLoader",
  "inputs": {
   "unet_name": "minimax_h3_fl2va_pruned_int8_convrot.safetensors",
   "weight_dtype": "default"
  }
 },
 "vae_a": {
  "class_type": "VAELoader",
  "inputs": {
   "vae_name": "minimax_h3_audio_vae_fp32.safetensors"
  }
 },
 "vae_v": {
  "class_type": "VAELoader",
  "inputs": {
   "vae_name": "minimax_h3_video_vae_fp16.safetensors"
  }
 },
 "video": {
  "class_type": "CreateVideo",
  "inputs": {
   "audio": [
    "dec_a",
    0
   ],
   "fps": 24,
   "images": [
    "dec_v",
    0
   ]
  }
 }
}`

const goldenR2VRefs = `{
 "clip": {
  "class_type": "CLIPLoader",
  "inputs": {
   "clip_name": "qwen3vl_32b_minimax_h3_int8_convrot.safetensors",
   "device": "default",
   "type": "minimax"
  }
 },
 "cond": {
  "class_type": "MiniMaxH3ReferenceToVideo",
  "inputs": {
   "audio_vae": [
    "vae_a",
    0
   ],
   "clip": [
    "clip",
    0
   ],
   "height": 1024,
   "length": 175,
   "prompt": "The character waves and speaks",
   "ref_audios.ref_audio_0": [
    "ref_aud_0",
    0
   ],
   "ref_image_size": "max",
   "ref_images.ref_image_0": [
    "ref_img_0",
    0
   ],
   "ref_images.ref_image_1": [
    "ref_img_1",
    0
   ],
   "vae": [
    "vae_v",
    0
   ],
   "width": 1024
  }
 },
 "dec_a": {
  "class_type": "VAEDecodeAudio",
  "inputs": {
   "samples": [
    "sample",
    0
   ],
   "vae": [
    "vae_a",
    0
   ]
  }
 },
 "dec_v": {
  "class_type": "VAEDecode",
  "inputs": {
   "samples": [
    "sample",
    0
   ],
   "vae": [
    "vae_v",
    0
   ]
  }
 },
 "guider": {
  "class_type": "BasicGuider",
  "inputs": {
   "conditioning": [
    "cond",
    0
   ],
   "model": [
    "lora",
    0
   ]
  }
 },
 "lora": {
  "class_type": "LoraLoaderModelOnly",
  "inputs": {
   "lora_name": "minimax_h3_ref2v_turbo_4step_v0.1_comfyui_bf16.safetensors",
   "model": [
    "unet",
    0
   ],
   "strength_model": 0.75
  }
 },
 "noise": {
  "class_type": "RandomNoise",
  "inputs": {
   "noise_seed": 2147483647
  }
 },
 "ref_aud_0": {
  "class_type": "LoadAudio",
  "inputs": {
   "audio": "h3gen/voice.wav"
  }
 },
 "ref_img_0": {
  "class_type": "LoadImage",
  "inputs": {
   "image": "h3gen/ref_a.png"
  }
 },
 "ref_img_1": {
  "class_type": "LoadImage",
  "inputs": {
   "image": "h3gen/ref_b.jpg"
  }
 },
 "sample": {
  "class_type": "SamplerCustomAdvanced",
  "inputs": {
   "guider": [
    "guider",
    0
   ],
   "latent_image": [
    "cond",
    1
   ],
   "noise": [
    "noise",
    0
   ],
   "sampler": [
    "sampler",
    0
   ],
   "sigmas": [
    "sigmas",
    0
   ]
  }
 },
 "sampler": {
  "class_type": "KSamplerSelect",
  "inputs": {
   "sampler_name": "res_multistep"
  }
 },
 "save": {
  "class_type": "SaveVideo",
  "inputs": {
   "codec": "h264",
   "filename_prefix": "h3/ref",
   "format": "mp4",
   "video": [
    "video",
    0
   ]
  }
 },
 "sigmas": {
  "class_type": "BasicScheduler",
  "inputs": {
   "denoise": 1.0,
   "model": [
    "lora",
    0
   ],
   "scheduler": "simple",
   "steps": 4
  }
 },
 "unet": {
  "class_type": "UNETLoader",
  "inputs": {
   "unet_name": "minimax_h3_ref2va_pruned_int8_convrot.safetensors",
   "weight_dtype": "default"
  }
 },
 "vae_a": {
  "class_type": "VAELoader",
  "inputs": {
   "vae_name": "minimax_h3_audio_vae_fp32.safetensors"
  }
 },
 "vae_v": {
  "class_type": "VAELoader",
  "inputs": {
   "vae_name": "minimax_h3_video_vae_fp16.safetensors"
  }
 },
 "video": {
  "class_type": "CreateVideo",
  "inputs": {
   "audio": [
    "dec_a",
    0
   ],
   "fps": 24,
   "images": [
    "dec_v",
    0
   ]
  }
 }
}`
