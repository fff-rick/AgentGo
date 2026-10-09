package main

import (
	"context"
	"testing"

	"github.com/enterprise/ai-agent-go/internal/laya"
)

type predictorStub struct {
	request laya.Request
}

func (p *predictorStub) Predict(_ context.Context, decision string, request laya.Request) (*laya.Response, error) {
	p.request = request
	passed, low := "passed", false
	abstained, yes := "abstained", true
	return &laya.Response{Answers: map[string]laya.Answer{
		"primary_tool":   {Choice: "calculator", AnswerConfidence: number(.8), Abstention: passed, LowConfidence: &low},
		"secondary_tool": {Choice: "web_search", AnswerConfidence: number(.7), Abstention: abstained, LowConfidence: &yes},
	}, Routing: laya.Routing{Model: "multilingual"}}, nil
}

func TestEvaluateKeepsRawDecisionAndMarksAbstention(t *testing.T) {
	client := &predictorStub{}
	result := evaluate(context.Background(), client, caseItem{ID: "x", Request: "calculate", ExpectedTools: []string{"calculator"}, ForbiddenTools: []string{"web_search"}},
		[]catalogEntry{{Name: "calculator", Description: "math", Selectable: true}, {Name: "web_search", Description: "web", Selectable: true}, {Name: "file_edit_apply", Description: "apply"}}, .8)
	if result.Covered || len(result.ActualTools) != 2 || len(result.LowConfidence) != 1 || result.LowConfidence[0] != "secondary_tool" || len(result.ForbiddenSelected) != 1 || result.Checkpoint != "multilingual" {
		t.Fatalf("result=%+v", result)
	}
	if client.request.MinConfidence == nil || *client.request.MinConfidence != .8 || len(client.request.Questions) != 2 {
		t.Fatalf("request=%+v", client.request)
	}
	criteria := client.request.Questions["primary_tool"].Criteria.(map[string]string)
	if _, ok := criteria["file_edit_apply"]; ok {
		t.Fatal("file_edit_apply must not be selectable")
	}
}

type passedWithoutLowConfidenceStub struct{}

func (passedWithoutLowConfidenceStub) Predict(_ context.Context, _ string, _ laya.Request) (*laya.Response, error) {
	return &laya.Response{Answers: map[string]laya.Answer{
		"primary_tool":   {Choice: "calculator", Abstention: "passed"},
		"secondary_tool": {Choice: "none", Abstention: "passed"},
	}}, nil
}

func TestEvaluateTreatsPassedAnswerWithoutLowConfidenceAsCovered(t *testing.T) {
	result := evaluate(context.Background(), passedWithoutLowConfidenceStub{}, caseItem{ID: "x", Request: "calculate", ExpectedTools: []string{"calculator"}},
		[]catalogEntry{{Name: "calculator", Description: "math", Selectable: true}}, .6)
	if !result.Covered || len(result.LowConfidence) != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestSummarizeReportsRawAndGatedQuality(t *testing.T) {
	catalog := []catalogEntry{{Name: "calculator", Selectable: true}, {Name: "web_search", Selectable: true}, {Name: "database_query", Selectable: true}}
	results := []caseResult{
		{ID: "correct", ExpectedTools: []string{"calculator"}, ActualTools: []string{"calculator"}, Covered: true, LatencyMS: 10},
		{ID: "abstained", ExpectedTools: []string{"web_search"}, Covered: false, LatencyMS: 20},
		{ID: "forbidden", ActualTools: []string{"database_query"}, ForbiddenSelected: []string{"database_query"}, Covered: true, LatencyMS: 30},
	}
	got := summarize(results, catalog)
	if got.ExactMatchRate != 1.0/3 || got.MicroPrecision != .5 || got.MicroRecall != .5 || got.MicroF1 != .5 {
		t.Fatalf("raw summary=%+v", got)
	}
	if got.Coverage != 2.0/3 || got.CoveredExactMatchRate != .5 || got.EffectivePassRate != 1.0/3 || got.ForbiddenSelections != 1 {
		t.Fatalf("gated summary=%+v", got)
	}
	if got.LatencyP50MS != 20 || got.LatencyP95MS != 30 || got.ScoresByTool["calculator"].F1 != 1 {
		t.Fatalf("latency/tool summary=%+v", got)
	}
}

func TestSortedDeduplicatesRepeatedPrimaryAndSecondaryChoice(t *testing.T) {
	got := sorted([]string{"calculator", "calculator"})
	if len(got) != 1 || got[0] != "calculator" {
		t.Fatalf("sorted=%v", got)
	}
	if empty := sorted(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("empty=%#v, want non-nil empty slice", empty)
	}
}

func TestValidateCasesRejectsUnsafeLabels(t *testing.T) {
	catalog := []catalogEntry{{Name: "file_edit_apply", Description: "apply", Selectable: false}}
	err := validateCases([]caseItem{{ID: "x", Request: "edit", ExpectedTools: []string{"file_edit_apply"}, Difficulty: "easy", Source: "seed"}}, catalog)
	if err == nil {
		t.Fatal("expected overlapping expected/forbidden error")
	}
}

func TestSeedDatasetIsValidAndBalancedEnoughForSmokeEvaluation(t *testing.T) {
	catalog, err := readCatalog("../../benchmarks/datasets/laya-tool-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	cases, err := readCases("../../benchmarks/datasets/laya-tool-selection.seed.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCases(cases, catalog); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 24 {
		t.Fatalf("seed cases=%d, want 24", len(cases))
	}
	support := map[string]int{}
	noTool := 0
	for _, item := range cases {
		if len(item.ExpectedTools) == 0 {
			noTool++
		}
		for _, name := range item.ExpectedTools {
			support[name]++
		}
	}
	if noTool < 5 {
		t.Fatalf("no-tool cases=%d", noTool)
	}
	for _, name := range []string{"calculator", "database_query", "file_edit_preview", "file_inspect", "knowledge_search", "web_search"} {
		if support[name] < 2 {
			t.Fatalf("tool %s support=%d", name, support[name])
		}
	}
}

func number(value float64) *float64 { return &value }
