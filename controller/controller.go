package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"

	"github.com/golang/glog"
	"github.com/nccurry/go-kong/kong"
	"github.com/pkg/errors"
)

// KongIngressController watches ingress updates and makes corresponding changes to the service proxy
type KongIngressController struct {
	IngressClient cache.Getter
	KongClient    *kong.Client
}

// New returns an instance of a KongIngressController
func New(ingressClient cache.Getter, kongClient *kong.Client) *KongIngressController {
	return &KongIngressController{
		ingressClient,
		kongClient,
	}
}

// FullResyncInterval determines how often a a full reconciliation of the kong and ingress configurations is done
var FullResyncInterval = time.Minute

// Run starts the KongIngressController
func (controller *KongIngressController) Run(ctx context.Context) error {
	glog.Infof("Starting watch for Ingress updates")

	_, err := controller.createWatches(ctx)
	if err != nil {
		return errors.Wrap(err, "Failed to register watchers for Ingress resources")
	}

	go apiReaper(ctx, controller)

	<-ctx.Done()
	return ctx.Err()
}

func apiReaper(ctx context.Context, controller *KongIngressController) {
	glog.Info("Reaper: watching for orphaned apis to kill")

	for {
		glog.V(2).Info("Reaper: Looking for orphaned apis to kill...")
		select {
		case <-ctx.Done():
			return
		default:
			err := reapOrphanedApis(controller.KongClient, controller.IngressClient)
			if err != nil {
				glog.Errorf("Failed to reap orphaned kong apis: %v", err)
			}
		}

		glog.V(2).Info("Reaper: Finished reap cycle")
		time.Sleep(FullResyncInterval)
	}
}

func reapOrphanedApis(kongClient *kong.Client, ingressClient cache.Getter) error {
	kongApis, _, err := kongClient.Apis.GetAll(nil)
	if err != nil {
		return errors.Wrapf(err, "Failed to get kong api list")
	}

	ingressObjects, err := ingressClient.
		Get().
		Namespace(metav1.NamespaceAll).
		Resource("ingresses").
		Do().
		Get()
	if err != nil {
		return errors.Wrapf(err, "Failed to get ingress list")
	}

	ingressList := ingressObjects.(*v1beta1.IngressList)
	ingMap := map[string]bool{}
	for _, ingress := range ingressList.Items {
		ingMap[getQualifiedName(&ingress)] = true
	}

	for _, api := range kongApis.Data {
		if !ingMap[api.Name] {
			err := deleteKongAPI(kongClient, api.Name)
			if err != nil {
				glog.Errorf("Error reaping orphaned kong api '%s': %v", api.Name, err)
			} else {
				glog.Infof("Reaper: Die, die, die! Orphaned kong api '%s' was reaped", api.Name)
			}
		}
	}

	return nil
}

func (controller *KongIngressController) createWatches(ctx context.Context) (cache.Controller, error) {
	watchedSource := cache.NewListWatchFromClient(
		controller.IngressClient,
		"ingresses",
		metav1.NamespaceAll,
		fields.Everything())

	_, informController := cache.NewInformer(
		watchedSource,
		&v1beta1.Ingress{},
		FullResyncInterval,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    ingressChanged(controller.KongClient),
			UpdateFunc: ingressUpdated(controller.KongClient),
			DeleteFunc: ingressDeleted(controller.KongClient),
		},
	)

	go informController.Run(ctx.Done())
	return informController, nil
}

func ingressChanged(kongClient *kong.Client) func(interface{}) {
	return func(obj interface{}) {
		ingress := obj.(*v1beta1.Ingress)

		if err := validateIngressSupported(ingress); err != nil {
			glog.Errorf("Unsupported ingress '%s' in namespace '%s': %v", ingress.ObjectMeta.Name, ingress.ObjectMeta.ClusterName, err)
			return
		}

		glog.V(2).Infof("Reconciling Ingress '%s' in namespace '%s' with Kong API", ingress.ObjectMeta.Name, ingress.ObjectMeta.Namespace)
		err := reconcileAPI(kongClient, ingress)
		if err != nil {
			glog.Errorf("An error occurred attempting to create or update API '%s': %v", getQualifiedName(ingress), err)
			return
		}
	}
}

func reconcileAPI(kongClient *kong.Client, ingress *v1beta1.Ingress) error {
	apiName := getQualifiedName(ingress)

	api, resp, err := kongClient.Apis.Get(apiName)
	if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
		return errors.Wrapf(err, "Failed to fetch API '%s'", apiName)
	}

	if resp.StatusCode == http.StatusNotFound {
		glog.Infof("Creating new API '%s'", apiName)
		kongAPI := apiRequestFromIngress(ingress)
		_, err := kongClient.Apis.Post(&kongAPI)
		if err != nil {
			return errors.Wrapf(err, "Failed to create API '%s'", apiName)
		}
	} else {
		correctUpstreamURL := getUpstreamURL(ingress)
		if api.UpstreamURL != correctUpstreamURL {
			glog.Infof("Updating upstream URL from '%s' to '%s' on API '%s'", api.UpstreamURL, correctUpstreamURL, api.Name)
			_, err := kongClient.Apis.Patch(&kong.ApiRequest{
				ID:          api.ID,
				UpstreamURL: correctUpstreamURL,
			})
			if err != nil {
				return errors.Wrapf(err, "Failed to patch API '%s'", apiName)
			}
		}
		correctHosts := ingress.Spec.Rules[0].Host
		if len(api.Hosts) != 1 || api.Hosts[0] != correctHosts {
			glog.Infof("Updating Hosts from '%s' to '%s' on API '%s'", api.Hosts, correctHosts, api.Name)
			_, err := kongClient.Apis.Patch(&kong.ApiRequest{
				ID:    api.ID,
				Hosts: correctHosts,
			})
			if err != nil {
				return errors.Wrapf(err, "Failed to patch API '%s'", apiName)
			}
		}
		if api.PreserveHost != true {
			glog.Infof("Updating PreserveHost from '%s' to '%s' on API '%s'", false, true, api.Name)
			_, err := kongClient.Apis.Patch(&kong.ApiRequest{
				ID:           api.ID,
				PreserveHost: true,
			})
			if err != nil {
				return errors.Wrapf(err, "Failed to patch API '%s'", apiName)
			}
		}
	}

	return nil
}

func ingressUpdated(kongClient *kong.Client) func(interface{}, interface{}) {
	return func(previousObj, newObj interface{}) {
		ingressChanged(kongClient)(newObj)
	}
}

func ingressDeleted(kongClient *kong.Client) func(interface{}) {
	return func(obj interface{}) {
		ingress := obj.(*v1beta1.Ingress)
		glog.Infof("Ingress '%s' was deleted from namespace '%s'. Removing it from Kong.", ingress.ObjectMeta.Name, ingress.ObjectMeta.Namespace)
		apiName := getQualifiedName(ingress)
		err := deleteKongAPI(kongClient, apiName)
		if err != nil {
			glog.Errorf("Failed to delete kong API '%s': %v", apiName, err)
		}
	}
}

func deleteKongAPI(kongClient *kong.Client, apiName string) error {
	_, _, err := kongClient.Apis.Get(apiName)
	if err != nil {
		return errors.Wrapf(err, "Failed to retrieve kong api '%s'", apiName)
	}

	_, err = kongClient.Apis.Delete(apiName)
	if err != nil {
		return errors.Wrapf(err, "Failed to delete kong api '%s'", apiName)
	}
	glog.Infof("Kong api '%s' was deleted", apiName)

	return nil
}

func validateIngressSupported(ingress *v1beta1.Ingress) error {
	if ingress.Spec.Backend != nil {
		return errors.New("Single Service Ingress types are not currently supported")
	}
	if len(ingress.Spec.Rules) != 1 {
		return errors.New("Only ingresses with a single rule are currently supported")
	}
	if len(ingress.Spec.Rules[0].HTTP.Paths) != 1 || ingress.Spec.Rules[0].HTTP.Paths[0].Path != "/" {
		return errors.New("Only ingresses with a single root path are currently supported")
	}

	return nil
}

func apiRequestFromIngress(ingress *v1beta1.Ingress) kong.ApiRequest {
	serviceName := getQualifiedName(ingress)
	upstreamURL := getUpstreamURL(ingress)
	return kong.ApiRequest{
		UpstreamURL:  upstreamURL,
		Name:         serviceName,
		Hosts:        ingress.Spec.Rules[0].Host,
		PreserveHost: true,
	}
}

func getUpstreamURL(ingress *v1beta1.Ingress) string {
	backend := getIngressBackend(ingress)
	return fmt.Sprintf("http://%s.%s:%s", backend.ServiceName, ingress.ObjectMeta.Namespace, backend.ServicePort.String())
}

func getQualifiedName(ingress *v1beta1.Ingress) string {
	return fmt.Sprintf("%s.%s", ingress.ObjectMeta.Name, ingress.ObjectMeta.Namespace)
}

func getIngressBackend(ingress *v1beta1.Ingress) *v1beta1.IngressBackend {
	return &ingress.Spec.Rules[0].HTTP.Paths[0].Backend
}
