package inventory

import "testing"

func TestDetectQuant(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"tq3_4s model label", "Qwen3.6-27B-TQ3_4S.gguf", "tq3_4s"},
		{"tq3_1s model label", "Qwen3.5-27B-TQ3_1S.gguf", "tq3_1s"},
		{"q4_k_m", "qwen2.5-coder-7b.Q4_K_M.gguf", "q4_k_m"},
		{"q8_0", "GLM-OCR-Q8_0.gguf", "q8_0"},
		{"q6_k", "Qwen3-VL-Embedding-8B-Q6_K.gguf", "q6_k"},
		{"q2_k", "Qwen3-VL-32B-Instruct-Q2_K.gguf", "q2_k"},
		{"bf16", "mmproj-BF16.gguf", "bf16"},
		{"f16", "mmproj-F16.gguf", "f16"},
		{"q4_k_m dotted", "nomic-embed-text-v2-moe.Q4_K_M.gguf", "q4_k_m"},
		{"iq4_xs", "model-IQ4_XS.gguf", "iq4_xs"},
		{"iq2_xxs", "model-IQ2_XXS.gguf", "iq2_xxs"},
		{"q5_k_s", "model-Q5_K_S.gguf", "q5_k_s"},
		{"legacy q4_0", "model.Q4_0.gguf", "q4_0"},
		{"tq3_0 base type", "kv-tq3_0-test.gguf", "tq3_0"},
		{"awq safetensors", "some-model-AWQ", "awq"},
		{"gptq safetensors", "some-model-GPTQ", "gptq"},
		{"lowercase input", "model-q4_k_m.gguf", "q4_k_m"},
		{"unknown -> empty", "random-model-name.gguf", ""},
		{"no quant token", "llama.gguf", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectQuant(tc.in); got != tc.want {
				t.Errorf("DetectQuant(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseShard(t *testing.T) {
	cases := []struct {
		in       string
		idx, tot int
		isShard  bool
	}{
		{"model-00001-of-00008", 1, 8, true},
		{"model-00003-of-00008", 3, 8, true},
		{"model-00010-of-00010", 10, 10, true},
		{"plain-model", 0, 0, false},
		{"model-1-of-8", 0, 0, false}, // not zero-padded 5 digits → not a shard marker
	}
	for _, tc := range cases {
		idx, tot, ok := parseShard(tc.in)
		if ok != tc.isShard || idx != tc.idx || tot != tc.tot {
			t.Errorf("parseShard(%q) = (%d,%d,%v), want (%d,%d,%v)",
				tc.in, idx, tot, ok, tc.idx, tc.tot, tc.isShard)
		}
	}
}
