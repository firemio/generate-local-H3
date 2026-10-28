// Package comfy talks to a local ComfyUI: graph construction for MiniMax H3 (graph.go) and the
// HTTP/WebSocket client that queues a graph, follows its execution and resolves the output files (client.go).
//
// graph.go mirrors the official ComfyUI templates video_minimax_h3_t2v / i2v / r2v (ComfyUI 0.35).
// All three modes share the same tail:
//
//	UNETLoader -> (LoraLoaderModelOnly turbo) -> (MiniMaxH3SigmaShift) -> (ModelAttentionBackend)
//	-> BasicGuider + BasicScheduler(res_multistep/simple) -> SamplerCustomAdvanced
//	-> VAEDecode (video) + VAEDecodeAudio (audio) -> CreateVideo(24fps) -> SaveVideo
package comfy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// FPS is H3's frame rate.
const FPS = 24

// DefaultFiles are the model filenames from Comfy-Org/MiniMax-H3 on Hugging Face, keyed
// fl2va, ref2va, clip, vae_video, vae_audio, lora_fl2v_8step, lora_fl2v_4step, lora_ref2v_4step.
var DefaultFiles = map[string]string{
	"fl2va":            "minimax_h3_fl2va_pruned_int8_convrot.safetensors",
	"ref2va":           "minimax_h3_ref2va_pruned_int8_convrot.safetensors",
	"clip":             "qwen3vl_32b_minimax_h3_int8_convrot.safetensors",
	"vae_video":        "minimax_h3_video_vae_fp16.safetensors",
	"vae_audio":        "minimax_h3_audio_vae_fp32.safetensors",
	"lora_fl2v_8step":  "minimax_h3_fl2v_turbo_8step_v1.0_comfyui_bf16.safetensors",
	"lora_fl2v_4step":  "minimax_h3_fl2v_turbo_4step_v1.0_768p_comfyui_bf16.safetensors",
	"lora_ref2v_4step": "minimax_h3_ref2v_turbo_4step_v0.1_comfyui_bf16.safetensors",
}

// SnapFrames is the frame count on H3's 17k+5 grid at 24 fps: n = max(5, round(seconds*24)) rounded half
// to even, then raised to the next 17k+5. NaN and anything shorter than 5 frames give 5; an absurd length
// is capped at MaxInt32 (ComfyUI refuses it either way).
func SnapFrames(seconds float64) int {
	x := math.RoundToEven(seconds * FPS)
	if !(x > 5) {
		x = 5
	}
	if x > math.MaxInt32 {
		x = math.MaxInt32
	}
	n := int(x)
	return n + (22-n%17)%17
}

// FramesSeconds is the duration in seconds of a frame count.
func FramesSeconds(frames int) float64 { return float64(frames) / FPS }

// Canvas returns the resolution preset for an aspect ("16:9", "9:16", "1:1") and a megapixel tier:
// >= 0.9 native (1344x768 / 768x1344 / 1024x1024), >= 0.3 draft (832x480 / 480x832 / 640x640), else small
// (only 16:9 has a small preset, 640x352). Unknown combinations fall back to 832x480.
func Canvas(aspect string, megapixels float64) (width, height int) {
	tier := "small"
	switch {
	case megapixels >= 0.9:
		tier = "native"
	case megapixels >= 0.3:
		tier = "draft"
	}
	switch aspect + "/" + tier {
	case "16:9/native":
		return 1344, 768
	case "9:16/native":
		return 768, 1344
	case "1:1/native":
		return 1024, 1024
	case "16:9/draft":
		return 832, 480
	case "9:16/draft":
		return 480, 832
	case "1:1/draft":
		return 640, 640
	case "16:9/small":
		return 640, 352
	}
	return 832, 480
}

// GenSpec describes one generation.
type GenSpec struct {
	Prompt string
	Mode   string // t2v | i2v | r2v
	Width  int
	Height int
	// Seconds is snapped to the 17k+5 frame grid by Length.
	Seconds float64
	// Seed is RandomNoise's noise_seed (ComfyUI accepts 0 .. 2^64-1).
	Seed  uint64
	Steps int
	Turbo bool
	// LoraStrength is strength_model of the turbo LoRA.
	LoraStrength float64
	// ShiftVideo / ShiftAudio: nil = model default (12.0 / 3.0). The MiniMaxH3SigmaShift node is only added when at least one is set.
	ShiftVideo *float64
	ShiftAudio *float64
	// Attention: "" | "pytorch attention" | "comfy kitchen attention" (ModelAttentionBackend node only when set).
	Attention string
	Scheduler string
	Sampler   string
	// FirstFrame / LastFrame are uploaded image names (i2v).
	FirstFrame string
	LastFrame  string
	// RefImages / RefAudios are uploaded names (r2v).
	RefImages    []string
	RefAudios    []string
	RefImageSize string // "match" | "max"
	// ClipDevice is CLIPLoader's device ("default" or "cpu"). "cpu" keeps the 26 GB text encoder out of
	// VRAM: long clips at 832x480 otherwise push HIGH_VRAM past the per-process cap, ComfyUI unloads the
	// resident INT8 DiT and on ROCm 10 that unload faults (docs/pitfalls.md §8).
	ClipDevice string
	// FilenamePrefix is SaveVideo's filename_prefix ("h3/h3" for the CLI, "aitv/seg" for the station).
	FilenamePrefix string
	// Files overrides DefaultFiles entries (a copy of DefaultFiles by default).
	Files map[string]string
}

// NewGenSpec returns a spec with the defaults: t2v, 832x480, 5 s, seed 0, 8 steps, turbo on,
// LoRA strength 1.0, scheduler "simple", sampler "res_multistep", ref_image_size "match", prefix "h3/h3",
// Files = a fresh copy of DefaultFiles.
func NewGenSpec(prompt string) GenSpec {
	files := make(map[string]string, len(DefaultFiles))
	for k, v := range DefaultFiles {
		files[k] = v
	}
	return GenSpec{
		Prompt:         prompt,
		Mode:           "t2v",
		Width:          832,
		Height:         480,
		Seconds:        5.0,
		Steps:          8,
		Turbo:          true,
		LoraStrength:   1.0,
		Scheduler:      "simple",
		Sampler:        "res_multistep",
		RefImageSize:   "match",
		FilenamePrefix: "h3/h3",
		Files:          files,
	}
}

// Length is the snapped frame count of Seconds (SnapFrames).
func (s GenSpec) Length() int { return SnapFrames(s.Seconds) }

// file returns the model filename for key: the spec's override when present, else the DefaultFiles entry
// (so a GenSpec built as a struct literal without Files still resolves every model).
func (s GenSpec) file(key string) string {
	if v, ok := s.Files[key]; ok {
		return v
	}
	return DefaultFiles[key]
}

// clipDevice is CLIPLoader's device: "cpu" only when asked for, else "default" (see GenSpec.ClipDevice).
func (s GenSpec) clipDevice() string {
	if s.ClipDevice == "cpu" {
		return "cpu"
	}
	return "default"
}

// Input is one entry of a node's inputs.
type Input struct {
	Key   string
	Value any
}

// Inputs is a node's inputs in insertion order. The order is part of the wire format: ComfyUI walks a
// node's inputs in JSON key order when it plans the execution (comfy_execution/graph.py
// TopologicalSort.add_node), so the order decides which loader runs first.
type Inputs []Input

// Set assigns key: an existing key keeps its position, a new one is appended.
func (in *Inputs) Set(key string, value any) {
	for i := range *in {
		if (*in)[i].Key == key {
			(*in)[i].Value = value
			return
		}
	}
	*in = append(*in, Input{key, value})
}

// Get returns the value of key, or nil when it is absent (Has tells them apart).
func (in Inputs) Get(key string) any {
	for _, e := range in {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}

// Has reports whether key is present.
func (in Inputs) Has(key string) bool {
	for _, e := range in {
		if e.Key == key {
			return true
		}
	}
	return false
}

// MarshalJSON writes the inputs as a JSON object in insertion order.
func (in Inputs) MarshalJSON() ([]byte, error) {
	return marshalObject(len(in), func(i int) (string, any) { return in[i].Key, in[i].Value })
}

// Node is one API-format ComfyUI node.
type Node struct {
	ClassType string `json:"class_type"`
	Inputs    Inputs `json:"inputs"`
}

// GraphNode is one entry of a Graph: node id and node.
type GraphNode struct {
	ID string
	Node
}

// Graph is an API-format ComfyUI prompt: node id -> node, in insertion order (the order Build created the
// nodes, which is the order ComfyUI's planner sees them in the JSON). Links are []any{"<node id>", <output index>}.
type Graph []GraphNode

// Set assigns id: an existing id keeps its position, a new one is appended.
func (g *Graph) Set(id string, n Node) {
	for i := range *g {
		if (*g)[i].ID == id {
			(*g)[i].Node = n
			return
		}
	}
	*g = append(*g, GraphNode{id, n})
}

// Node returns the node with id, or a zero Node when it is absent (Has tells them apart).
func (g Graph) Node(id string) Node {
	for _, e := range g {
		if e.ID == id {
			return e.Node
		}
	}
	return Node{}
}

// Has reports whether a node with id is present.
func (g Graph) Has(id string) bool {
	for _, e := range g {
		if e.ID == id {
			return true
		}
	}
	return false
}

// IDs lists the node ids in insertion order.
func (g Graph) IDs() []string {
	out := make([]string, len(g))
	for i, e := range g {
		out[i] = e.ID
	}
	return out
}

// MarshalJSON writes the graph as a JSON object in insertion order. Pass the graph to an encoder with
// SetIndent("", " ") for the readable form `h3gen gen --dry-run` prints.
func (g Graph) MarshalJSON() ([]byte, error) {
	return marshalObject(len(g), func(i int) (string, any) { return g[i].ID, g[i].Node })
}

// marshalObject writes n key/value pairs as a JSON object in the given order.
func marshalObject(n int, pair func(i int) (string, any)) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i := 0; i < n; i++ {
		key, value := pair(i)
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(key)
		v, err := encodeJSON(value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// encodeJSON is json.Marshal without HTML escaping (prompts carry <d> tags) and without a trailing newline.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// link is a ComfyUI link: ["<node id>", <output index>].
func link(node string, output int) []any { return []any{node, output} }

// Build assembles the API graph: node ids unet, clip, vae_v, vae_a, (lora), (shift), (attn),
// (first/last | ref_img_N/ref_aud_N), cond, guider, sampler, sigmas, noise, sample, dec_v, dec_a, video,
// save, in that order. The turbo LoRA is lora_ref2v_4step for r2v, else lora_fl2v_4step when Steps <= 4 and
// lora_fl2v_8step otherwise.
func Build(spec GenSpec) Graph {
	isRef := spec.Mode == "r2v"
	unet := spec.file("fl2va")
	if isRef {
		unet = spec.file("ref2va")
	}
	g := Graph{}

	g.Set("unet", Node{"UNETLoader", Inputs{{"unet_name", unet}, {"weight_dtype", "default"}}})
	g.Set("clip", Node{"CLIPLoader", Inputs{{"clip_name", spec.file("clip")}, {"type", "minimax"}, {"device", spec.clipDevice()}}})
	g.Set("vae_v", Node{"VAELoader", Inputs{{"vae_name", spec.file("vae_video")}}})
	g.Set("vae_a", Node{"VAELoader", Inputs{{"vae_name", spec.file("vae_audio")}}})

	modelRef := link("unet", 0)
	if spec.Turbo {
		var lora string
		switch {
		case isRef:
			lora = spec.file("lora_ref2v_4step")
		case spec.Steps <= 4:
			lora = spec.file("lora_fl2v_4step")
		default:
			lora = spec.file("lora_fl2v_8step")
		}
		g.Set("lora", Node{"LoraLoaderModelOnly",
			Inputs{{"model", modelRef}, {"lora_name", lora}, {"strength_model", spec.LoraStrength}}})
		modelRef = link("lora", 0)
	}
	if spec.ShiftVideo != nil || spec.ShiftAudio != nil {
		shiftVideo, shiftAudio := 12.0, 3.0
		if spec.ShiftVideo != nil {
			shiftVideo = *spec.ShiftVideo
		}
		if spec.ShiftAudio != nil {
			shiftAudio = *spec.ShiftAudio
		}
		g.Set("shift", Node{"MiniMaxH3SigmaShift",
			Inputs{{"model", modelRef}, {"shift_video", shiftVideo}, {"shift_audio", shiftAudio}}})
		modelRef = link("shift", 0)
	}
	if spec.Attention != "" {
		g.Set("attn", Node{"ModelAttentionBackend", Inputs{{"model", modelRef}, {"attention", spec.Attention}}})
		modelRef = link("attn", 0)
	}

	if isRef {
		cond := Inputs{
			{"clip", link("clip", 0)}, {"vae", link("vae_v", 0)}, {"audio_vae", link("vae_a", 0)},
			{"prompt", spec.Prompt}, {"width", spec.Width}, {"height", spec.Height},
			{"length", spec.Length()}, {"ref_image_size", spec.RefImageSize},
		}
		for i, name := range spec.RefImages {
			nid := fmt.Sprintf("ref_img_%d", i)
			g.Set(nid, Node{"LoadImage", Inputs{{"image", name}}})
			cond.Set(fmt.Sprintf("ref_images.ref_image_%d", i), link(nid, 0))
		}
		for i, name := range spec.RefAudios {
			nid := fmt.Sprintf("ref_aud_%d", i)
			g.Set(nid, Node{"LoadAudio", Inputs{{"audio", name}}})
			cond.Set(fmt.Sprintf("ref_audios.ref_audio_%d", i), link(nid, 0))
		}
		g.Set("cond", Node{"MiniMaxH3ReferenceToVideo", cond})
	} else {
		cond := Inputs{
			{"clip", link("clip", 0)}, {"vae", link("vae_v", 0)}, {"prompt", spec.Prompt},
			{"width", spec.Width}, {"height", spec.Height}, {"length", spec.Length()},
		}
		if spec.FirstFrame != "" {
			g.Set("first", Node{"LoadImage", Inputs{{"image", spec.FirstFrame}}})
			cond.Set("first_frame", link("first", 0))
		}
		if spec.LastFrame != "" {
			g.Set("last", Node{"LoadImage", Inputs{{"image", spec.LastFrame}}})
			cond.Set("last_frame", link("last", 0))
		}
		g.Set("cond", Node{"MiniMaxH3ImageToVideo", cond})
	}

	g.Set("guider", Node{"BasicGuider", Inputs{{"model", modelRef}, {"conditioning", link("cond", 0)}}})
	g.Set("sampler", Node{"KSamplerSelect", Inputs{{"sampler_name", spec.Sampler}}})
	g.Set("sigmas", Node{"BasicScheduler",
		Inputs{{"model", modelRef}, {"scheduler", spec.Scheduler}, {"steps", spec.Steps}, {"denoise", 1.0}}})
	g.Set("noise", Node{"RandomNoise", Inputs{{"noise_seed", spec.Seed}}})
	g.Set("sample", Node{"SamplerCustomAdvanced",
		Inputs{{"noise", link("noise", 0)}, {"guider", link("guider", 0)}, {"sampler", link("sampler", 0)},
			{"sigmas", link("sigmas", 0)}, {"latent_image", link("cond", 1)}}})
	g.Set("dec_v", Node{"VAEDecode", Inputs{{"samples", link("sample", 0)}, {"vae", link("vae_v", 0)}}})
	g.Set("dec_a", Node{"VAEDecodeAudio", Inputs{{"samples", link("sample", 0)}, {"vae", link("vae_a", 0)}}})
	g.Set("video", Node{"CreateVideo", Inputs{{"images", link("dec_v", 0)}, {"fps", FPS}, {"audio", link("dec_a", 0)}}})
	g.Set("save", Node{"SaveVideo",
		Inputs{{"video", link("video", 0)}, {"filename_prefix", spec.FilenamePrefix},
			{"format", "mp4"}, {"codec", "h264"}}})
	return g
}
