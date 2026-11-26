package cache

import (
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/utils/kube"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

func svc(namespace, name string) v1alpha1.ResourceRef {
	return v1alpha1.ResourceRef{Kind: kube.ServiceKind, Namespace: namespace, Name: name}
}

func targetWeightsOf(t *testing.T, manifest string) []v1alpha1.ResourceTargetWeight {
	t.Helper()
	info := &ResourceInfo{}
	populateNodeInfo(strToUnstructured(manifest), info, []string{})
	require.NotNil(t, info.NetworkingInfo)
	return info.NetworkingInfo.TargetWeights
}

func TestTargetPercentages(t *testing.T) {
	t.Run("rounds to whole percentages", func(t *testing.T) {
		assert.Equal(t, []v1alpha1.ResourceTargetWeight{
			{Target: svc("ns", "a"), Percentage: 33},
			{Target: svc("ns", "b"), Percentage: 67},
		}, targetPercentages(map[v1alpha1.ResourceRef]int64{svc("ns", "b"): 2, svc("ns", "a"): 1}))
	})
	t.Run("single target has no split", func(t *testing.T) {
		assert.Nil(t, targetPercentages(map[v1alpha1.ResourceRef]int64{svc("ns", "a"): 100}))
	})
	t.Run("zero total", func(t *testing.T) {
		assert.Nil(t, targetPercentages(map[v1alpha1.ResourceRef]int64{svc("ns", "a"): 0, svc("ns", "b"): 0}))
	})
	t.Run("negative weight", func(t *testing.T) {
		assert.Nil(t, targetPercentages(map[v1alpha1.ResourceRef]int64{svc("ns", "a"): -1, svc("ns", "b"): 10}))
	})
}

func TestIstioVirtualServiceTargetWeights(t *testing.T) {
	t.Run("weighted default route", func(t *testing.T) {
		weights := targetWeightsOf(t, `
apiVersion: networking.istio.io/v1beta1
kind: VirtualService
metadata:
  name: rollout-vsvc
  namespace: demo
spec:
  http:
  - name: primary
    route:
    - destination:
        host: stable-svc
      weight: 90
    - destination:
        host: canary-svc.demo.svc.cluster.local
      weight: 10
`)
		assert.Equal(t, []v1alpha1.ResourceTargetWeight{
			{Target: svc("demo", "canary-svc"), Percentage: 10},
			{Target: svc("demo", "stable-svc"), Percentage: 90},
		}, weights)
	})
	t.Run("rules with match conditions are ignored", func(t *testing.T) {
		weights := targetWeightsOf(t, `
apiVersion: networking.istio.io/v1beta1
kind: VirtualService
metadata:
  name: rollout-vsvc
  namespace: demo
spec:
  http:
  - match:
    - headers:
        x-canary:
          exact: "true"
    route:
    - destination:
        host: canary-svc
      weight: 100
  - route:
    - destination:
        host: stable-svc
      weight: 75
    - destination:
        host: canary-svc
      weight: 25
`)
		assert.Equal(t, []v1alpha1.ResourceTargetWeight{
			{Target: svc("demo", "canary-svc"), Percentage: 25},
			{Target: svc("demo", "stable-svc"), Percentage: 75},
		}, weights)
	})
	t.Run("single destination", func(t *testing.T) {
		assert.Nil(t, targetWeightsOf(t, `
apiVersion: networking.istio.io/v1beta1
kind: VirtualService
metadata:
  name: vsvc
  namespace: demo
spec:
  http:
  - route:
    - destination:
        host: stable-svc
`))
	})
}

func TestGatewayAPIRouteTargetWeights(t *testing.T) {
	t.Run("weighted backends", func(t *testing.T) {
		weights := targetWeightsOf(t, `
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: rollout-route
  namespace: demo
spec:
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /
    backendRefs:
    - name: stable-svc
      port: 80
      weight: 80
    - name: canary-svc
      port: 80
      weight: 20
`)
		assert.Equal(t, []v1alpha1.ResourceTargetWeight{
			{Target: svc("demo", "canary-svc"), Percentage: 20},
			{Target: svc("demo", "stable-svc"), Percentage: 80},
		}, weights)
	})
	t.Run("weight defaults to 1", func(t *testing.T) {
		weights := targetWeightsOf(t, `
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: route
  namespace: demo
spec:
  rules:
  - backendRefs:
    - name: a
    - name: b
`)
		assert.Equal(t, []v1alpha1.ResourceTargetWeight{
			{Target: svc("demo", "a"), Percentage: 50},
			{Target: svc("demo", "b"), Percentage: 50},
		}, weights)
	})
	t.Run("rules for specific paths are ignored", func(t *testing.T) {
		assert.Nil(t, targetWeightsOf(t, `
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: route
  namespace: demo
spec:
  rules:
  - matches:
    - path:
        value: /api
    backendRefs:
    - name: a
      weight: 50
    - name: b
      weight: 50
`))
	})
}

func TestALBIngressTargetWeights(t *testing.T) {
	t.Run("networking.k8s.io/v1 forward action", func(t *testing.T) {
		info := &ResourceInfo{}
		populateNodeInfo(strToUnstructured(`
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: rollout-ingress
  namespace: demo
  annotations:
    alb.ingress.kubernetes.io/actions.root-svc: |
      {"type":"forward","forwardConfig":{"targetGroups":[{"serviceName":"canary-svc","servicePort":"80","weight":30},{"serviceName":"stable-svc","servicePort":"80","weight":70}]}}
spec:
  rules:
  - http:
      paths:
      - path: /*
        pathType: ImplementationSpecific
        backend:
          service:
            name: root-svc
            port:
              name: use-annotation
`), info, []string{})
		require.NotNil(t, info.NetworkingInfo)
		assert.ElementsMatch(t, []v1alpha1.ResourceRef{svc("demo", "canary-svc"), svc("demo", "stable-svc")}, info.NetworkingInfo.TargetRefs)
		assert.Equal(t, []v1alpha1.ResourceTargetWeight{
			{Target: svc("demo", "canary-svc"), Percentage: 30},
			{Target: svc("demo", "stable-svc"), Percentage: 70},
		}, info.NetworkingInfo.TargetWeights)
	})
	t.Run("legacy targetGroups and extensions/v1beta1", func(t *testing.T) {
		weights := targetWeightsOf(t, `
apiVersion: extensions/v1beta1
kind: Ingress
metadata:
  name: rollout-ingress
  namespace: demo
  annotations:
    alb.ingress.kubernetes.io/actions.root-svc: |
      {"Type":"forward","TargetGroups":[{"ServiceName":"canary-svc","ServicePort":"80","Weight":0},{"ServiceName":"stable-svc","ServicePort":"80","Weight":100}]}
spec:
  rules:
  - http:
      paths:
      - path: /*
        backend:
          serviceName: root-svc
          servicePort: use-annotation
`)
		assert.Equal(t, []v1alpha1.ResourceTargetWeight{
			{Target: svc("demo", "canary-svc"), Percentage: 0},
			{Target: svc("demo", "stable-svc"), Percentage: 100},
		}, weights)
	})
	t.Run("unparseable action keeps the action name as target", func(t *testing.T) {
		info := &ResourceInfo{}
		populateNodeInfo(strToUnstructured(`
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: rollout-ingress
  namespace: demo
  annotations:
    alb.ingress.kubernetes.io/actions.root-svc: not-json
spec:
  rules:
  - http:
      paths:
      - path: /*
        pathType: ImplementationSpecific
        backend:
          service:
            name: root-svc
            port:
              name: use-annotation
`), info, []string{})
		require.NotNil(t, info.NetworkingInfo)
		assert.Equal(t, []v1alpha1.ResourceRef{svc("demo", "root-svc")}, info.NetworkingInfo.TargetRefs)
		assert.Nil(t, info.NetworkingInfo.TargetWeights)
	})
}

func TestNginxCanaryIngressTargetWeights(t *testing.T) {
	canary := func(annotations string) string {
		return `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: canary-ingress
  namespace: demo
  annotations:
` + annotations + `
spec:
  ingressClassName: nginx
  rules:
  - host: app.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: canary-svc
            port:
              number: 80
`
	}
	t.Run("canary weight", func(t *testing.T) {
		assert.Equal(t, []v1alpha1.ResourceTargetWeight{{Target: svc("demo", "canary-svc"), Percentage: 15}},
			targetWeightsOf(t, canary(`    nginx.ingress.kubernetes.io/canary: "true"
    nginx.ingress.kubernetes.io/canary-weight: "15"`)))
	})
	t.Run("custom weight total", func(t *testing.T) {
		assert.Equal(t, []v1alpha1.ResourceTargetWeight{{Target: svc("demo", "canary-svc"), Percentage: 25}},
			targetWeightsOf(t, canary(`    nginx.ingress.kubernetes.io/canary: "true"
    nginx.ingress.kubernetes.io/canary-weight: "250"
    nginx.ingress.kubernetes.io/canary-weight-total: "1000"`)))
	})
	t.Run("not a canary", func(t *testing.T) {
		assert.Nil(t, targetWeightsOf(t, canary(`    nginx.ingress.kubernetes.io/canary-weight: "15"`)))
	})
}
