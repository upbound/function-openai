/*
Copyright 2025 The Upbound Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
)

func TestRunFunction(t *testing.T) {

	type args struct {
		ctx context.Context
		req *fnv1.RunFunctionRequest
		ai  agentInvoker
	}
	type want struct {
		rsp *fnv1.RunFunctionResponse
		err error
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"IgnoredResource": {
			reason: "We should return early if the incoming resource should be ignored.",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Context: &structpb.Struct{Fields: map[string]*structpb.Value{
						"ops.upbound.io/ignored-resource": structpb.NewBoolValue(true),
					}},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Context: &structpb.Struct{Fields: map[string]*structpb.Value{
						"ops.upbound.io/ignored-resource": structpb.NewBoolValue(true),
					}},
					Meta: &fnv1.ResponseMeta{
						Ttl: &durationpb.Duration{
							Seconds: 60,
						},
					},
					Conditions: []*fnv1.Condition{
						{
							Type:   "FunctionSuccess",
							Status: fnv1.Status_STATUS_CONDITION_TRUE,
							Reason: "Success",
							Target: fnv1.Target_TARGET_COMPOSITE_AND_CLAIM.Enum(),
						},
					},
					Results: []*fnv1.Result{
						{
							Severity: fnv1.Severity_SEVERITY_NORMAL,
							Message:  "received an ignored resource, skipping",
							Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
						},
					},
				},
			},
		},
		"ResponseIsReturned": {
			reason: "The Function should return a fatal result if credential cannot be found.",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "hello"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "openai.fn.upbound.io/v1alpha1",
						"kind": "Prompt",
						"systemPrompt": "I'm a system",
						"userPrompt": "I'm a user"
					}`),
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "hello", Ttl: durationpb.New(response.DefaultTTL)},
					Results: []*fnv1.Result{
						{
							Severity: fnv1.Severity_SEVERITY_FATAL,
							Message:  `cannot get OPENAI_API_KEY from credential "gpt": gpt: credential not found`,
							Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
						},
					},
				},
				err: cmpopts.AnyError,
			},
		},
		"SimpleCompositionPipeline": {
			reason: "We should go through the composition pipeline without error.",
			args: args{
				ai: &mockAgentInvoker{
					InvokeFn: func(_ context.Context, _, _, _, _, _ string, _ int) (string, error) {
						return `---
apiVersion: some.group/v1
metadata:
  name: some-name
  annotations:
    upbound.io/name: some-name
`, nil
					},
				},
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "hello"},
					Input: resource.MustStructJSON(`{
								"apiVersion": "openai.fn.upbound.io/v1alpha1",
								"kind": "Prompt",
								"systemPrompt": "I'm a system",
								"userPrompt": "I'm a user"
							}`),
					Credentials: mockCredentials(),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: &structpb.Struct{
								Fields: map[string]*structpb.Value{},
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{},
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "hello", Ttl: durationpb.New(response.DefaultTTL)},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{},
						Resources: map[string]*fnv1.Resource{
							"some-name": {
								Resource: &structpb.Struct{
									Fields: map[string]*structpb.Value{
										"apiVersion": {
											Kind: &structpb.Value_StringValue{
												StringValue: "some.group/v1",
											},
										},
										"metadata": {
											Kind: &structpb.Value_StructValue{
												StructValue: &structpb.Struct{
													Fields: map[string]*structpb.Value{
														"annotations": {
															Kind: &structpb.Value_StructValue{
																StructValue: &structpb.Struct{
																	Fields: map[string]*structpb.Value{
																		"upbound.io/name": {
																			Kind: &structpb.Value_StringValue{
																				StringValue: "some-name",
																			},
																		},
																	},
																},
															},
														},
														"name": {
															Kind: &structpb.Value_StringValue{
																StringValue: "some-name",
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		"OperationPipelineAppliesParseableDecision": {
			reason: "A parseable resource from the model should be returned as a desired resource.",
			args: args{
				ai: &mockAgentInvoker{
					InvokeFn: func(_ context.Context, _, _, _, _, _ string, _ int) (string, error) {
						return `{"apiVersion":"example.org/v1","kind":"Thing","metadata":{"name":"some-name"}}`, nil
					},
				},
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "hello"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "openai.fn.upbound.io/v1alpha1",
						"kind": "Prompt",
						"systemPrompt": "I'm a system",
						"userPrompt": "I'm a user"
					}`),
					Credentials: mockCredentials(),
					RequiredResources: map[string]*fnv1.Resources{
						"ops.crossplane.io/watched-resource": {
							Items: []*fnv1.Resource{{Resource: resource.MustStructJSON(
								`{"apiVersion":"example.org/v1","kind":"Thing","metadata":{"name":"some-name"}}`)}},
						},
					},
					Desired: &fnv1.State{},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "hello", Ttl: &durationpb.Duration{Seconds: 60}},
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_NORMAL,
						Message:  `{"apiVersion":"example.org/v1","kind":"Thing","metadata":{"name":"some-name"}}`,
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
					Conditions: []*fnv1.Condition{{
						Type:   "FunctionSuccess",
						Status: fnv1.Status_STATUS_CONDITION_TRUE,
						Reason: "Success",
						Target: fnv1.Target_TARGET_COMPOSITE_AND_CLAIM.Enum(),
					}},
					Desired: &fnv1.State{
						Resources: map[string]*fnv1.Resource{
							"some-name": {
								Resource: resource.MustStructJSON(`{"apiVersion":"example.org/v1","kind":"Thing","metadata":{"name":"some-name"}}`),
							},
						},
					},
				},
			},
		},
		"OperationPipelineStripsCodeFence": {
			reason: "A JSON reply wrapped in a markdown fence must still be applied. The operation path used to skip fence stripping entirely, so a fenced reply was discarded silently.",
			args: args{
				ai: &mockAgentInvoker{
					InvokeFn: func(_ context.Context, _, _, _, _, _ string, _ int) (string, error) {
						return "```json\n{\"apiVersion\":\"example.org/v1\",\"kind\":\"Thing\",\"metadata\":{\"name\":\"fenced\"}}\n```", nil
					},
				},
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "hello"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "openai.fn.upbound.io/v1alpha1",
						"kind": "Prompt",
						"systemPrompt": "I'm a system",
						"userPrompt": "I'm a user"
					}`),
					Credentials: mockCredentials(),
					RequiredResources: map[string]*fnv1.Resources{
						"ops.crossplane.io/watched-resource": {
							Items: []*fnv1.Resource{{Resource: resource.MustStructJSON(
								`{"apiVersion":"example.org/v1","kind":"Thing","metadata":{"name":"fenced"}}`)}},
						},
					},
					Desired: &fnv1.State{},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "hello", Ttl: &durationpb.Duration{Seconds: 60}},
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_NORMAL,
						Message:  "```json\n{\"apiVersion\":\"example.org/v1\",\"kind\":\"Thing\",\"metadata\":{\"name\":\"fenced\"}}\n```",
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
					Conditions: []*fnv1.Condition{{
						Type:   "FunctionSuccess",
						Status: fnv1.Status_STATUS_CONDITION_TRUE,
						Reason: "Success",
						Target: fnv1.Target_TARGET_COMPOSITE_AND_CLAIM.Enum(),
					}},
					Desired: &fnv1.State{
						Resources: map[string]*fnv1.Resource{
							"fenced": {
								Resource: resource.MustStructJSON(`{"apiVersion":"example.org/v1","kind":"Thing","metadata":{"name":"fenced"}}`),
							},
						},
					},
				},
			},
		},
		"OperationPipelineNoOpWritesNothing": {
			reason: "An empty reply must produce no desired resources, so a WatchOperation-driven pipeline does not re-trigger itself forever.",
			args: args{
				ai: &mockAgentInvoker{
					InvokeFn: func(_ context.Context, _, _, _, _, _ string, _ int) (string, error) {
						return "  \n ", nil
					},
				},
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "hello"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "openai.fn.upbound.io/v1alpha1",
						"kind": "Prompt",
						"systemPrompt": "I'm a system",
						"userPrompt": "I'm a user"
					}`),
					Credentials: mockCredentials(),
					RequiredResources: map[string]*fnv1.Resources{
						"ops.crossplane.io/watched-resource": {
							Items: []*fnv1.Resource{{Resource: &structpb.Struct{}}},
						},
					},
					Desired: &fnv1.State{},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "hello", Ttl: &durationpb.Duration{Seconds: 60}},
					Conditions: []*fnv1.Condition{{
						Type:   "FunctionSuccess",
						Status: fnv1.Status_STATUS_CONDITION_TRUE,
						Reason: "NoOp",
						Target: fnv1.Target_TARGET_COMPOSITE_AND_CLAIM.Enum(),
					}},
					Desired: &fnv1.State{},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &Function{log: logging.NewNopLogger(), ai: tc.args.ai}
			rsp, err := f.RunFunction(tc.args.ctx, tc.args.req)

			if diff := cmp.Diff(tc.want.rsp, rsp, protocmp.Transform()); diff != "" {
				t.Errorf("%s\nf.RunFunction(...): -want rsp, +got rsp:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.err, err, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("%s\nf.RunFunction(...): -want err, +got err:\n%s", tc.reason, diff)
			}
		})
	}
}

func mockCredentials() map[string]*fnv1.Credentials {
	return map[string]*fnv1.Credentials{
		"gpt": {
			Source: &fnv1.Credentials_CredentialData{
				CredentialData: &fnv1.CredentialData{
					Data: map[string][]byte{
						"OPENAI_API_KEY": []byte("data"),
					},
				},
			},
		},
	}
}

type mockAgentInvoker struct {
	InvokeFn func(ctx context.Context, key, system, prompt, baseURL, modelName string, maxTokens int) (string, error)
}

func (m *mockAgentInvoker) Invoke(ctx context.Context, key, system, prompt, baseURL, modelName string, maxTokens int) (string, error) {
	return m.InvokeFn(ctx, key, system, prompt, baseURL, modelName, maxTokens)
}

// TestOperationPipelineDoesNotEscapePrompt asserts that the watched resource is
// interpolated into the prompt as raw JSON.
//
// This function used to build prompts with html/template rather than
// text/template, which HTML-escapes every interpolated value. The watched
// resource therefore reached the model with each " turned into &#34;, leaving it
// unparseable as JSON and about 1.3x larger for nothing. Nothing failed loudly:
// the model simply received mangled input and answered badly.
//
// The whole existing suite passed both before and after that fix, because no
// test looked at the prompt. This one does.
func TestOperationPipelineDoesNotEscapePrompt(t *testing.T) {
	var gotPrompt string
	f := &Function{
		log: logging.NewNopLogger(),
		ai: &mockAgentInvoker{
			InvokeFn: func(_ context.Context, _, _, prompt, _, _ string, _ int) (string, error) {
				gotPrompt = prompt
				return "", nil
			},
		},
	}

	watched := resource.MustStructJSON(`{
		"apiVersion": "example.org/v1",
		"kind": "Thing",
		"metadata": {"name": "a-thing", "namespace": "a-namespace"},
		"spec": {"nested": {"value": "quoted"}}
	}`)

	if _, err := f.RunFunction(context.Background(), &fnv1.RunFunctionRequest{
		Meta: &fnv1.RequestMeta{Tag: "hello"},
		Input: resource.MustStructJSON(`{
			"apiVersion": "openai.fn.upbound.io/v1alpha1",
			"kind": "Prompt",
			"systemPrompt": "sys",
			"userPrompt": "here is the resource:\n{{ .Resources }}"
		}`),
		Credentials: mockCredentials(),
		RequiredResources: map[string]*fnv1.Resources{
			"ops.crossplane.io/watched-resource": {
				Items: []*fnv1.Resource{{Resource: watched}},
			},
		},
		Desired: &fnv1.State{},
	}); err != nil {
		t.Fatalf("f.RunFunction(...): unexpected error: %v", err)
	}

	if gotPrompt == "" {
		t.Fatal("f.RunFunction(...): the invoker was never called, so no prompt was captured")
	}

	// The specific corruption html/template produced.
	for _, entity := range []string{"&#34;", "&quot;", "&amp;", "&lt;", "&gt;", "&#39;"} {
		if strings.Contains(gotPrompt, entity) {
			t.Errorf("f.RunFunction(...): prompt contains HTML entity %q, so the resource "+
				"was escaped rather than interpolated raw. Check that fn.go imports "+
				"text/template and not html/template.\nprompt: %s", entity, gotPrompt)
		}
	}

	// Positive assertion: the interpolated resource must still be parseable
	// JSON, which is the property the model actually depends on.
	i := strings.Index(gotPrompt, "{")
	if i < 0 {
		t.Fatalf("f.RunFunction(...): no JSON object in the prompt: %s", gotPrompt)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(gotPrompt[i:]), &parsed); err != nil {
		t.Fatalf("f.RunFunction(...): the interpolated resource is not parseable JSON "+
			"(%v)\nprompt: %s", err, gotPrompt)
	}
	if got := parsed["kind"]; got != "Thing" {
		t.Errorf("f.RunFunction(...): want kind Thing in the interpolated resource, got %v", got)
	}
}

// TestOperationPipelineUnparseableOutput asserts that non-empty output the
// function cannot parse is reported as a failure rather than as success.
//
// It previously logged at Debug and still set FunctionSuccess=True with no
// desired resources, so a model drifting off-format looked exactly like one
// that had decided to do nothing. Asserts the reason and severity, not the
// parser's message text, which is protojson's to change.
func TestOperationPipelineUnparseableOutput(t *testing.T) {
	f := &Function{
		log: logging.NewNopLogger(),
		ai: &mockAgentInvoker{
			InvokeFn: func(_ context.Context, _, _, _, _, _ string, _ int) (string, error) {
				return "I think you should probably scale this up.", nil
			},
		},
	}

	rsp, err := f.RunFunction(context.Background(), operationRequest())
	if err != nil {
		t.Fatalf("f.RunFunction(...): unexpected error: %v", err)
	}
	if got := len(rsp.GetConditions()); got != 1 {
		t.Fatalf("f.RunFunction(...): want 1 condition, got %d", got)
	}
	c := rsp.GetConditions()[0]
	if diff := cmp.Diff("UnparseableModelOutput", c.GetReason()); diff != "" {
		t.Errorf("f.RunFunction(...): -want reason, +got:\n%s", diff)
	}
	if c.GetStatus() != fnv1.Status_STATUS_CONDITION_FALSE {
		t.Errorf("f.RunFunction(...): want FunctionSuccess=False, got %v", c.GetStatus())
	}
	if c.GetMessage() == "" {
		t.Error("f.RunFunction(...): want a message explaining the parse failure, got none")
	}
	if got := len(rsp.GetResults()); got != 1 ||
		rsp.GetResults()[0].GetSeverity() != fnv1.Severity_SEVERITY_WARNING {
		t.Errorf("f.RunFunction(...): want one warning event, got %v", rsp.GetResults())
	}
	// The critical property: nothing is written on a failure.
	if got := len(rsp.GetDesired().GetResources()); got != 0 {
		t.Errorf("f.RunFunction(...): want no desired resources, got %d", got)
	}
}

// TestMaxTokensIsAlwaysCapped asserts a generation cap always reaches the model.
//
// Without one, a self-hosted OpenAI-compatible engine generates until its
// context window is exhausted - llama.cpp reports "decode() failed: Context
// size has been exceeded" and the function blocks until the request ends, which
// on CPU inference hangs an Operation for minutes. Hosted APIs supply their own
// default, which is why this was invisible.
func TestMaxTokensIsAlwaysCapped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  int
	}{
		{name: "DefaultsWhenUnset", input: "", want: defaultMaxTokens},
		{name: "DefaultsWhenZero", input: `"maxTokens": 0,`, want: defaultMaxTokens},
		{name: "HonoursExplicitValue", input: `"maxTokens": 64,`, want: 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got int
			f := &Function{
				log: logging.NewNopLogger(),
				ai: &mockAgentInvoker{
					InvokeFn: func(_ context.Context, _, _, _, _, _ string, maxTokens int) (string, error) {
						got = maxTokens
						return "", nil
					},
				},
			}
			req := operationRequest()
			req.Input = resource.MustStructJSON(`{
				"apiVersion": "openai.fn.upbound.io/v1alpha1",
				"kind": "Prompt",
				` + tc.input + `
				"systemPrompt": "sys",
				"userPrompt": "user"
			}`)
			if _, err := f.RunFunction(context.Background(), req); err != nil {
				t.Fatalf("f.RunFunction(...): unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("f.RunFunction(...): want maxTokens %d reaching the model, got %d",
					tc.want, got)
			}
			if got == 0 {
				t.Error("f.RunFunction(...): maxTokens must never be 0 - an uncapped " +
					"request lets a self-hosted engine generate to context exhaustion")
			}
		})
	}
}

// TestStripCodeFence covers the fences models actually produce. The previous
// implementation handled only "```yaml", so a model asked for JSON - which
// naturally reaches for "```json" - had its reply discarded.
func TestStripCodeFence(t *testing.T) {
	want := `{"a":1}`
	for _, tc := range []struct{ name, in string }{
		{"Bare", `{"a":1}`},
		{"Whitespace", "  \n{\"a\":1}\n  "},
		{"JSONFence", "```json\n{\"a\":1}\n```"},
		{"YAMLFence", "```yaml\n{\"a\":1}\n```"},
		{"YMLFence", "```yml\n{\"a\":1}\n```"},
		{"UnlabelledFence", "```\n{\"a\":1}\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(want, stripCodeFence(tc.in)); diff != "" {
				t.Errorf("stripCodeFence(%q): -want, +got:\n%s", tc.in, diff)
			}
		})
	}
}

// operationRequest returns a minimal operations-pipeline request with one
// watched resource, which is what function-openai requires.
func operationRequest() *fnv1.RunFunctionRequest {
	return &fnv1.RunFunctionRequest{
		Meta: &fnv1.RequestMeta{Tag: "hello"},
		Input: resource.MustStructJSON(`{
			"apiVersion": "openai.fn.upbound.io/v1alpha1",
			"kind": "Prompt",
			"systemPrompt": "sys",
			"userPrompt": "user"
		}`),
		Credentials: mockCredentials(),
		RequiredResources: map[string]*fnv1.Resources{
			"ops.crossplane.io/watched-resource": {
				Items: []*fnv1.Resource{{Resource: resource.MustStructJSON(watchedJSON)}},
			},
		},
		Desired: &fnv1.State{},
	}
}

// watchedJSON is the resource the operation-path tests are asked about. Shaped
// like the real SQLInstance the scaling WatchOperation watches, because the
// identity guard compares against it.
const watchedJSON = `{
	"apiVersion": "aws.platform.upbound.io/v1alpha1",
	"kind": "SQLInstance",
	"metadata": {"name": "solo-1", "namespace": "database-team"},
	"spec": {"parameters": {"instanceClass": "db.t3.micro"}}
}`

// TestOperationPipelineIdentityGuard asserts the function refuses to patch any
// resource other than the one it was asked about.
//
// This is not hypothetical. Running the scaling WatchOperation against a
// self-hosted CPU engine, the model copied a few-shot example's name and
// namespace straight out of the prompt instead of reading the watched
// resource - 5 of 8 Operations did it under 8-way concurrency, 0 of 2
// sequentially, the trigger being a KV cache oversubscribed by concurrent
// requests. Crossplane server-side-applies whatever comes back, so on a fleet
// where "team-y/db-d" exists that patch resizes another team's database. It
// failed safe on the test rig only because that namespace did not exist.
//
// The guard therefore cannot depend on prompt wording, model size or engine
// capacity. The version is deliberately NOT part of the comparison: two
// versions of one kind address the same object.
func TestOperationPipelineIdentityGuard(t *testing.T) {
	// The literal reply observed in production, verbatim from example 4 of
	// operations/rds-intelligent-scaling-watch/operation.yaml.
	const regurgitatedExample = `{"apiVersion":"aws.platform.upbound.io/v1alpha1",` +
		`"kind":"SQLInstance","metadata":{"name":"db-d","namespace":"team-y",` +
		`"annotations":{"intelligent-scaling/reasoning":"CPU 93% sustained above 80%"}},` +
		`"spec":{"parameters":{"instanceClass":"db.t3.small"}}}`

	for _, tc := range []struct {
		name      string
		reply     string
		wantApply bool
	}{
		{
			name:      "RegurgitatedFewShotExampleIsRefused",
			reply:     regurgitatedExample,
			wantApply: false,
		},
		{
			name: "ForeignNamespaceAloneIsRefused",
			reply: `{"apiVersion":"aws.platform.upbound.io/v1alpha1","kind":"SQLInstance",` +
				`"metadata":{"name":"solo-1","namespace":"team-y"},` +
				`"spec":{"parameters":{"instanceClass":"db.t3.small"}}}`,
			wantApply: false,
		},
		{
			// A model that returns only a spec, with no identity at all, must
			// not have it applied to the watched resource by default.
			name: "NamelessReplyIsRefused",
			reply: `{"apiVersion":"aws.platform.upbound.io/v1alpha1","kind":"SQLInstance",` +
				`"spec":{"parameters":{"instanceClass":"db.t3.small"}}}`,
			wantApply: false,
		},
		{
			name: "ForeignKindIsRefused",
			reply: `{"apiVersion":"aws.platform.upbound.io/v1alpha1","kind":"Bucket",` +
				`"metadata":{"name":"solo-1","namespace":"database-team"}}`,
			wantApply: false,
		},
		{
			name: "WatchedResourceIsApplied",
			reply: `{"apiVersion":"aws.platform.upbound.io/v1alpha1","kind":"SQLInstance",` +
				`"metadata":{"name":"solo-1","namespace":"database-team"},` +
				`"spec":{"parameters":{"instanceClass":"db.t3.small"}}}`,
			wantApply: true,
		},
		{
			// Negative control for over-strictness: a different version of the
			// same kind is the same object and must still be applied.
			name: "DifferentVersionSameObjectIsApplied",
			reply: `{"apiVersion":"aws.platform.upbound.io/v1beta1","kind":"SQLInstance",` +
				`"metadata":{"name":"solo-1","namespace":"database-team"},` +
				`"spec":{"parameters":{"instanceClass":"db.t3.small"}}}`,
			wantApply: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &Function{
				log: logging.NewNopLogger(),
				ai: &mockAgentInvoker{
					InvokeFn: func(_ context.Context, _, _, _, _, _ string, _ int) (string, error) {
						return tc.reply, nil
					},
				},
			}

			rsp, err := f.RunFunction(context.Background(), operationRequest())
			if err != nil {
				t.Fatalf("f.RunFunction(...): unexpected error: %v", err)
			}
			if got := len(rsp.GetConditions()); got != 1 {
				t.Fatalf("f.RunFunction(...): want 1 condition, got %d", got)
			}
			c := rsp.GetConditions()[0]

			if tc.wantApply {
				if diff := cmp.Diff("Success", c.GetReason()); diff != "" {
					t.Errorf("f.RunFunction(...): -want reason, +got:\n%s", diff)
				}
				if got := len(rsp.GetDesired().GetResources()); got != 1 {
					t.Errorf("f.RunFunction(...): want the decision applied (1 desired "+
						"resource), got %d - the guard is rejecting valid output", got)
				}
				return
			}

			if diff := cmp.Diff("ResourceIdentityMismatch", c.GetReason()); diff != "" {
				t.Errorf("f.RunFunction(...): -want reason, +got:\n%s", diff)
			}
			if c.GetStatus() != fnv1.Status_STATUS_CONDITION_FALSE {
				t.Errorf("f.RunFunction(...): want FunctionSuccess=False, got %v", c.GetStatus())
			}
			if c.GetMessage() == "" {
				t.Error("f.RunFunction(...): want a message naming both identities, got none")
			}
			if got := len(rsp.GetResults()); got != 1 ||
				rsp.GetResults()[0].GetSeverity() != fnv1.Severity_SEVERITY_WARNING {
				t.Errorf("f.RunFunction(...): want one warning event, got %v", rsp.GetResults())
			}
			// The property that matters: nothing is written for a resource we
			// were not asked about.
			if got := len(rsp.GetDesired().GetResources()); got != 0 {
				t.Errorf("f.RunFunction(...): want NO desired resources, got %d - a patch "+
					"for a foreign resource would reach the API server", got)
			}
		})
	}
}

func TestResourceIdentitySameObject(t *testing.T) {
	watched := resourceIdentity{
		APIVersion: "aws.platform.upbound.io/v1alpha1",
		Kind:       "SQLInstance",
		Namespace:  "database-team",
		Name:       "solo-1",
	}
	for _, tc := range []struct {
		name  string
		other resourceIdentity
		want  bool
	}{
		{name: "Identical", other: watched, want: true},
		{
			name: "VersionDiffersIsSameObject",
			other: resourceIdentity{APIVersion: "aws.platform.upbound.io/v1beta1",
				Kind: "SQLInstance", Namespace: "database-team", Name: "solo-1"},
			want: true,
		},
		{
			name: "GroupDiffers",
			other: resourceIdentity{APIVersion: "other.example.org/v1alpha1",
				Kind: "SQLInstance", Namespace: "database-team", Name: "solo-1"},
			want: false,
		},
		{
			name: "NameDiffers",
			other: resourceIdentity{APIVersion: "aws.platform.upbound.io/v1alpha1",
				Kind: "SQLInstance", Namespace: "database-team", Name: "db-d"},
			want: false,
		},
		{
			name: "NamespaceDiffers",
			other: resourceIdentity{APIVersion: "aws.platform.upbound.io/v1alpha1",
				Kind: "SQLInstance", Namespace: "team-y", Name: "solo-1"},
			want: false,
		},
		{
			name: "KindDiffers",
			other: resourceIdentity{APIVersion: "aws.platform.upbound.io/v1alpha1",
				Kind: "Bucket", Namespace: "database-team", Name: "solo-1"},
			want: false,
		},
		{
			// A model that omits the namespace must not be treated as a match:
			// the patch would land somewhere other than intended.
			name: "MissingNamespaceIsNotAMatch",
			other: resourceIdentity{APIVersion: "aws.platform.upbound.io/v1alpha1",
				Kind: "SQLInstance", Name: "solo-1"},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.other.sameObject(watched); got != tc.want {
				t.Errorf("sameObject(%s, %s) = %v, want %v", tc.other, watched, got, tc.want)
			}
		})
	}
}

func TestIdentityOfHandlesMalformedContent(t *testing.T) {
	// A model reply need not have the shape we expect. identityOf must not
	// panic on it, and must not silently produce an identity that matches
	// something real.
	for _, tc := range []struct {
		name string
		obj  map[string]any
	}{
		{name: "Empty", obj: map[string]any{}},
		{name: "Nil", obj: nil},
		{name: "MetadataNotAMap", obj: map[string]any{"metadata": "nope"}},
		{name: "NameNotAString", obj: map[string]any{"metadata": map[string]any{"name": 7}}},
		{name: "KindNotAString", obj: map[string]any{"kind": []any{"Thing"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := identityOf(tc.obj)
			if got.Name != "" {
				t.Errorf("identityOf(%v): want empty name from malformed content, got %q",
					tc.obj, got.Name)
			}
		})
	}
}
