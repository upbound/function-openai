import models.io.upbound.dev.meta.v1alpha1 as metav1alpha1

_items = [
    metav1alpha1.CompositionTest{
        metadata.name: "function-openai"
        spec= {
            functionCredentialsPath: "example/secret.yaml"
            assertResources: [
                {
                    apiVersion: "example.crossplane.io/v1"
                    kind: "App"
                    metadata: {
                        name: "example-db"
                    }
                }
            ]
            compositionPath: "example/composition.yaml"
            xrPath: "example/xr.yaml"
            timeoutSeconds: 60
            validate: False
        }
    }
]
items= _items