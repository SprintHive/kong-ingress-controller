package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"
	v1beta1 "k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/rest/fake"

	"github.com/nccurry/go-kong/kong"
)

var (
	// HTTP mux used with test server
	mux *http.ServeMux

	// Kong client being tested
	kongClient *kong.Client

	// Test server used to stub Kong resources
	server *httptest.Server

	opTimeout time.Duration

	lookupHost = func(host string) ([]string, error) { return []string{"ip-" + host}, nil }

	// Kubernetes namespace
	namespace = "default"
)

type Payload struct {
	request    interface{}
	response   interface{}
	httpMethod string
}

func TestControllerIgnoresSingleServiceIngress(t *testing.T) {
	setup()
	defer shutdown()

	unsupportedIngress := v1beta1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "somename",
			Namespace: "infra",
		},
		Spec: v1beta1.IngressSpec{
			Backend: &v1beta1.IngressBackend{
				ServiceName: "service-1",
				ServicePort: intstr.FromInt(32000),
			},
		},
	}

	// This will match everything until we add more specific handlers
	mux.HandleFunc("/apis/somename.infra", func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("No requests to Kong expected for unsupported ingress")
	})

	ingressChanged(kongClient)(&unsupportedIngress)
}
func TestControllerIgnoresIngressWithMultipleRules(t *testing.T) {
	setup()
	defer shutdown()

	unsupportedIngress := sampleIngress("somename", "infra")
	newRule := sampleIngress("somename", "infra").Spec.Rules[0]
	newRule.Host = "some.other.host"
	unsupportedIngress.Spec.Rules = append(unsupportedIngress.Spec.Rules, newRule)

	// This will match everything until we add more specific handlers
	mux.HandleFunc("/apis/somename.infra", func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("No requests to Kong expected for unsupported ingress")
	})

	ingressChanged(kongClient)(&unsupportedIngress)
}
func TestControllerIgnoresIngressWithNonRootPath(t *testing.T) {
	setup()
	defer shutdown()

	unsupportedIngress := sampleIngress("somename", "infra")
	unsupportedIngress.Spec.Rules[0].HTTP.Paths[0].Path = "/somepath"

	// This will match everything until we add more specific handlers
	mux.HandleFunc("/apis/somename.infra", func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("No requests to Kong expected for unsupported ingress")
	})

	ingressChanged(kongClient)(&unsupportedIngress)
}

func TestControllerIgnoresIngressWithMultiplePaths(t *testing.T) {
	setup()
	defer shutdown()

	unsupportedIngress := sampleIngress("somename", "infra")
	newPath := sampleIngress("somename", "infra").Spec.Rules[0].HTTP.Paths[0]
	newPath.Path = "/newpath"
	unsupportedIngress.Spec.Rules[0].HTTP.Paths = append(unsupportedIngress.Spec.Rules[0].HTTP.Paths, newPath)
	// This will match everything until we add more specific handlers
	mux.HandleFunc("/apis/somename.infra", func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("No requests to Kong expected for unsupported ingress")
	})

	ingressChanged(kongClient)(&unsupportedIngress)
}

func TestKongUpdatedOnDeletedIngress(t *testing.T) {
	setup()
	defer shutdown()
	waitGroup := sync.WaitGroup{}

	serviceName := "boringservice"
	ingress := sampleIngress(serviceName, "infra")

	waitGroup.Add(1)
	go testAPIDeleted(t, getQualifiedName(&ingress), &waitGroup)

	ingressDeleted(kongClient)(&ingress)

	waitGroup.Wait()
}

func TestKongUpdatedOnNewIngress(t *testing.T) {
	setup()
	defer shutdown()
	waitGroup := sync.WaitGroup{}

	newIngress := sampleIngress("bestservice", "prod")

	// Create API
	waitGroup.Add(1)
	go testKongOperationCalled(t, "/apis", http.MethodPost, getAPIRequestFromIngress(&newIngress), nil, &waitGroup)

	ingressChanged(kongClient)(&newIngress)
	waitGroup.Wait()
}
func TestKongUpdatedOnIngressBackendServiceUpdate(t *testing.T) {
	setup()
	defer shutdown()

	serviceName := "bestservice"
	serviceNamespace := "prod"

	originalIngress := sampleIngress(serviceName, serviceNamespace)
	qualifiedName := getQualifiedName(&originalIngress)
	newIngress := sampleIngress(serviceName, serviceNamespace)
	ingressBackend := getIngressBackend(&newIngress)
	ingressBackend.ServiceName = ingressBackend.ServiceName + "v2"

	expectedAPIPatch := kong.ApiRequest{
		ID:          qualifiedName,
		UpstreamURL: fmt.Sprintf("http://%s.%s:%s", ingressBackend.ServiceName, newIngress.ObjectMeta.Namespace, ingressBackend.ServicePort.String()),
	}

	testKongAPIPatched(t, &originalIngress, &newIngress, &expectedAPIPatch)
}
func TestKongUpdatedOnIngressBackendServicePortUpdate(t *testing.T) {
	setup()
	defer shutdown()

	serviceName := "bestservice"
	serviceNamespace := "prod"

	originalIngress := sampleIngress(serviceName, serviceNamespace)
	qualifiedName := getQualifiedName(&originalIngress)
	newIngress := sampleIngress(serviceName, serviceNamespace)
	ingressBackend := getIngressBackend(&newIngress)
	ingressBackend.ServicePort = intstr.FromInt(ingressBackend.ServicePort.IntValue() + 1)

	expectedAPIPatch := kong.ApiRequest{
		ID:          qualifiedName,
		UpstreamURL: fmt.Sprintf("http://%s.%s:%s", ingressBackend.ServiceName, newIngress.ObjectMeta.Namespace, ingressBackend.ServicePort.String()),
	}

	testKongAPIPatched(t, &originalIngress, &newIngress, &expectedAPIPatch)
}
func TestKongUpdatedOnIngressHostUpdate(t *testing.T) {
	setup()
	defer shutdown()

	serviceName := "bestservice"
	serviceNamespace := "prod"

	originalIngress := sampleIngress(serviceName, serviceNamespace)
	qualifiedName := getQualifiedName(&originalIngress)
	newIngress := sampleIngress(serviceName, serviceNamespace)
	newIngress.Spec.Rules[0].Host = "some-other-host"

	expectedAPIPatch := kong.ApiRequest{
		ID:    qualifiedName,
		Hosts: newIngress.Spec.Rules[0].Host,
	}

	testKongAPIPatched(t, &originalIngress, &newIngress, &expectedAPIPatch)
}
func TestKongReconciledWithNewIngresss(t *testing.T) {
	setup()
	defer shutdown()

	waitGroup := sync.WaitGroup{}

	sampleIngress := sampleIngress("sneakyservice", "infra")
	ingressListJSON, err := objectToJSON(v1beta1.IngressList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "IngressList",
			APIVersion: v1beta1.SchemeGroupVersion.String(),
		},
		Items: []v1beta1.Ingress{sampleIngress},
	})
	if err != nil {
		t.Fatal("Could not convert mock IngressList into JSON")
	}
	restClient, err := mockRESTClientRaw(ingressListJSON)

	// Create missing API
	waitGroup.Add(1)
	go testKongOperationCalled(t, "/apis", http.MethodPost, apiRequestFromIngress(&sampleIngress), nil, &waitGroup)

	controller := KongIngressController{restClient, kongClient}
	ctx, _ := context.WithTimeout(context.Background(), time.Millisecond*5)
	controller.createWatches(ctx)

	<-ctx.Done()
	waitGroup.Wait()
}
func TestKongReconciledWithDeletedIngresss(t *testing.T) {
	setup()
	defer shutdown()

	waitGroup := sync.WaitGroup{}

	orphanedAPI1 := "orphanedAPI1"
	orphanedAPI2 := "orphanedAPI2"
	waitGroup.Add(1)
	go testKongOperationCalled(t, "/apis", http.MethodGet, nil, kong.Apis{
		Data: []*kong.Api{
			&kong.Api{Name: orphanedAPI1},
			&kong.Api{Name: orphanedAPI2},
		},
	}, &waitGroup)
	waitGroup.Add(1)
	go testKongOperationCalledMultiple(t, "/apis/"+orphanedAPI1, []Payload{
		Payload{
			request:    nil,
			response:   kong.Api{UpstreamURL: orphanedAPI1},
			httpMethod: http.MethodGet,
		},
		Payload{
			request:    nil,
			response:   nil,
			httpMethod: http.MethodDelete,
		},
	}, &waitGroup)
	waitGroup.Add(1)
	go testKongOperationCalledMultiple(t, "/apis/"+orphanedAPI2, []Payload{
		Payload{
			request:    nil,
			response:   kong.Api{UpstreamURL: orphanedAPI2},
			httpMethod: http.MethodGet,
		},
		Payload{
			request:    nil,
			response:   nil,
			httpMethod: http.MethodDelete,
		},
	}, &waitGroup)

	restClient, err := mockRESTClient([]v1beta1.Ingress{})
	if err != nil {
		t.Fatal("Could not create rest client")
	}

	controller := KongIngressController{restClient, kongClient}
	ctx, _ := context.WithTimeout(context.Background(), time.Millisecond)
	controller.Run(ctx)

	waitGroup.Wait()
}
func TestResilienceToKongUnavailable(t *testing.T) {
	setup()
	defer shutdown()
	// This will match everything until we add more specific handlers
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})

	serviceName := "bestservice"
	testIngress := sampleIngress(serviceName, "infra")

	waitGroup := sync.WaitGroup{}

	restClient, err := mockRESTClient([]v1beta1.Ingress{testIngress})
	if err != nil {
		t.Fatal("Could not create mock REST client")
	}
	controller := KongIngressController{restClient, kongClient}
	ctx, _ := context.WithTimeout(context.Background(), time.Millisecond*1100)

	// Start controller without starting mock Kong endpoint
	go controller.Run(ctx)

	// Wait a bit to give the controller an opportunity to fail at connecting to Kong
	time.Sleep(time.Millisecond * 950)

	// Make sure the API is correct
	waitGroup.Add(1)
	go testKongOperationCalled(t, fmt.Sprintf("/apis/%s", getQualifiedName(&testIngress)), http.MethodGet, nil, apiFromIngress(&testIngress), &waitGroup)

	<-ctx.Done()
	waitGroup.Wait()
}

func testAPIDeleted(t *testing.T, apiName string, waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)

	apiDeleted := false
	mux.HandleFunc("/apis/"+apiName, func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			if !apiDeleted {
				testRequestMatches(t, request, http.MethodGet, nil)
				writeObjectResponse(t, &writer, kong.Api{
					UpstreamURL: apiName,
				})
			} else {
				writer.WriteHeader(http.StatusNotFound)
				t.Error("Tried to GET the API that was just deleted.")
			}
		case http.MethodDelete:
			apiDeleted = true
			testRequestMatches(t, request, http.MethodDelete, nil)
			cancel()
		default:
			t.Errorf("Unexpected http method '%s' used on kong /apis/ endpoint", request.Method)
		}
	})

	<-ctx.Done()
	if ctx.Err() != context.Canceled {
		t.Errorf("Kong API associated with ingress was not deleted as expected")
	}
}

func testKongAPIPatched(t *testing.T, originalIngress *v1beta1.Ingress, newIngress *v1beta1.Ingress, expectedPatch *kong.ApiRequest) {
	waitGroup := sync.WaitGroup{}

	waitGroup.Add(1)
	go testKongOperationCalledMultiple(t, fmt.Sprintf("/apis/%s", getQualifiedName(originalIngress)), []Payload{
		{
			httpMethod: http.MethodGet,
			response:   apiFromIngress(originalIngress),
		},
		{
			httpMethod: http.MethodPatch,
			request:    expectedPatch,
		}}, &waitGroup)

	ingressChanged(kongClient)(newIngress)
	waitGroup.Wait()
}

func testKongOperationCalled(t *testing.T, apiPath string, httpMethod string, expectedPayload interface{}, responsePayload interface{}, waitGroup *sync.WaitGroup) {
	testKongOperationCalledMultiple(t, apiPath, []Payload{Payload{
		request:    expectedPayload,
		response:   responsePayload,
		httpMethod: httpMethod,
	}}, waitGroup)
}

func testKongOperationCalledMultiple(t *testing.T, apiPath string, payloads []Payload, waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)

	payloadIndex := 0
	mux.HandleFunc(apiPath, func(writer http.ResponseWriter, request *http.Request) {
		if payloadIndex >= len(payloads) {
			return
		}

		payload := payloads[payloadIndex]
		payloadIndex++
		testRequestMatches(t, request, payload.httpMethod, payload.request)
		if payload.response != nil {
			writeObjectResponse(t, &writer, payload.response)
		}

		if payloadIndex == len(payloads) {
			cancel()
		}
	})

	<-ctx.Done()
	if ctx.Err() != context.Canceled {
		t.Errorf("Kong operation %s was not called as expected", apiPath)
	}
}

func testKongOperationNotCalled(t *testing.T, apiPath string, httpMethod string, waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)

	mux.HandleFunc(apiPath, func(writer http.ResponseWriter, request *http.Request) {
		if httpMethod == request.Method {
			cancel()
		}
	})

	<-ctx.Done()
	if ctx.Err() != context.DeadlineExceeded {
		t.Errorf("Kong operation %s was called unexpectedly", apiPath)
	}
}

func sampleIngress(name string, namespace string) v1beta1.Ingress {
	return v1beta1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1beta1.IngressSpec{
			Rules: []v1beta1.IngressRule{
				{
					Host: fmt.Sprintf("%s.somedomain", name),
					IngressRuleValue: v1beta1.IngressRuleValue{
						HTTP: &v1beta1.HTTPIngressRuleValue{
							Paths: []v1beta1.HTTPIngressPath{
								{
									Path: "/",
									Backend: v1beta1.IngressBackend{
										ServiceName: "service-1",
										ServicePort: intstr.FromInt(32000),
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func apiFromIngress(ingress *v1beta1.Ingress) kong.Api {
	backend := getIngressBackend(ingress)
	return kong.Api{
		UpstreamURL: fmt.Sprintf("http://%s.%s:%s", backend.ServiceName, ingress.ObjectMeta.Namespace, backend.ServicePort.String()),
		Name:        getQualifiedName(ingress),
		ID:          getQualifiedName(ingress),
	}
}

func getAPIRequestFromIngress(ingress *v1beta1.Ingress) kong.ApiRequest {
	backend := getIngressBackend(ingress)
	return kong.ApiRequest{
		UpstreamURL:  fmt.Sprintf("http://%s.%s:%s", backend.ServiceName, ingress.ObjectMeta.Namespace, backend.ServicePort.String()),
		Name:         getQualifiedName(ingress),
		Hosts:        ingress.Spec.Rules[0].Host,
		PreserveHost: true,
	}
}

func mockRESTClient(ingresses []v1beta1.Ingress) (*rest.RESTClient, error) {
	ingressList := v1beta1.IngressList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "IngressList",
			APIVersion: v1beta1.SchemeGroupVersion.String(),
		},
		Items: ingresses,
	}
	ingressListJSON, err := objectToJSON(ingressList)
	if err != nil {
		return nil, err
	}

	return mockRESTClientRaw(ingressListJSON)
}

func mockRESTClientRaw(response string) (*rest.RESTClient, error) {
	scheme := runtime.NewScheme()
	v1beta1.AddToScheme(scheme)
	config := rest.Config{}
	config.GroupVersion = &v1beta1.SchemeGroupVersion
	config.APIPath = "/apis"
	config.ContentType = runtime.ContentTypeJSON
	config.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: serializer.NewCodecFactory(scheme)}

	restClient, err := rest.RESTClientFor(&config)
	if err != nil {
		return nil, err
	}
	restClient.Client = fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
		httpResp := http.Response{
			StatusCode: 200,
			Body:       ioutil.NopCloser(strings.NewReader(response)),
			Request:    req,
		}
		return &httpResp, nil
	})

	return restClient, nil
}

func setup() {
	mux = http.NewServeMux()
	server = httptest.NewServer(mux)

	kongClient, _ = kong.NewClient(nil, server.URL)
	FullResyncInterval = time.Millisecond * 100
	opTimeout = time.Millisecond * 100
}

func shutdown() {
	server.Close()
}

func testRequestMatches(t *testing.T, request *http.Request, expectedMethod string, expectedObject interface{}) {
	if got := request.Method; got != expectedMethod {
		t.Errorf("Request method: %v, want %v at uri %v", got, expectedMethod, request.RequestURI)
	}

	if expectedObject != nil {
		bodyBytes, err := ioutil.ReadAll(request.Body)
		if err != nil {
			t.Errorf("Error reading request body: %v", err)
			return
		}
		expectedObjectJSON, err := objectToJSON(expectedObject)
		if err != nil {
			t.Errorf("Error converting expected objectin into json: %v", err)
			return
		}
		if got := string(bodyBytes); got != expectedObjectJSON {
			t.Errorf("Request body is '%s' but I want '%s'", got, expectedObjectJSON)
		}
	}
}

func writeObjectResponse(t *testing.T, writer *http.ResponseWriter, object interface{}) {
	objectJSON, err := objectToJSON(object)
	if err != nil {
		t.Errorf("Error converting expected object into json: %v", err)
		return
	}
	fmt.Fprint(*writer, objectJSON)
}

func objectToJSON(obj interface{}) (string, error) {
	var buf io.ReadWriter
	buf = new(bytes.Buffer)
	err := json.NewEncoder(buf).Encode(obj)
	if err != nil {
		return "", err
	}
	objectJSON, err := ioutil.ReadAll(buf)
	if err != nil {
		return "", err
	}

	return string(objectJSON), nil
}
