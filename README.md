# function-openai
[![CI](https://github.com/upbound/function-openai/actions/workflows/ci.yml/badge.svg)](https://github.com/upbound/function-openai/actions/workflows/ci.yml)
[![Slack](https://img.shields.io/badge/slack-upbound_crossplane-purple?logo=slack)](https://crossplane.slack.com/archives/C01TRKD4623)
[![GitHub release](https://img.shields.io/github/release/upbound/function-openai/all.svg)](https://github.com/upbound/function-openai/releases)

Use natural language prompts to compose resources.

```yaml
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: compose-an-app-with-gpt
spec:
  compositeTypeRef:
    apiVersion: example.crossplane.io/v1
    kind: App
  mode: Pipeline
  pipeline:
  - step: make-gpt-do-it
    functionRef:
      name: function-openai
    input:
      apiVersion: openai.fn.upbound.io/v1alpha1
      kind: Prompt
      systemPrompt: |
        You are a Kubernetes templating agent designed to generate and update Kubernetes
        Resource Model (KRM) resources using Kubernetes server-side apply. Your task is
        to create, update, or delete YAML manifests based on the provided composite
        resource and any existing composed resources.

        Respond with only valid YAML manifests.
      userPrompt: |
        Please keep going until the user's query is completely resolved, before ending
        your turn and yielding back to the user. Only terminate your turn when you are
        sure that the problem is solved.
        Please follow these instructions carefully:
        1. Analyze the provided composite resource and any existing composed resources.
        2. Analyze the input to understand what composed resources you should create,
           update, or delete. You may be asked to derive composed resources from the
           composite resource, or from other composed resources.
        3. Generate a stream of YAML manifests based on your analysis in steps 1 and 2.
           Each manifest must:
           a. Be valid for Kubernetes server-side apply (fully specified intent).
           b. Omit names and namespaces.
           c. Include an annotation with the key "upbound.io/name". This annotation
              must uniquely identify the manifest within the YAML stream. It must be
              lowercase, hyphen separated, and less than 30 characters long. Prefer
              to use the manifest's kind. If two or more manifests have the same
              kind, look for something unique about the manifest and append that to
              the kind. This annotation is used to match the manifests you return to
              any manifests that were passed you inside the <composed> tag, so if
              your intent is to update a manifest never change its "upbound.io/name"
              annotation. This is critically important.
           d. If it's necessary to use labels to create relationships between
              resources, use the name of the composite resource as the label value.
        4. If there are existing composed resources:
            a. You can update an existing composed resource by including it in your
               output with any changes you deem necessary based on the input. Try to
               reuse existing composed resource values as much as possible. Only
               change values when you're sure it's necessary.
            b. If the input indicates that a resource is no longer required, you can
               delete it by omitting it from your output.
        5. Your output must only be a stream of YAML manifests, each separated by
           "---".
        ---
        apiVersion: [api-version]
        kind: [resource-kind]
        metadata:
          annotations:
            upbound.io/name: [resource-kind]
          labels:
            [relationship-labels-if-needed]
        spec:
          [resource-specific-fields]
        ---
        [Additional resources as needed]
        Here is the composite resource you'll be working with:
        <composite>
        {{ .Composite }}
        </composite>
        If there are any existing composed resources, they will be provided here:
        <composed>
        {{ .Composed }}
        </composed>
        Additional input is provided here:
        <input>
        {{ .Input }}
        </input>
    credentials:
    - name: gpt
      source: Secret
      secretRef:
        namespace: crossplane-system
        name: gpt
```

See `fn.go` for the prompt.

Composed resource output _should_ be more stable if you pass the output back in
using the `--observed-resources` flag. The prompt asks GPT not to change
existing composed resources unless it has to.

This template uses [Go][go], [Docker][docker], and the [Crossplane CLI][cli] to
build functions.

```shell
# Run code generation - see input/generate.go
$ go generate ./...

# Run tests - see fn_test.go
$ go test ./...

# Build the function's runtime image - see Dockerfile
$ docker build . --tag=runtime

# Build a function package - see package/crossplane.yaml
$ crossplane xpkg build -f package --embed-runtime-image=runtime
```

## Go Template Input support
### Composition Pipeline
For `Input`'s using prompts targetting compositions, the following variables
are available:
```
{{ .Composed }}
{{ .Composite }}
```

Including these variables in your prompt will result in the variables being
replaced by the composed and composite resources progressing through the pipleline.

### Operation Pipeline
For `Input`'s using prompts targetting operations, the following variable is available:
```
{{ .Resources }}
```

Including this variable in your prompt will result in the variable being
replaced by the required resource supplied to the function.

## Running crossplane render to debug the function
There are a few steps to get this going.

1. Add a secret.yaml that contains your OPENAI_API_KEY for use with local
development.
```bash
export OPENAI_API_KEY_B64=$(echo ${OPENAI_API_KEY} | base64)

cat <<EOF | envsubst > example/secret.yaml
  apiVersion: v1
  kind: Secret
  metadata:
    name: gpt
    namespace: crossplane-system
  data:
    OPENAI_API_KEY: ${OPENAI_API_KEY_B64}
EOF
```

2. In a separate terminal, start the function
```bash
 go run . --insecure --debug
```

3. Run `crossplane render`
```bash
./hack/bin/crossplane render example/xr.yaml example/composition.yaml example/functions.yaml --function-credentials=example/secret.yaml --verbose
```

## Running composition tests
1. Download the `up` CLI. # Currently main is needed due to features that have 
not shipped.
```bash
curl -sL https://cli.upbound.io | CHANNEL=main sh
```

2. Run render assertion tests
```
./up test run tests/*
```

[functions]: https://docs.crossplane.io/latest/concepts/composition-functions
[go]: https://go.dev
[function guide]: https://docs.crossplane.io/knowledge-base/guides/write-a-composition-function-in-go
[package docs]: https://pkg.go.dev/github.com/crossplane/function-sdk-go
[docker]: https://www.docker.com
[cli]: https://docs.crossplane.io/latest/cli
