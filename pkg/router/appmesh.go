package router

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	appmeshv1 "github.com/weaveworks/flagger/pkg/apis/appmesh/v1beta1"
	flaggerv1 "github.com/weaveworks/flagger/pkg/apis/flagger/v1alpha3"
	clientset "github.com/weaveworks/flagger/pkg/client/clientset/versioned"
)

// AppMeshRouter is managing AppMesh virtual services
type AppMeshRouter struct {
	kubeClient    kubernetes.Interface
	appmeshClient clientset.Interface
	flaggerClient clientset.Interface
	logger        *zap.SugaredLogger
}

// Reconcile creates or updates App Mesh virtual nodes and virtual services
func (ar *AppMeshRouter) Reconcile(canary *flaggerv1.Canary) error {
	if canary.Spec.Service.MeshName == "" {
		return fmt.Errorf("mesh name cannot be empty")
	}

	targetName := canary.Spec.TargetRef.Name
	targetHost := fmt.Sprintf("%s.%s", targetName, canary.Namespace)
	primaryName := fmt.Sprintf("%s-primary", targetName)
	primaryHost := fmt.Sprintf("%s.%s", primaryName, canary.Namespace)
	canaryName := fmt.Sprintf("%s-canary", targetName)
	canaryHost := fmt.Sprintf("%s.%s", canaryName, canary.Namespace)

	// sync virtual node e.g. app-namespace
	// DNS app.namespace
	err := ar.reconcileVirtualNode(canary, targetName, primaryHost)
	if err != nil {
		return err
	}

	// sync virtual node e.g. app-primary-namespace
	// DNS app-primary.namespace
	err = ar.reconcileVirtualNode(canary, primaryName, primaryHost)
	if err != nil {
		return err
	}

	// sync virtual node e.g. app-canary-namespace
	// DNS app-canary.namespace
	err = ar.reconcileVirtualNode(canary, canaryName, canaryHost)
	if err != nil {
		return err
	}

	// sync main virtual service
	// DNS app.namespace
	err = ar.reconcileVirtualService(canary, targetHost, 0)
	if err != nil {
		return err
	}

	// sync canary virtual service
	// DNS app-canary.namespace
	err = ar.reconcileVirtualService(canary, fmt.Sprintf("%s.%s", canaryName, canary.Namespace), 100)
	if err != nil {
		return err
	}

	return nil
}

// reconcileVirtualNode creates or updates a virtual node
// the virtual node naming format is name-role-namespace
func (ar *AppMeshRouter) reconcileVirtualNode(canary *flaggerv1.Canary, name string, host string) error {
	protocol := getProtocol(canary)
	vnSpec := appmeshv1.VirtualNodeSpec{
		MeshName: canary.Spec.Service.MeshName,
		Listeners: []appmeshv1.Listener{
			{
				PortMapping: appmeshv1.PortMapping{
					Port:     int64(canary.Spec.Service.Port),
					Protocol: protocol,
				},
			},
		},
		ServiceDiscovery: &appmeshv1.ServiceDiscovery{
			Dns: &appmeshv1.DnsServiceDiscovery{
				HostName: host,
			},
		},
	}

	backends := []appmeshv1.Backend{}
	for _, b := range canary.Spec.Service.Backends {
		backend := appmeshv1.Backend{
			VirtualService: appmeshv1.VirtualServiceBackend{
				VirtualServiceName: b,
			},
		}
		backends = append(backends, backend)
	}
	if len(backends) > 0 {
		vnSpec.Backends = backends
	}

	virtualnode, err := ar.appmeshClient.AppmeshV1beta1().VirtualNodes(canary.Namespace).Get(name, metav1.GetOptions{})

	// create virtual node
	if errors.IsNotFound(err) {
		virtualnode = &appmeshv1.VirtualNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: canary.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(canary, schema.GroupVersionKind{
						Group:   flaggerv1.SchemeGroupVersion.Group,
						Version: flaggerv1.SchemeGroupVersion.Version,
						Kind:    flaggerv1.CanaryKind,
					}),
				},
			},
			Spec: vnSpec,
		}
		_, err = ar.appmeshClient.AppmeshV1beta1().VirtualNodes(canary.Namespace).Create(virtualnode)
		if err != nil {
			return fmt.Errorf("VirtualNode %s.%s create error %v", name, canary.Namespace, err)
		}
		ar.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
			Infof("VirtualNode %s.%s created", virtualnode.GetName(), canary.Namespace)
		return nil
	}

	if err != nil {
		return fmt.Errorf("VirtualNode %s query error %v", name, err)
	}

	// update virtual node
	if virtualnode != nil {
		if diff := cmp.Diff(vnSpec, virtualnode.Spec); diff != "" {
			vnClone := virtualnode.DeepCopy()
			vnClone.Spec = vnSpec
			_, err = ar.appmeshClient.AppmeshV1beta1().VirtualNodes(canary.Namespace).Update(vnClone)
			if err != nil {
				return fmt.Errorf("VirtualNode %s update error %v", name, err)
			}
			ar.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
				Infof("VirtualNode %s updated", virtualnode.GetName())
		}
	}

	return nil
}

// reconcileVirtualService creates or updates a virtual service
func (ar *AppMeshRouter) reconcileVirtualService(canary *flaggerv1.Canary, name string, canaryWeight int64) error {
	targetName := canary.Spec.TargetRef.Name
	canaryVirtualNode := fmt.Sprintf("%s-canary", targetName)
	primaryVirtualNode := fmt.Sprintf("%s-primary", targetName)
	protocol := getProtocol(canary)

	routerName := targetName
	if canaryWeight > 0 {
		routerName = fmt.Sprintf("%s-canary", targetName)
	}
	// App Mesh supports only URI prefix
	routePrefix := "/"
	if len(canary.Spec.Service.Match) > 0 &&
		canary.Spec.Service.Match[0].Uri != nil &&
		canary.Spec.Service.Match[0].Uri.Prefix != "" {
		routePrefix = canary.Spec.Service.Match[0].Uri.Prefix
	}

	// Canary progressive traffic shift
	routes := []appmeshv1.Route{
		{
			Name: routerName,
			Http: &appmeshv1.HttpRoute{
				Match: appmeshv1.HttpRouteMatch{
					Prefix: routePrefix,
				},
				RetryPolicy: makeRetryPolicy(canary),
				Action: appmeshv1.HttpRouteAction{
					WeightedTargets: []appmeshv1.WeightedTarget{
						{
							VirtualNodeName: canaryVirtualNode,
							Weight:          canaryWeight,
						},
						{
							VirtualNodeName: primaryVirtualNode,
							Weight:          100 - canaryWeight,
						},
					},
				},
			},
		},
	}

	// A/B testing - header based routing
	if len(canary.Spec.CanaryAnalysis.Match) > 0 && canaryWeight == 0 {
		routes = []appmeshv1.Route{
			{
				Name:     fmt.Sprintf("%s-a", targetName),
				Priority: int64p(10),
				Http: &appmeshv1.HttpRoute{
					Match: appmeshv1.HttpRouteMatch{
						Prefix:  routePrefix,
						Headers: makeHeaders(canary),
					},
					RetryPolicy: makeRetryPolicy(canary),
					Action: appmeshv1.HttpRouteAction{
						WeightedTargets: []appmeshv1.WeightedTarget{
							{
								VirtualNodeName: canaryVirtualNode,
								Weight:          canaryWeight,
							},
							{
								VirtualNodeName: primaryVirtualNode,
								Weight:          100 - canaryWeight,
							},
						},
					},
				},
			},
			{
				Name:     fmt.Sprintf("%s-b", targetName),
				Priority: int64p(20),
				Http: &appmeshv1.HttpRoute{
					Match: appmeshv1.HttpRouteMatch{
						Prefix: routePrefix,
					},
					RetryPolicy: makeRetryPolicy(canary),
					Action: appmeshv1.HttpRouteAction{
						WeightedTargets: []appmeshv1.WeightedTarget{
							{
								VirtualNodeName: primaryVirtualNode,
								Weight:          100,
							},
						},
					},
				},
			},
		}
	}

	vsSpec := appmeshv1.VirtualServiceSpec{
		MeshName: canary.Spec.Service.MeshName,
		VirtualRouter: &appmeshv1.VirtualRouter{
			Name: routerName,
			Listeners: []appmeshv1.VirtualRouterListener{
				{
					PortMapping: appmeshv1.PortMapping{
						Port:     int64(canary.Spec.Service.Port),
						Protocol: protocol,
					},
				},
			},
		},
		Routes: routes,
	}

	virtualService, err := ar.appmeshClient.AppmeshV1beta1().VirtualServices(canary.Namespace).Get(name, metav1.GetOptions{})

	// create virtual service
	if errors.IsNotFound(err) {
		virtualService = &appmeshv1.VirtualService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: canary.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(canary, schema.GroupVersionKind{
						Group:   flaggerv1.SchemeGroupVersion.Group,
						Version: flaggerv1.SchemeGroupVersion.Version,
						Kind:    flaggerv1.CanaryKind,
					}),
				},
			},
			Spec: vsSpec,
		}
		_, err = ar.appmeshClient.AppmeshV1beta1().VirtualServices(canary.Namespace).Create(virtualService)
		if err != nil {
			return fmt.Errorf("VirtualService %s create error %v", name, err)
		}
		ar.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
			Infof("VirtualService %s created", virtualService.GetName())
		return nil
	}

	if err != nil {
		return fmt.Errorf("VirtualService %s query error %v", name, err)
	}

	// update virtual service but keep the original target weights
	if virtualService != nil {
		if diff := cmp.Diff(vsSpec, virtualService.Spec, cmpopts.IgnoreTypes(appmeshv1.WeightedTarget{})); diff != "" {
			vsClone := virtualService.DeepCopy()
			vsClone.Spec = vsSpec
			vsClone.Spec.Routes[0].Http.Action = virtualService.Spec.Routes[0].Http.Action

			_, err = ar.appmeshClient.AppmeshV1beta1().VirtualServices(canary.Namespace).Update(vsClone)
			if err != nil {
				return fmt.Errorf("VirtualService %s update error %v", name, err)
			}
			ar.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
				Infof("VirtualService %s updated", virtualService.GetName())
		}
	}

	return nil
}

// GetRoutes returns the destinations weight for primary and canary
func (ar *AppMeshRouter) GetRoutes(canary *flaggerv1.Canary) (
	primaryWeight int,
	canaryWeight int,
	mirrored bool,
	err error,
) {
	targetName := canary.Spec.TargetRef.Name
	vsName := fmt.Sprintf("%s.%s", targetName, canary.Namespace)
	vs, err := ar.appmeshClient.AppmeshV1beta1().VirtualServices(canary.Namespace).Get(vsName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			err = fmt.Errorf("VirtualService %s not found", vsName)
			return
		}
		err = fmt.Errorf("VirtualService %s query error %v", vsName, err)
		return
	}

	if len(vs.Spec.Routes) < 1 || len(vs.Spec.Routes[0].Http.Action.WeightedTargets) != 2 {
		err = fmt.Errorf("VirtualService routes %s not found", vsName)
		return
	}

	targets := vs.Spec.Routes[0].Http.Action.WeightedTargets
	for _, t := range targets {
		if t.VirtualNodeName == fmt.Sprintf("%s-canary", targetName) {
			canaryWeight = int(t.Weight)
		}
		if t.VirtualNodeName == fmt.Sprintf("%s-primary", targetName) {
			primaryWeight = int(t.Weight)
		}
	}

	if primaryWeight == 0 && canaryWeight == 0 {
		err = fmt.Errorf("VirtualService %s does not contain routes for %s-primary and %s-canary",
			vsName, targetName, targetName)
	}

	mirrored = false

	return
}

// SetRoutes updates the destinations weight for primary and canary
func (ar *AppMeshRouter) SetRoutes(
	canary *flaggerv1.Canary,
	primaryWeight int,
	canaryWeight int,
	mirrored bool,
) error {
	targetName := canary.Spec.TargetRef.Name
	vsName := fmt.Sprintf("%s.%s", targetName, canary.Namespace)
	vs, err := ar.appmeshClient.AppmeshV1beta1().VirtualServices(canary.Namespace).Get(vsName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("VirtualService %s not found", vsName)
		}
		return fmt.Errorf("VirtualService %s query error %v", vsName, err)
	}

	vsClone := vs.DeepCopy()
	vsClone.Spec.Routes[0].Http.Action = appmeshv1.HttpRouteAction{
		WeightedTargets: []appmeshv1.WeightedTarget{
			{
				VirtualNodeName: fmt.Sprintf("%s-canary", targetName),
				Weight:          int64(canaryWeight),
			},
			{
				VirtualNodeName: fmt.Sprintf("%s-primary", targetName),
				Weight:          int64(primaryWeight),
			},
		},
	}

	_, err = ar.appmeshClient.AppmeshV1beta1().VirtualServices(canary.Namespace).Update(vsClone)
	if err != nil {
		return fmt.Errorf("VirtualService %s update error %v", vsName, err)
	}

	return nil
}

// makeRetryPolicy creates an App Mesh HttpRetryPolicy from the Canary.Service.Retries
// default: one retry on gateway error with a 250ms timeout
func makeRetryPolicy(canary *flaggerv1.Canary) *appmeshv1.HttpRetryPolicy {
	if canary.Spec.Service.Retries != nil {
		timeout := int64(250)
		if d, err := time.ParseDuration(canary.Spec.Service.Retries.PerTryTimeout); err == nil {
			timeout = d.Milliseconds()
		}

		attempts := int64(1)
		if canary.Spec.Service.Retries.Attempts > 0 {
			attempts = int64(canary.Spec.Service.Retries.Attempts)
		}

		retryPolicy := &appmeshv1.HttpRetryPolicy{
			PerRetryTimeoutMillis: int64p(timeout),
			MaxRetries:            int64p(attempts),
		}

		events := []string{"gateway-error"}
		if len(canary.Spec.Service.Retries.RetryOn) > 0 {
			events = strings.Split(canary.Spec.Service.Retries.RetryOn, ",")
		}
		for _, value := range events {
			retryPolicy.HttpRetryPolicyEvents = append(retryPolicy.HttpRetryPolicyEvents, appmeshv1.HttpRetryPolicyEvent(value))
		}
		return retryPolicy
	}

	return nil
}

// makeRetryPolicy creates an App Mesh HttpRouteHeader from the Canary.CanaryAnalysis.Match
func makeHeaders(canary *flaggerv1.Canary) []appmeshv1.HttpRouteHeader {
	headers := []appmeshv1.HttpRouteHeader{}

	for _, m := range canary.Spec.CanaryAnalysis.Match {
		for key, value := range m.Headers {
			header := appmeshv1.HttpRouteHeader{
				Name: key,
				Match: &appmeshv1.HeaderMatchMethod{
					Exact:  stringp(value.Exact),
					Prefix: stringp(value.Prefix),
					Regex:  stringp(value.Regex),
					Suffix: stringp(value.Suffix),
				},
			}
			headers = append(headers, header)
		}
	}

	return headers
}

func getProtocol(canary *flaggerv1.Canary) string {
	if strings.Contains(canary.Spec.Service.PortName, "grpc") {
		return "grpc"
	}
	return "http"
}

func int64p(i int64) *int64 {
	return &i
}

func stringp(s string) *string {
	if s != "" {
		return &s
	}
	return nil
}
