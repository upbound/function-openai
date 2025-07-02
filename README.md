# function-openai
[![CI](https://github.com/upbound/function-openai/actions/workflows/ci.yml/badge.svg)](https://github.com/upbound/function-openai/actions/workflows/ci.yml)

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
      prompt: |
        Use the resource in the <composite> tag to template a Deployment.
        Use the value at JSON path .spec.replicas to set the Deployment's
        replicas. Use the value at JSON path .spec.image to set its
        container image.

        Create a Service that exposes the Deployment's port 8080.
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

[functions]: https://docs.crossplane.io/latest/concepts/composition-functions
[go]: https://go.dev
[function guide]: https://docs.crossplane.io/knowledge-base/guides/write-a-composition-function-in-go
[package docs]: https://pkg.go.dev/github.com/crossplane/function-sdk-go
[docker]: https://www.docker.com
[cli]: https://docs.crossplane.io/latest/cli
