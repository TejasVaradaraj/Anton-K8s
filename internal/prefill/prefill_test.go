package prefill

import (
	"math"
	"testing"
)

// Qwen2.5-7B on an L4 with vLLM 0.33 (bench/calibration_vllm_qwen7b.json).
const qwen7bL4 = "1000:0.2475,2000:0.4922,4000:1.0277,8000:2.1659,16000:4.8260,32000:11.7153"

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestCurvePassesThroughMeasuredPoints(t *testing.T) {
	c := MustParse(qwen7bL4)
	for n, want := range map[int64]float64{1000: 0.2475, 8000: 2.1659, 32000: 11.7153} {
		if got := c.Seconds(n); !near(got, want, 1e-9) {
			t.Errorf("Seconds(%d) = %v, want %v", n, got, want)
		}
	}
	if got := c.Seconds(12000); !near(got, (2.1659+4.8260)/2, 1e-9) {
		t.Errorf("midpoint interpolation: %v", got)
	}
	if c.Seconds(0) != 0 || c.Seconds(-5) != 0 || !near(c.Seconds(500), 0.2475/2, 1e-9) {
		t.Errorf("below the first point the curve runs from the origin")
	}
}

func TestCurveIsSuperlinearAndExtrapolatesUpward(t *testing.T) {
	c := MustParse(qwen7bL4)
	if c.c <= 0 {
		t.Fatalf("expected a positive quadratic term, got b=%g c=%g", c.b, c.c)
	}
	perTok := func(n int64) float64 { return c.Seconds(n) / float64(n) }
	if !(perTok(1000) < perTok(16000) && perTok(16000) < perTok(32000) && perTok(32000) < perTok(48000)) {
		t.Fatalf("cost per token should rise with context: %g %g %g %g", perTok(1000), perTok(16000), perTok(32000), perTok(48000))
	}
	// Continuous at the last measured point.
	if !near(c.Seconds(32001)-c.Seconds(32000), c.Seconds(32000)-c.Seconds(31999), 1e-4) {
		t.Fatalf("extrapolation jumps at the last point")
	}
}

func TestCostOfSuffixIncludesAttentionOverPrefix(t *testing.T) {
	c := MustParse(qwen7bL4)
	// 1,000 new tokens on top of 15,000 cached cost more than 1,000 from scratch.
	if suffix, fresh := c.Cost(16000, 15000), c.Seconds(1000); suffix <= fresh {
		t.Fatalf("suffix %g should exceed fresh %g", suffix, fresh)
	}
	if c.Cost(8000, 8000) != 0 || c.Cost(8000, 9000) != 0 {
		t.Fatal("fully cached context costs nothing")
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	for _, s := range []string{"1000", "a:1", "1000:-1", "2000:1,1000:2", "1000:1,1000:2"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted bad input", s)
		}
	}
	if c, err := Parse(""); c != nil || err != nil {
		t.Errorf("empty curve should be nil, nil")
	}
}
