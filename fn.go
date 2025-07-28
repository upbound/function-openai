package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"

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
)

const (
	credName = "gpt"
	credKey  = "OPENAI_API_KEY"
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
	Invoke(ctx context.Context, key, system, prompt string) (string, error)
}

// NewFunction creates a new function powered by GPT.
func NewFunction(log logging.Logger) *Function {
	return &Function{
		ai:  &agent{},
		log: log,
	}
}

// RunFunction runs the Function.
func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	log := f.log.WithValues("tag", req.GetMeta().GetTag())
	log.Info("Running function", "tag", req.GetMeta().GetTag())

	rsp := response.To(req, response.DefaultTTL)

	in := &v1alpha1.Prompt{}
	if err := request.GetInput(req, in); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, nil
	}

	c, err := request.GetCredentials(req, credName)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get OPENAI_API_KEY from credential %q", credName))
		return rsp, nil
	}
	if c.Type != resource.CredentialsTypeData {
		response.Fatal(rsp, errors.Errorf("expected credential %q to be %q, got %q", credName, resource.CredentialsTypeData, c.Type))
		return rsp, nil
	}

	b, ok := c.Data[credKey]
	if !ok {
		response.Fatal(rsp, errors.Errorf("credential %q is missing required key %q", credName, credKey))
		return rsp, nil
	}

	// TODO(negz): Where the heck is the newline at the end of this key
	// coming from? Bug in crossplane render?
	key := strings.Trim(string(b), "\n")
	d := pipelineDetails{
		req:  req,
		rsp:  rsp,
		in:   in,
		cred: key,
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

// removeYAMLMarkdown is a helper function for cleaning the output from GPT.
// The responses can be inconsitent with markdown being returned at times.
// This function takes a multi-line string containing YAML tags and cleans
// those for future processing of the YAML stream.
func removeYAMLMarkdown(in string) string {
	wsRemoved := strings.TrimSpace(in)
	yamlPrefix := strings.TrimPrefix(wsRemoved, "```yaml")
	return strings.TrimSuffix(yamlPrefix, "```")
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
}

// compositionPipeline processes the given pipelineDetails with the assumption
// that the function is defined in a composition pipeline and will be working
// with composites and desired resources.
func (f *Function) compositionPipeline(ctx context.Context, log logging.Logger, d pipelineDetails) (*fnv1.RunFunctionResponse, error) {
	userPrompt, err := template.New("prompt").Parse(d.in.UserPrompt)
	if err != nil {
		response.Fatal(d.rsp, errors.Wrap(err, "cannot parse userPrompt"))
		return d.rsp, nil
	}

	// TODO(ththornton): possibly switch to just JSON to remove the double encode.
	xr, err := CompositeToYAML(d.req.GetObserved().GetComposite())
	if err != nil {
		response.Fatal(d.rsp, errors.Wrap(err, "cannot convert observed XR to YAML"))
		return d.rsp, nil
	}

	cds, err := ComposedToYAML(d.req.GetObserved().GetResources())
	if err != nil {
		response.Fatal(d.rsp, errors.Wrap(err, "cannot convert observed composed resources to YAML"))
		return d.rsp, nil
	}

	pb := &strings.Builder{}
	if err := userPrompt.Execute(pb, &Variables{Composite: xr, Composed: cds}); err != nil {
		response.Fatal(d.rsp, errors.Wrapf(err, "cannot build prompt from template"))
		return d.rsp, nil
	}

	log.Debug("Using prompt", "prompt", pb.String())

	resp, err := f.ai.Invoke(ctx, d.cred, d.in.SystemPrompt, pb.String())

	if err != nil {
		response.Fatal(d.rsp, errors.Wrap(err, "failed to run chain"))
		return d.rsp, nil
	}

	result := ""
	dcds, err := ComposedFromYAML(removeYAMLMarkdown(resp))
	if err != nil {
		result = err.Error()
		log.Debug("Submitted YAML stream", "result", result, "isError", true)
		response.Fatal(d.rsp, errors.Wrap(err, "did not receive a YAML stream from GPT"))
	}

	log.Debug("Received YAML manifests from GPT", "resourceCount", len(dcds))
	d.rsp.Desired.Resources = dcds
	return d.rsp, nil
}

// operationPipeline processes the given pipelineDetails with the assumption
// that the function is defined in an operations pipeline.
func (f *Function) operationPipeline(ctx context.Context, log logging.Logger, d pipelineDetails) (*fnv1.RunFunctionResponse, error) {
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
		return d.rsp, nil
	}

	rb, err := json.Marshal(rs[0].Resource.UnstructuredContent())
	if err != nil {
		response.Fatal(d.rsp, errors.New("failed to unmarshal required resource"))
		return d.rsp, err
	}

	prompt := fmt.Sprintf("%s\n%s", d.in.UserPrompt, string(rb))

	log.Debug("Using prompt", "prompt", prompt)

	resp, err := f.ai.Invoke(ctx, d.cred, d.in.SystemPrompt, prompt)

	if err != nil {
		response.Fatal(d.rsp, errors.Wrap(err, "failed to run chain"))
		return d.rsp, err
	}

	response.ConditionTrue(d.rsp, "FunctionSuccess", "Success").TargetCompositeAndClaim()
	response.Normal(d.rsp, resp)
	return d.rsp, nil
}

type agent struct{}

// Invoke makes an external call to the configured LLM with the supplied
// credential key, system and user prompts.
func (a *agent) Invoke(ctx context.Context, key, system, prompt string) (string, error) {
	model, err := openaillm.New(
		openaillm.WithToken(key),
		// NOTE(tnthornton): gpt-4 is noticeably slow compared to gpt-4o, but
		// gpt-4o is sending input back that the agent is having trouble
		// parsing. More to dig into here before switching.
		openaillm.WithModel("gpt-4"),
	)
	if err != nil {
		return "", errors.Wrap(err, "failed to build model")
	}

	agent := agents.NewOneShotAgent(
		model,
		// NOTE(tnthornton) Placeholder for future integrations with external Tools.
		[]tools.Tool{},
		agents.WithMaxIterations(3),
		agents.NewOpenAIOption().WithSystemMessage(system),
	)

	return chains.Run(
		ctx,
		agents.NewExecutor(agent),
		prompt,
		chains.WithTemperature(float64(0)),
	)
}
