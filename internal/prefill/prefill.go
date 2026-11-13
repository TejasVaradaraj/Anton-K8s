// Package prefill turns measured prefill timings into a cost function.
//
// Prefill time grows faster than linearly with context (attention is
// quadratic in sequence length, the MLP is linear), so a single ms-per-token
// rate misprices long contexts. A Curve is built from measured
// (tokens, seconds) points, e.g. bench/calibration_vllm_qwen7b.json:
//
//   - inside the measured range it interpolates linearly between points;
//   - beyond the last point it follows a least-squares fit of
//     seconds = b*n + c*n^2 (c >= 0), continued from the last point.
//
// Computing only the uncached suffix of a context costs Cost(total, cached) =
// Seconds(total) - Seconds(cached): the new tokens attend over the whole
// context, which is what makes long-context recompute expensive.
package prefill

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type point struct {
	tokens  float64
	seconds float64
}

type Curve struct {
	pts  []point // sorted by tokens, starting at (0, 0)
	b, c float64 // fit used beyond the last point
}

// Parse reads "1000:0.2475,2000:0.4922,..." (tokens:seconds pairs).
func Parse(s string) (*Curve, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var pts []point
	for _, part := range strings.Split(s, ",") {
		tok, sec, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			return nil, fmt.Errorf("prefill curve: %q is not tokens:seconds", part)
		}
		n, err1 := strconv.ParseFloat(tok, 64)
		t, err2 := strconv.ParseFloat(sec, 64)
		if err1 != nil || err2 != nil || n <= 0 || t <= 0 {
			return nil, fmt.Errorf("prefill curve: bad point %q", part)
		}
		pts = append(pts, point{n, t})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].tokens < pts[j].tokens })
	for i := 1; i < len(pts); i++ {
		if pts[i].tokens == pts[i-1].tokens || pts[i].seconds < pts[i-1].seconds {
			return nil, fmt.Errorf("prefill curve: points must be strictly increasing in tokens and non-decreasing in time")
		}
	}
	c := &Curve{pts: append([]point{{0, 0}}, pts...)}
	c.fit(pts)
	return c, nil
}

// MustParse is Parse for constants and flags known to be valid.
func MustParse(s string) *Curve {
	c, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return c
}

// fit solves least squares for seconds = b*n + c*n^2 (no intercept), and
// falls back to a linear fit if the quadratic term comes out negative.
func (c *Curve) fit(pts []point) {
	var s11, s12, s22, y1, y2 float64
	for _, p := range pts {
		n := p.tokens
		s11 += n * n
		s12 += n * n * n
		s22 += n * n * n * n
		y1 += n * p.seconds
		y2 += n * n * p.seconds
	}
	det := s11*s22 - s12*s12
	if det != 0 {
		c.b = (y1*s22 - y2*s12) / det
		c.c = (s11*y2 - s12*y1) / det
	}
	if det == 0 || c.c < 0 || c.b < 0 {
		c.b, c.c = y1/s11, 0
	}
}

// Seconds is the prefill time for a context of n tokens from scratch.
func (c *Curve) Seconds(n int64) float64 {
	if n <= 0 {
		return 0
	}
	x := float64(n)
	last := c.pts[len(c.pts)-1]
	if x > last.tokens {
		model := func(v float64) float64 { return c.b*v + c.c*v*v }
		return last.seconds + model(x) - model(last.tokens)
	}
	i := sort.Search(len(c.pts), func(i int) bool { return c.pts[i].tokens >= x })
	lo, hi := c.pts[i-1], c.pts[i]
	return lo.seconds + (x-lo.tokens)/(hi.tokens-lo.tokens)*(hi.seconds-lo.seconds)
}

// Cost is the prefill time for a context of total tokens when the first
// cached tokens are already in the KV cache.
func (c *Curve) Cost(total, cached int64) float64 {
	if cached >= total {
		return 0
	}
	if cached < 0 {
		cached = 0
	}
	return c.Seconds(total) - c.Seconds(cached)
}

// String renders the curve back in Parse's format (without the origin).
func (c *Curve) String() string {
	var parts []string
	for _, p := range c.pts[1:] {
		parts = append(parts, fmt.Sprintf("%g:%g", p.tokens, p.seconds))
	}
	return strings.Join(parts, ",")
}
