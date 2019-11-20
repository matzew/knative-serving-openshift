package servicemesh

import (
	"context"
	"errors"
	"fmt"
	"strings"

	maistrav1 "github.com/maistra/istio-operator/pkg/apis/maistra/v1"
	"github.com/openshift-knative/knative-serving-openshift/pkg"
	servingv1alpha1 "knative.dev/serving-operator/pkg/apis/serving/v1alpha1"

	mf "github.com/jcrossley3/manifestival"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"knative.dev/pkg/apis/istio/v1alpha3"
)

const (
	// ServiceMeshControlPlane name
	smcpName = "basic-install"
	// ServiceMeshMemberRole name
	smmrName       = "default"
	ownerName      = "serving.knative.openshift.io/ownerName"
	ownerNamespace = "serving.knative.openshift.io/ownerNamespace"
)

var (
	log = pkg.Log.WithName("servicemesh")
)

func ApplyServiceMesh(instance *servingv1alpha1.KnativeServing, api client.Client) error {
	log.Info("Creating namespace for service mesh")
	if err := createIngressNamespace(instance.GetNamespace(), api); err != nil {
		return err
	}
	log.Info(fmt.Sprintf("Successfully created namespace %s", ingressNamespace(instance.GetNamespace())))
	log.Info("Installing serviceMeshControlPlane")
	if err := installServiceMeshControlPlane(instance, api); err != nil {
		return err
	}
	log.Info("Successfully installed serviceMeshControlPlane")
	log.Info("Wait ServiceMeshControlPlane condition to be ready")
	// wait for serviceMeshControlPlane condition to be ready before reconciling knative serving component
	if err := isServiceMeshControlPlaneReady(instance.GetNamespace(), api); err != nil {
		return err
	}
	log.Info("ServiceMeshControlPlane is ready")
	log.Info("Installing ServiceMeshMemberRoll")
	if err := installServiceMeshMemberRoll(instance, api); err != nil {
		// ref for substring https://github.com/Maistra/istio-operator/blob/maistra-1.0/pkg/controller/servicemesh/validation/memberroll.go#L95
		if strings.Contains(err.Error(), "one or more members are already defined in another ServiceMeshMemberRoll") {
			log.Info(fmt.Sprintf("failed to update ServiceMeshMemberRole because namespace %s is already a member of another ServiceMeshMemberRoll", instance.GetNamespace()))
			msg := "Could not add '%s' to ServiceMeshMemberRoll (SMMR) because it's already part of another SMMR, " +
				"likely one in 'istio-system' (check with 'oc get smmr --all-namespaces'). " +
				"Remove '%s' and all namespaces that contain Knative Services from that other SMMR"
			return fmt.Errorf(msg, instance.GetNamespace(), instance.GetNamespace())
		}
		return err
	}
	log.Info(fmt.Sprintf("Successfully installed ServiceMeshMemberRoll and configured %s namespace", instance.GetNamespace()))
	log.Info(fmt.Sprintf("Wait ServiceMeshMemberRoll to update %s namespace into configured members", instance.GetNamespace()))
	if err := isServiceMeshMemberRollReady(instance.GetNamespace(), api); err != nil {
		return err
	}
	log.Info(fmt.Sprintf("Successfully configured %s namespace into configured members", instance.GetNamespace()))
	// TODO: instance.Status.MarkDependenciesInstalled()
	return nil
}

func RemoveServiceMesh(instance *servingv1alpha1.KnativeServing, api client.Client) error {
	log.Info("Removing service mesh")
	ns, err := getNamespaceObject(ingressNamespace(instance.GetNamespace()), api)
	if apierrors.IsNotFound(err) {
		// We can safely ignore this. There is nothing to do for us.
		return nil
	} else if err != nil {
		return err
	}
	return api.Delete(context.TODO(), ns)
}

func Configure(instance *servingv1alpha1.KnativeServing) bool {
	ns := ingressNamespace(instance.GetNamespace())
	c1 := pkg.Configure(instance, "istio", "gateway.knative-ingress-gateway", "istio-ingressgateway."+ns+".svc.cluster.local")
	c2 := pkg.Configure(instance, "istio", "local-gateway.cluster-local-gateway", "cluster-local-gateway."+ns+".svc.cluster.local")
	return c1 || c2
}

func UpdateGateway(u *unstructured.Unstructured, scheme *runtime.Scheme) error {
	if u.GetKind() == "Gateway" {
		gatewayConfig := &v1alpha3.Gateway{}
		if err := scheme.Convert(u, gatewayConfig, nil); err != nil {
			return err
		}
		gatewayConfig.Spec.Selector["maistra-control-plane"] = ingressNamespace(u.GetNamespace())
		return scheme.Convert(gatewayConfig, u, nil)
	}
	return nil
}

func WatchResources(c controller.Controller) error {
	if err := watchServiceMeshType(c, &maistrav1.ServiceMeshControlPlane{}); err != nil {
		return err
	}
	if err := watchServiceMeshType(c, &maistrav1.ServiceMeshMemberRoll{}); err != nil {
		return err
	}
	return nil
}

// isServiceMeshControlPlaneReady checks whether serviceMeshControlPlane installs all required component
func isServiceMeshControlPlaneReady(servingNamespace string, api client.Client) error {
	smcp := &maistrav1.ServiceMeshControlPlane{}
	if err := api.Get(context.TODO(), client.ObjectKey{Namespace: ingressNamespace(servingNamespace), Name: smcpName}, smcp); err != nil {
		return err
	}
	var ready = false
	for _, cond := range smcp.Status.Conditions {
		if cond.Type == maistrav1.ConditionTypeReady && cond.Status == maistrav1.ConditionStatusTrue {
			ready = true
			break
		}
	}
	if !ready {
		return errors.New("SMCP not yet ready")
	}
	return nil
}

// installServiceMeshControlPlane installs serviceMeshControlPlane
func installServiceMeshControlPlane(instance *servingv1alpha1.KnativeServing, api client.Client) error {
	const (
		path = "deploy/resources/servicemesh/smcp.yaml"
	)
	manifest, err := mf.NewManifest(path, false, api)
	if err != nil {
		log.Error(err, "Unable to create ServiceMeshControlPlane manifest")
		return err
	}
	transforms := []mf.Transformer{
		mf.InjectNamespace(ingressNamespace(instance.GetNamespace())),
		func(u *unstructured.Unstructured) error {
			u.SetLabels(map[string]string{
				ownerName:      instance.Name,
				ownerNamespace: instance.Namespace,
			})
			return nil
		},
	}
	if err := manifest.Transform(transforms...); err != nil {
		log.Error(err, "Unable to transform ServiceMeshControlPlane manifest")
		return err
	}
	if err := manifest.ApplyAll(); err != nil {
		log.Error(err, "Unable to install ServiceMeshControlPlane")
		return err
	}
	return nil
}

// installServiceMeshMemberRoll installs ServiceMeshMemberRoll for knative-serving namespace
func installServiceMeshMemberRoll(instance *servingv1alpha1.KnativeServing, api client.Client) error {
	smmr := &maistrav1.ServiceMeshMemberRoll{}
	if err := api.Get(context.TODO(), client.ObjectKey{Namespace: ingressNamespace(instance.Namespace), Name: smmrName}, smmr); err != nil {
		if apierrors.IsNotFound(err) {
			smmr.Name = smmrName
			smmr.Namespace = ingressNamespace(instance.Namespace)
			smmr.Spec.Members = []string{instance.Namespace}
			smmr.Labels = map[string]string{
				ownerName:      instance.Name,
				ownerNamespace: instance.Namespace,
			}
			return api.Create(context.TODO(), smmr)
		}
		return err
	}
	// If ServiceMeshMemberRoll already exist than check for knative-serving ns is configured member or not
	// if knative-serving ns is not configured by any chance than update existing ServiceMeshMemberRoll
	if newMembers, changed := appendIfAbsent(smmr.Spec.Members, instance.Namespace); changed {
		smmr.Spec.Members = newMembers
		return api.Update(context.TODO(), smmr)
	}
	return nil
}

// appendIfAbsent append namespace to member if its not exist
func appendIfAbsent(members []string, routeNamespace string) ([]string, bool) {
	for _, val := range members {
		if val == routeNamespace {
			return members, false
		}
	}
	return append(members, routeNamespace), true
}

// isServiceMeshMemberRoleReady Checks knative-serving namespace is a configured member or not
func isServiceMeshMemberRollReady(servingNamespace string, api client.Client) error {
	smmr := &maistrav1.ServiceMeshMemberRoll{}
	if err := api.Get(context.TODO(), client.ObjectKey{Namespace: ingressNamespace(servingNamespace), Name: smmrName}, smmr); err != nil {
		return err
	}
	for _, member := range smmr.Status.ConfiguredMembers {
		if member == servingNamespace {
			return nil
		}
	}
	return errors.New("SMMR not yet ready")
}

func watchServiceMeshType(c controller.Controller, obj runtime.Object) error {
	return c.Watch(&source.Kind{Type: obj},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: handler.ToRequestsFunc(func(a handler.MapObject) []reconcile.Request {
				if a.Meta.GetLabels()[ownerName] == "" || a.Meta.GetLabels()[ownerNamespace] == "" {
					return nil
				}
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Namespace: a.Meta.GetLabels()[ownerNamespace],
						Name:      a.Meta.GetLabels()[ownerName],
					},
				}}
			}),
		})
}

func ingressNamespace(servingNamespace string) string {
	return servingNamespace + "-ingress"
}

func createIngressNamespace(servingNamespace string, api client.Client) error {
	ns := &v1.Namespace{}
	if err := api.Get(context.TODO(), client.ObjectKey{Name: ingressNamespace(servingNamespace)}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			ns.Name = ingressNamespace(servingNamespace)
			if err = api.Create(context.TODO(), ns); err != nil {
				return err
			}
			return nil
		}
		return err
	}
	return nil
}

func getNamespaceObject(namespace string, api client.Client) (*v1.Namespace, error) {
	ns := &v1.Namespace{}
	return ns, api.Get(context.TODO(), client.ObjectKey{Name: namespace}, ns)
}