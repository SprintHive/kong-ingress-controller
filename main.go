package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/SprintHive/kong-ingress-controller/controller"
	"github.com/nccurry/go-kong/kong"
)

func main() {
	var config *rest.Config
	var kubeConfig *string
	var err error
	externalAPIAccess := flag.Bool("externalapi", false, "connect to the API from outside the kubernetes cluster")
	kongAPIAddress := flag.String("kongaddress", "http://kong-admin:8001", "address of the kong API server")
	if home := homeDir(); home != "" {
		kubeConfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeConfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	flag.Parse()

	if *externalAPIAccess {
		// use the current context in kubeConfig
		config, err = clientcmd.BuildConfigFromFlags("", *kubeConfig)
		if err != nil {
			panic(err.Error())
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
	}

	clientSet, err := kubernetes.NewForConfig(config)
	ingClient := clientSet.ExtensionsV1beta1().RESTClient()
	if err != nil {
		panic(err.Error())
	}

	// Create Kong client
	kongClient, err := kong.NewClient(nil, *kongAPIAddress)
	if err != nil {
		panic(err.Error())
	}

	ingController := controller.New(ingClient, kongClient)

	ctx := context.Background()
	go ingController.Run(ctx)

	<-ctx.Done()
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}
