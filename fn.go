package main

import (
	"context"
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

	log logging.Logger
}

// NewFunction creates a new function powered by GPT.
func NewFunction(log logging.Logger) *Function {
	return &Function{
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

	userPrompt, err := template.New("prompt").Parse(in.UserPrompt)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot parse userPrompt"))
		return rsp, nil
	}

	// TODO(ththornton): possibly switch to just JSON to remove the double encode.
	xr, err := CompositeToYAML(req.GetObserved().GetComposite())
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot convert observed XR to YAML"))
		return rsp, nil
	}

	cds, err := ComposedToYAML(req.GetObserved().GetResources())
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot convert observed composed resources to YAML"))
		return rsp, nil
	}

	prompt := &strings.Builder{}
	if err := userPrompt.Execute(prompt, &Variables{Composite: xr, Composed: cds}); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot build prompt from template"))
		return rsp, nil
	}

	log.Debug("Using prompt", "prompt", prompt.String())

	resp, err := invokeAgent(ctx, key, in.SystemPrompt, prompt.String())

	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "failed to run chain"))
		return rsp, nil
	}

	result := ""
	dcds, err := ComposedFromYAML(removeYAMLMarkdown(resp))
	if err != nil {
		result = err.Error()
		log.Debug("Submitted YAML stream", "result", result, "isError", true)
		response.Fatal(rsp, errors.Wrap(err, "did not receive a YAML stream from GPT"))
	}

	log.Debug("Received YAML manifests from GPT", "resourceCount", len(dcds))
	rsp.Desired.Resources = dcds

	return rsp, nil
}

// invokeAgent calls the GPT backed agent with the given prompt.
func invokeAgent(ctx context.Context, key, system, prompt string) (string, error) {
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

	for _, doc := range strings.Split(y, "---") {
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
