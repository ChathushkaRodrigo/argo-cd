package cache

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/utils/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const (
	albActionsAnnotationPrefix = "alb.ingress.kubernetes.io/actions."
	albUseAnnotationPort       = "use-annotation"

	nginxCanaryAnnotation            = "nginx.ingress.kubernetes.io/canary"
	nginxCanaryWeightAnnotation      = "nginx.ingress.kubernetes.io/canary-weight"
	nginxCanaryWeightTotalAnnotation = "nginx.ingress.kubernetes.io/canary-weight-total"
	nginxDefaultCanaryWeightTotal    = 100
)

// targetPercentages turns the weights of the targets of a single traffic split into each target's percentage of
// the total. It returns nil unless the traffic is split between at least two targets with a positive total weight.
func targetPercentages(weights map[v1alpha1.ResourceRef]int64) []v1alpha1.ResourceTargetWeight {
	if len(weights) < 2 {
		return nil
	}
	var total int64
	for _, weight := range weights {
		if weight < 0 {
			return nil
		}
		total += weight
	}
	if total <= 0 {
		return nil
	}
	result := make([]v1alpha1.ResourceTargetWeight, 0, len(weights))
	for target, weight := range weights {
		result = append(result, v1alpha1.ResourceTargetWeight{
			Target:     target,
			Percentage: int64(math.Round(float64(weight) * 100 / float64(total))),
		})
	}
	sortTargetWeights(result)
	return result
}

func sortTargetWeights(weights []v1alpha1.ResourceTargetWeight) {
	sort.Slice(weights, func(i, j int) bool {
		a, b := weights[i].Target, weights[j].Target
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
}

// isDefaultIstioHTTPRoute reports whether an Istio VirtualService http rule applies to all requests,
// i.e. it has no match conditions.
func isDefaultIstioHTTPRoute(rule map[string]any) bool {
	matches, ok := rule["match"].([]any)
	return !ok || len(matches) == 0
}

// istioRouteWeight returns the weight of an Istio http route destination. Istio sends all traffic to a destination
// without a weight when it is the only destination of the rule.
func istioRouteWeight(route map[string]any, numRoutes int) int64 {
	if weight, ok := weightField(route); ok {
		return weight
	}
	if numRoutes == 1 {
		return 100
	}
	return 0
}

// weightField reads the "weight" field of a route destination or backend. Objects decoded from JSON hold whole
// numbers as int64, but objects built from YAML or by other decoders can hold them as float64 or int.
func weightField(obj map[string]any) (int64, bool) {
	switch weight := obj["weight"].(type) {
	case int64:
		return weight, true
	case int:
		return int64(weight), true
	case int32:
		return int64(weight), true
	case float64:
		if weight == math.Trunc(weight) {
			return int64(weight), true
		}
	}
	return 0, false
}

// isDefaultGatewayAPIRouteRule reports whether a Gateway API route rule applies to all requests: it has no matches,
// or its only match is the default PathPrefix "/" match.
func isDefaultGatewayAPIRouteRule(rule map[string]any) bool {
	matches, ok := rule["matches"].([]any)
	if !ok || len(matches) == 0 {
		return true
	}
	if len(matches) != 1 {
		return false
	}
	match, ok := matches[0].(map[string]any)
	if !ok {
		return false
	}
	for key := range match {
		if key != "path" {
			return false
		}
	}
	pathType, _, _ := unstructured.NestedString(match, "path", "type")
	pathValue, _, _ := unstructured.NestedString(match, "path", "value")
	return (pathType == "" || pathType == "PathPrefix") && (pathValue == "" || pathValue == "/")
}

// gatewayAPIBackendWeight returns the weight of a Gateway API backendRef, which defaults to 1.
func gatewayAPIBackendWeight(backend map[string]any) int64 {
	if weight, ok := weightField(backend); ok {
		return weight
	}
	return 1
}

// isALBActionBackend reports whether an Ingress backend refers to an AWS Load Balancer Controller action
// annotation instead of a Service port.
func isALBActionBackend(backend map[string]any, gvk schema.GroupVersionKind) bool {
	if gvk.Group == "networking.k8s.io" && gvk.Version == "v1" {
		portName, _, _ := unstructured.NestedString(backend, "service", "port", "name")
		return portName == albUseAnnotationPort
	}
	servicePort, _, _ := unstructured.NestedString(backend, "servicePort")
	return servicePort == albUseAnnotationPort
}

type albTargetGroup struct {
	ServiceName string `json:"serviceName"`
	Weight      *int64 `json:"weight"`
}

type albAction struct {
	Type          string `json:"type"`
	ForwardConfig *struct {
		TargetGroups []albTargetGroup `json:"targetGroups"`
	} `json:"forwardConfig"`
	// TargetGroups is the older way to configure a forward action, kept by the AWS Load Balancer Controller.
	TargetGroups []albTargetGroup `json:"targetGroups"`
}

// albForwardServiceWeights returns the Services, and their weights, that the forward action named actionName in
// the annotations of an Ingress sends traffic to. It returns false if there is no such forward action, or if it
// doesn't refer to any Service (for example when it uses target group ARNs).
func albForwardServiceWeights(un *unstructured.Unstructured, actionName string) (map[string]int64, bool) {
	value, ok := un.GetAnnotations()[albActionsAnnotationPrefix+actionName]
	if !ok {
		return nil, false
	}
	var action albAction
	if err := json.Unmarshal([]byte(value), &action); err != nil || action.Type != "forward" {
		return nil, false
	}
	targetGroups := action.TargetGroups
	if action.ForwardConfig != nil {
		targetGroups = action.ForwardConfig.TargetGroups
	}
	weights := make(map[string]int64)
	for _, targetGroup := range targetGroups {
		if targetGroup.ServiceName == "" {
			continue
		}
		weight := int64(1)
		if targetGroup.Weight != nil {
			weight = *targetGroup.Weight
		}
		weights[targetGroup.ServiceName] += weight
	}
	return weights, len(weights) > 0
}

// nginxCanaryPercentage returns the percentage of the traffic that an NGINX canary Ingress sends to its backend,
// from its canary-weight and canary-weight-total annotations. It returns false if the Ingress isn't a weighted
// NGINX canary Ingress.
func nginxCanaryPercentage(un *unstructured.Unstructured) (int64, bool) {
	annotations := un.GetAnnotations()
	if annotations[nginxCanaryAnnotation] != "true" {
		return 0, false
	}
	weight, err := strconv.ParseInt(annotations[nginxCanaryWeightAnnotation], 10, 64)
	if err != nil || weight < 0 {
		return 0, false
	}
	total := int64(nginxDefaultCanaryWeightTotal)
	if value, ok := annotations[nginxCanaryWeightTotalAnnotation]; ok {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 {
			total = parsed
		}
	}
	percentage := int64(math.Round(float64(weight) * 100 / float64(total)))
	return min(percentage, 100), true
}

func serviceRef(namespace, name string) v1alpha1.ResourceRef {
	return v1alpha1.ResourceRef{Group: "", Kind: kube.ServiceKind, Namespace: namespace, Name: name}
}
