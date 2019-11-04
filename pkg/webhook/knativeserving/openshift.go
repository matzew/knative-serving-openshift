package knativeserving

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/coreos/go-semver/semver"
	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	servingv1alpha1 "knative.dev/serving-operator/pkg/apis/serving/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// configure egress
func (a *KnativeServingConfigurator) egress(ctx context.Context, ks *servingv1alpha1.KnativeServing) error {
	networkConfig := &configv1.Network{}
	if err := a.client.Get(ctx, client.ObjectKey{Name: "cluster"}, networkConfig); err != nil {
		if !meta.IsNoMatchError(err) {
			return err
		}
		log.Info("No OpenShift cluster network config available")
		return nil
	}
	network := strings.Join(networkConfig.Spec.ServiceNetwork, ",")
	return configure(ks, "network", "istio.sidecar.includeOutboundIPRanges", network)
}

// configure ingress
func (a *KnativeServingConfigurator) ingress(ctx context.Context, ks *servingv1alpha1.KnativeServing) error {
	ingressConfig := &configv1.Ingress{}
	if err := a.client.Get(ctx, client.ObjectKey{Name: "cluster"}, ingressConfig); err != nil {
		if !meta.IsNoMatchError(err) {
			return err
		}
		log.Info("No OpenShift ingress config available")
		return nil
	}
	domain := ingressConfig.Spec.Domain
	if len(domain) > 0 {
		return configure(ks, "domain", domain, "")
	}
	return nil
}

// configure observability if ClusterLogging is installed
func (a *KnativeServingConfigurator) configureLogURLTemplate(ctx context.Context, ks *servingv1alpha1.KnativeServing) error {
	const (
		configmap = "observability"
		key       = "logging.revision-url-template"
		name      = "kibana"
		namespace = "openshift-logging"
	)
	// attempt to locate kibana route which is available if openshift-logging has been configured
	route := &routev1.Route{}
	if err := a.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, route); err != nil {
		log.Info(fmt.Sprintf("No revision-url-template; no route for %s/%s found", namespace, name))
		return nil
	}
	// retrieve host from kibana route, construct a concrete logUrl template with actual host name, update observability
	if len(route.Status.Ingress) > 0 {
		host := route.Status.Ingress[0].Host
		if host != "" {
			url := "https://" + host + "/app/kibana#/discover?_a=(index:.all,query:'kubernetes.labels.serving_knative_dev%5C%2FrevisionUID:${REVISION_UID}')"
			return configure(ks, configmap, key, url)
		}
	}
	return nil
}

// validate minimum openshift version
func (v *KnativeServingValidator) validateVersion(ctx context.Context, ks *servingv1alpha1.KnativeServing) (bool, string, error) {
	minVersion, err := semver.NewVersion(os.Getenv("MIN_OPENSHIFT_VERSION"))
	if err != nil {
		return false, "Unable to validate version; check MIN_OPENSHIFT_VERSION env var", nil
	}

	clusterVersion := &configv1.ClusterVersion{}
	if err := v.client.Get(ctx, client.ObjectKey{Name: "version"}, clusterVersion); err != nil {
		return false, "Unable to get ClusterVersion", err
	}

	current, err := semver.NewVersion(clusterVersion.Status.Desired.Version)
	if err != nil {
		return false, "Could not parse version string", err
	}

	if current.Major == 0 && current.Minor == 0 {
		return true, "CI build detected, bypassing version check", nil
	}

	if current.LessThan(*minVersion) {
		msg := fmt.Sprintf("Version constraint not fulfilled: minimum version: %s, current version: %s", minVersion.String(), current.String())
		return false, msg, nil
	}
	return true, "", nil
}

// validate required namespace, if any
func (v *KnativeServingValidator) validateNamespace(ctx context.Context, ks *servingv1alpha1.KnativeServing) (bool, string, error) {
	ns, required := os.LookupEnv("REQUIRED_NAMESPACE")
	if required && ns != ks.Namespace {
		return false, fmt.Sprintf("KnativeServing may only be created in %s namespace", ns), nil
	}
	return true, "", nil
}
