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
	"fmt"
	"strings"
	"text/template"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tmc/langchaingo/agents"
	"github.com/tmc/langchaingo/chains"
	openaillm "github.com/tmc/langchaingo/llms/openai"
	"github.com/tmc/langchaingo/tools"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"sigs.k8s.io/yaml"

	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"

	"github.com/upbound/function-openai/input/v1alpha1"
	"github.com/upbound/function-openai/internal/tool"
)

const (
	credName       = "gpt"
	credKey        = "OPENAI_API_KEY"
	credBaseURLKey = "OPENAI_BASE_URL"
	credModelKey   = "OPENAI_MODEL"
	defaultModel   = "gpt-4"

	// Cap generation when the input does not. Hosted APIs impose their own
	// default, but a self-hosted OpenAI-compatible engine will happily generate
	// until its context window is full.
	defaultMaxTokens = 1024
)

// Variables used to form the prompt.
type Variables struct {
	// Observed composite resource, as a YAML manifest.
	Composite string

	// Observed composed resources, as a stream of YAML manifests.
	Composed string
}

// Function asks GPT to compose resources.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer
	ai agentInvoker

	log logging.Logger
}

// agentInvoker is a consumer interface for working with agents. Notably this
// is helpful for writing tests that mock the agent invocations.
type agentInvoker interface {
	Invoke(ctx context.Context, key, system, prompt, baseURL, modelName string, maxTokens int) (string, error)
}

// Option modifies the underlying Function.
type Option func(*Function)

// WithLogger overrides the default logger.
func WithLogger(log logging.Logger) Option {
	return func(f *Function) {
		f.log = log
	}
}

// NewFunction creates a new function powered by GPT.
func NewFunction(opts ...Option) *Function {
	f := &Function{
		log: logging.NewNopLogger(),
	}

	for _, o := range opts {
		o(f)
	}

	f.ai = &agent{
		log: f.log,
		res: tool.NewResolver(tool.WithLogger(f.log)),
	}

	return f
}

// RunFunction runs the Function.
func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	log := f.log.WithValues("tag", req.GetMeta().GetTag())
	log.Info("Running function", "tag", req.GetMeta().GetTag())

	rsp := response.To(req, response.DefaultTTL)

	if f.shouldIgnore(req) {
		response.ConditionTrue(rsp, "FunctionSuccess", "Success").TargetCompositeAndClaim()
		response.Normal(rsp, "received an ignored resource, skipping")
		return rsp, nil
	}

	in := &v1alpha1.Prompt{}
	if err := request.GetInput(req, in); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, err
	}

	c, err := request.GetCredentials(req, credName)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get OPENAI_API_KEY from credential %q", credName))
		return rsp, err
	}
	if c.Type != resource.CredentialsTypeData {
		response.Fatal(rsp, errors.Errorf("expected credential %q to be %q, got %q", credName, resource.CredentialsTypeData, c.Type))
		return rsp, err
	}

	b, ok := c.Data[credKey]
	if !ok {
		response.Fatal(rsp, errors.Errorf("credential %q is missing required key %q", credName, credKey))
		return rsp, err
	}

	// TODO(negz): Where the heck is the newline at the end of this key
	// coming from? Bug in crossplane render?
	key := strings.Trim(string(b), "\n")

	// Extract optional base URL from credentials
	var baseURL string
	if baseURLBytes, ok := c.Data[credBaseURLKey]; ok {
		baseURL = strings.Trim(string(baseURLBytes), "\n")
	}

	// Extract optional model from credentials, default to gpt-4
	model := defaultModel
	if modelBytes, ok := c.Data[credModelKey]; ok {
		model = strings.Trim(string(modelBytes), "\n")
	}

	maxTokens := defaultMaxTokens
	if in.MaxTokens > 0 {
		maxTokens = in.MaxTokens
	}

	d := pipelineDetails{
		req:       req,
		rsp:       rsp,
		in:        in,
		cred:      key,
		baseURL:   baseURL,
		model:     model,
		maxTokens: maxTokens,
	}

	// If we're in a composition pipeline we want to do things with the
	// composed resources.
	if inCompositionPipeline(req) {
		return f.compositionPipeline(ctx, log, d)
	}
	// Handle operation pipeline separately.
	return f.operationPipeline(ctx, log, d)
}

// CompositeToYAML returns the XR as YAML.
func CompositeToYAML(xr *fnv1.Resource) (string, error) {
	j, err := protojson.Marshal(xr.GetResource())
	if err != nil {
		return "", errors.Wrap(err, "cannot convert XR to JSON")
	}
	y, err := yaml.JSONToYAML(j)
	return string(y), errors.Wrap(err, "cannot convert XR to YAML")
}

// ComposedToYAML returns the supplied composed resources as a YAML stream. The
// resources are annotated with their upbound.io/name annotations.
func ComposedToYAML(cds map[string]*fnv1.Resource) (string, error) {
	composed := &strings.Builder{}

	for name, ocd := range cds {
		jocd, err := protojson.Marshal(ocd.GetResource())
		if err != nil {
			return "", errors.Wrap(err, "cannot convert composed resource to JSON")
		}

		jocd, err = sjson.SetBytes(jocd, "metadata.annotations.upbound\\.io/name", name)
		if err != nil {
			return "", errors.Wrapf(err, "cannot set upbound.io/name annotation")
		}

		yocd, err := yaml.JSONToYAML(jocd)
		if err != nil {
			return "", errors.Wrap(err, "cannot convert composed resource to YAML")
		}
		composed.WriteString("---\n")
		composed.Write(yocd)
	}

	return composed.String(), nil
}

// ComposedFromYAML parses the supplied YAML stream as desired composed
// resources. The resource names are extracted from the upbound.io/name
// annotation.
func ComposedFromYAML(y string) (map[string]*fnv1.Resource, error) {
	out := make(map[string]*fnv1.Resource)

	for doc := range strings.SplitSeq(y, "---") {
		if doc == "" {
			continue
		}
		j, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			return nil, errors.Wrap(err, "cannot parse YAML")
		}

		s := &structpb.Struct{}
		if err := protojson.Unmarshal(j, s); err != nil {
			return nil, errors.Wrap(err, "cannot parse JSON")
		}

		name := gjson.GetBytes(j, "metadata.annotations.upbound\\.io/name").String()
		if name == "" {
			return nil, errors.New("missing 'upbound.io/name' annotation")
		}
		if _, seen := out[name]; seen {
			return nil, errors.Errorf("'upbound.io/name' annotation %q must be unique within the YAML stream", name)
		}
		out[name] = &fnv1.Resource{Resource: s}
	}

	return out, nil
}

// stripCodeFence removes a markdown code fence wrapping a model's reply.
//
// Models wrap structured output in fences unpredictably, and a fenced reply is
// neither valid YAML nor valid JSON - so without this a perfectly good answer
// is discarded. The previous version handled only a "```yaml" prefix, which
// missed "```json" entirely; a model asked for JSON naturally reaches for that
// one.
func stripCodeFence(in string) string {
	out := strings.TrimSpace(in)
	for _, prefix := range []string{"```yaml", "```yml", "```json", "```"} {
		if strings.HasPrefix(out, prefix) {
			out = strings.TrimPrefix(out, prefix)
			break
		}
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(out), "```"))
}

// resourceFrom produces a map of resource name to resources derived from the
// given string. If the string is neither JSON nor YAML, an error is returned.
func (f *Function) resourceFrom(i string) (map[string]*fnv1.Resource, error) {
	out := make(map[string]*fnv1.Resource)

	// Strip a markdown fence first. The composition path already did this; the
	// operation path did not, so a fenced reply failed to parse here and was
	// then discarded silently.
	b := []byte(stripCodeFence(i))

	// Is i YAML?
	jb, err := yaml.YAMLToJSON(b)
	if err != nil {
		f.log.Debug("error seen while attempting to convert YAML to JSON", "error", err)
		// i doesn't appear to be YAML, maybe it's JSON...
		jb = b
	}

	s := &structpb.Struct{}
	if err := protojson.Unmarshal(jb, s); err != nil {
		return nil, errors.Wrap(err, "cannot parse JSON")
	}

	name := gjson.GetBytes(jb, "metadata.name").String()
	out[name] = &fnv1.Resource{Resource: s}

	return out, nil
}

// resourceIdentity is the tuple that decides which object a patch lands on.
type resourceIdentity struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

func (r resourceIdentity) String() string {
	gvk := strings.TrimSpace(r.APIVersion + " " + r.Kind)
	if r.Namespace == "" {
		return strings.TrimSpace(gvk + " " + r.Name)
	}
	return gvk + " " + r.Namespace + "/" + r.Name
}

// sameObject reports whether two identities name the same Kubernetes object.
//
// The API *version* is deliberately excluded. v1alpha1 and v1beta1 of one kind
// address the same object, so rejecting on a version difference would refuse
// output that is perfectly correct. Group, kind, namespace and name are what
// actually select the object being patched.
func (r resourceIdentity) sameObject(o resourceIdentity) bool {
	return apiGroup(r.APIVersion) == apiGroup(o.APIVersion) &&
		r.Kind == o.Kind &&
		r.Namespace == o.Namespace &&
		r.Name == o.Name
}

// apiGroup returns the group from an apiVersion, which is "" for core types.
func apiGroup(apiVersion string) string {
	if i := strings.Index(apiVersion, "/"); i >= 0 {
		return apiVersion[:i]
	}
	return ""
}

// identityOf reads an object's identity out of unstructured content. It takes a
// plain map so it serves both the watched resource and a parsed model reply.
func identityOf(obj map[string]any) resourceIdentity {
	id := resourceIdentity{}
	if v, ok := obj["apiVersion"].(string); ok {
		id.APIVersion = v
	}
	if v, ok := obj["kind"].(string); ok {
		id.Kind = v
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		if v, ok := md["name"].(string); ok {
			id.Name = v
		}
		if v, ok := md["namespace"].(string); ok {
			id.Namespace = v
		}
	}
	return id
}

// attempts to identify if the function is operating within a composition
// pipeline or not by looking to see if a composite was sent with the request.
func inCompositionPipeline(req *fnv1.RunFunctionRequest) bool {
	return req.GetObserved().GetComposite() != nil
}

// pipelineDetails wraps the inputs and outputs for the given function run.
type pipelineDetails struct {
	// FunctionRequest
	req *fnv1.RunFunctionRequest
	// FunctionResponse
	rsp *fnv1.RunFunctionResponse
	// marshalled input
	in *v1alpha1.Prompt
	// LLM API credential
	cred string
	// Optional base URL for OpenAI API
	baseURL string
	// Optional model name, defaults to gpt-4
	model string
	// Cap on generated tokens, defaults to defaultMaxTokens
	maxTokens int
}

// compositionPipeline processes the given pipelineDetails with the assumption
// that the function is defined in a composition pipeline and will be working
// with composites and desired resources.
func (f *Function) compositionPipeline(ctx context.Context, log logging.Logger, d pipelineDetails) (*fnv1.RunFunctionResponse, error) {
	userPrompt, err := template.New("prompt").Parse(d.in.UserPrompt)
	if err != nil {
		response.Fatal(d.rsp, errors.Wrap(err, "cannot parse userPrompt"))
		return d.rsp, err
	}

	// TODO(ththornton): possibly switch to just JSON to remove the double encode.
	xr, err := CompositeToYAML(d.req.GetObserved().GetComposite())
	if err != nil {
		response.Fatal(d.rsp, errors.Wrap(err, "cannot convert observed XR to YAML"))
		return d.rsp, err
	}

	cds, err := ComposedToYAML(d.req.GetObserved().GetResources())
	if err != nil {
		response.Fatal(d.rsp, errors.Wrap(err, "cannot convert observed composed resources to YAML"))
		return d.rsp, err
	}

	pb := &strings.Builder{}
	if err := userPrompt.Execute(pb, &Variables{Composite: xr, Composed: cds}); err != nil {
		response.Fatal(d.rsp, errors.Wrapf(err, "cannot build prompt from template"))
		return d.rsp, err
	}

	log.Debug("Using prompt", "prompt", pb.String())

	resp, err := f.ai.Invoke(ctx, d.cred, d.in.SystemPrompt, pb.String(), d.baseURL, d.model, d.maxTokens)

	if err != nil {
		response.Fatal(d.rsp, errors.Wrap(err, "failed to run chain"))
		return d.rsp, err
	}

	result := ""
	dcds, err := ComposedFromYAML(stripCodeFence(resp))
	if err != nil {
		result = err.Error()
		log.Debug("Submitted YAML stream", "result", result, "isError", true)
		response.Fatal(d.rsp, errors.Wrap(err, "did not receive a YAML stream from GPT"))
		return d.rsp, err
	}

	log.Debug("Received YAML manifests from GPT", "resourceCount", len(dcds))
	d.rsp.Desired.Resources = dcds
	return d.rsp, nil
}

// OperationVariables used to form the prompt.
type OperationVariables struct {
	Input     string `json:"input"`
	Resources string `json:"resources"`
}

// operationPipeline processes the given pipelineDetails with the assumption
// that the function is defined in an operations pipeline.
func (f *Function) operationPipeline(ctx context.Context, log logging.Logger, d pipelineDetails) (*fnv1.RunFunctionResponse, error) {
	prompt, err := template.New("prompt").Parse(d.in.UserPrompt)
	if err != nil {
		response.Fatal(d.rsp, errors.New("failed to parse UserPrompt as a go-template"))
		return d.rsp, err
	}
	rr, err := request.GetRequiredResources(d.req)
	if err != nil {
		response.Fatal(d.rsp, errors.Wrapf(err, "cannot get Function extra resources from %T", d.req))
		return d.rsp, err
	}

	// TODO(tnthornton) reference const from c/c instead. Currently too many
	// conflicting dependencies are pulled in when updating c/c in this repo.
	rs, ok := rr["ops.crossplane.io/watched-resource"]
	if !ok {
		f.log.Debug("no resource to process")
		response.ConditionTrue(d.rsp, "FunctionSuccess", "Success").TargetCompositeAndClaim()
		return d.rsp, nil
	}

	if len(rs) != 1 {
		response.Fatal(d.rsp, errors.New("too many resources sent to the function. expected 1"))
		return d.rsp, err
	}

	rb, err := json.MarshalIndent(rs[0].Resource.UnstructuredContent(), "", "    ")
	if err != nil {
		response.Fatal(d.rsp, errors.New("failed to unmarshal required resource"))
		return d.rsp, err
	}

	vars := &strings.Builder{}
	if err := prompt.Execute(vars, &OperationVariables{Input: d.in.UserPrompt, Resources: string(rb)}); err != nil {
		response.Fatal(d.rsp, errors.Wrapf(err, "cannot build prompt from template"))
		return d.rsp, err
	}

	log.Debug("Using prompt", "prompt", vars.String())

	resp, err := f.ai.Invoke(ctx, d.cred, d.in.SystemPrompt, vars.String(), d.baseURL, d.model, d.maxTokens)

	if err != nil {
		response.Fatal(d.rsp, errors.Wrap(err, "failed to run chain"))
		return d.rsp, err
	}

	// An empty reply is a deliberate no-op, and has to stay a genuine no-write.
	// An operations pipeline driven by a WatchOperation re-triggers on any
	// change to the watched resource, so writing anything at all when there is
	// nothing to do makes the watch fire forever.
	if strings.TrimSpace(stripCodeFence(resp)) == "" {
		log.Debug("model returned no output, treating as an explicit no-op")
		response.ConditionTrue(d.rsp, "FunctionSuccess", "NoOp").TargetCompositeAndClaim()
		return d.rsp, nil
	}

	desired, err := f.resourceFrom(resp)
	if err != nil {
		// Non-empty but unparseable. This used to be logged at Debug while
		// still reporting FunctionSuccess=True with no desired resources, so a
		// model that had drifted off-format was indistinguishable from one that
		// had decided to do nothing - and nothing anywhere said so. Report it.
		log.Info("cannot parse model output, no desired resources will be sent to crossplane",
			"error", err)
		response.ConditionFalse(d.rsp, "FunctionSuccess", "UnparseableModelOutput").
			WithMessage(fmt.Sprintf("model output was neither valid YAML nor JSON: %s", err)).
			TargetCompositeAndClaim()
		response.Warning(d.rsp, errors.Wrap(err, "cannot parse model output"))
		return d.rsp, nil
	}

	// Refuse to patch anything other than the resource we were asked about.
	//
	// A model under load copies a few-shot example's name and namespace out of
	// the prompt instead of reading the watched resource's. Crossplane
	// server-side-applies whatever comes back, so on a fleet where that name
	// exists this silently reconfigures somebody else's resource. Measured on a
	// self-hosted CPU engine: 5 of 8 Operations did this under 8-way
	// concurrency, 0 of 2 sequentially.
	//
	// This is a safety invariant, so it is enforced here rather than left to
	// prompt wording or engine capacity. Rewriting the identity to match would
	// be worse than refusing: the decision itself was reasoned about the wrong
	// resource, so the instance class in it cannot be trusted either.
	watched := identityOf(rs[0].Resource.UnstructuredContent())
	for _, r := range desired {
		got := identityOf(r.Resource.AsMap())
		if got.sameObject(watched) {
			continue
		}
		log.Info("model returned a resource other than the watched one, refusing to apply",
			"watched", watched.String(), "returned", got.String())
		response.ConditionFalse(d.rsp, "FunctionSuccess", "ResourceIdentityMismatch").
			WithMessage(fmt.Sprintf("model returned %s but the watched resource is %s; "+
				"refusing to apply", got, watched)).
			TargetCompositeAndClaim()
		response.Warning(d.rsp, errors.Errorf(
			"model returned %s but the watched resource is %s", got, watched))
		return d.rsp, nil
	}

	response.ConditionTrue(d.rsp, "FunctionSuccess", "Success").TargetCompositeAndClaim()
	response.Normal(d.rsp, resp)

	d.rsp.Desired.Resources = desired
	return d.rsp, nil
}

// shouldIgnore returns true if the caller has communicated that the resource
// is an ignored resource. False otherwise.
func (f *Function) shouldIgnore(req *fnv1.RunFunctionRequest) bool {
	fctx := req.GetContext()
	ignored, ok := fctx.AsMap()["ops.upbound.io/ignored-resource"]
	if ok {
		i, ok := ignored.(bool)
		if ok && i {
			return true
		}
	}
	return false
}

type agent struct {
	log logging.Logger
	res *tool.Resolver
}

// Invoke makes an external call to the configured LLM with the supplied
// credential key, system and user prompts.
func (a *agent) Invoke(ctx context.Context, key, system, prompt, baseURL, modelName string, maxTokens int) (string, error) {
	opts := []openaillm.Option{
		openaillm.WithToken(key),
		openaillm.WithModel(modelName),
	}

	// Add custom base URL if provided
	if baseURL != "" {
		opts = append(opts, openaillm.WithBaseURL(baseURL))
	}

	model, err := openaillm.New(opts...)
	if err != nil {
		return "", errors.Wrap(err, "failed to build model")
	}

	agent := agents.NewOpenAIFunctionsAgent(
		model,
		a.tools(ctx),
		agents.WithMaxIterations(20),
		agents.NewOpenAIOption().WithSystemMessage(system),
	)

	return chains.Run(
		ctx,
		agents.NewExecutor(agent),
		prompt,
		chains.WithTemperature(float64(0)),
		// Without a cap the engine generates until its context is exhausted.
		// See the MaxTokens field on the Prompt input.
		chains.WithMaxTokens(maxTokens),
	)
}

func (a *agent) tools(ctx context.Context) []tools.Tool {
	cfgs := a.res.FromEnvVars()
	if len(cfgs) == 0 {
		a.log.Debug("no valid mcp server configurations found")
	}
	return a.res.Resolve(ctx, cfgs)
}
