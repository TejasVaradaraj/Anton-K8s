package main

import "testing"

func TestCombineByModeSumsCountsAndAveragesLatency(t *testing.T) {
	rs := []result{
		{Mode: "plain", Seed: 1, Requests: 10, Lost: 2, Affected: 5, TTFTP50Ms: 100, Events: []eventResult{{Kind: "drain", Lost: 1, Affected: 2, ResumeP50Ms: 10}}},
		{Mode: "anton", Seed: 1, Requests: 20, Lost: 0, Affected: 4, TTFTP50Ms: 50, Events: []eventResult{{Kind: "drain"}}},
		{Mode: "plain", Seed: 2, Requests: 30, Lost: 4, Affected: 7, TTFTP50Ms: 300, Events: []eventResult{{Kind: "drain", Lost: 3, Affected: 4, ResumeP50Ms: 30}}},
	}
	out := combineByMode(rs)
	if len(out) != 2 || out[0].Mode != "plain" || out[1].Mode != "anton" {
		t.Fatalf("modes/order: %+v", out)
	}
	p := out[0]
	if p.Requests != 40 || p.Lost != 6 || p.Affected != 12 || p.TTFTP50Ms != 200 {
		t.Fatalf("plain combined %+v", p)
	}
	if e := p.Events[0]; e.Lost != 4 || e.Affected != 6 || e.ResumeP50Ms != 20 {
		t.Fatalf("event combined %+v", e)
	}
}
