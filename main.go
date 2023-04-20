/*
Copyright © 2023 NAME HERE <EMAIL ADDRESS>
*/
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/metrics/pkg/client/clientset/versioned"
)

//===========================---global-var---========================

var (
	namespace = os.Getenv("POD_NAMESPACE")
	interval  = time.Duration(5) * time.Second
)

//==============================---main---============================

func main() {

	// Create the Kubernetes clientset.
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)

	// get config
	config, err := kubeconfig.ClientConfig()
	if err != nil {
		panic(err.Error())
	}

	// get clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	//get metrics clientset for metrics collection
	metricsClientset, err := versioned.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// Create the Prometheus-metrics, where our metrics send to
	serviceBytesReceived := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "service_network_bytes_received",
		Help: "Total number of bytes received by a service.",
	}, []string{"namespace", "service"})

	serviceBytesTransmitted := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "service_network_bytes_transmitted",
		Help: "Total number of bytes transmitted by a service.",
	}, []string{"namespace", "service"})

	prometheus.MustRegister(serviceBytesReceived)
	prometheus.MustRegister(serviceBytesTransmitted)

	// Create the informer factory and informer.
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset, //clientset used to communicate with api server
		interval,  // resync period
		informers.WithNamespace(namespace),
	)
	serviceInformer := informerFactory.Core().V1().Services().Informer()

	// Define the event handlers.
	serviceEventHandler := cache.ResourceEventHandlerFuncs{
		// when a service is added, start to collect metrics for that service
		AddFunc: func(obj interface{}) {
			service := obj.(*corev1.Service)
			go collectServiceNetworkMetrics(clientset, metricsClientset, service, serviceBytesReceived, serviceBytesTransmitted)
		},
		// when a service is updated, start to collect for the service with new status
		UpdateFunc: func(oldObj, newObj interface{}) {
			service := newObj.(*corev1.Service)
			go collectServiceNetworkMetrics(clientset, metricsClientset, service, serviceBytesReceived, serviceBytesTransmitted)
		},
		// the collecting function stops itself when the service can not find in clientset, so do not need action on service deletion
		DeleteFunc: func(obj interface{}) {},
	}

	// Register the event handlers with the informer.
	serviceInformer.AddEventHandler(serviceEventHandler)

	// Start the informer and wait for it to sync.
	informerFactory.Start(wait.NeverStop)
	if !cache.WaitForCacheSync(wait.NeverStop, serviceInformer.HasSynced) {
		panic("failed to synchronize informers")
	}

	// Start the HTTP server to expose the Prometheus metrics.
	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":8080", nil)

}

//==============================---Collect-Service-Network-Metrics---============================

// TO DO: make the function a neverstop loop, fetch and calulate metrics periodically and stop when the service does not exist anymore
func collectServiceNetworkMetrics(clientset kubernetes.Interface, metricsClientset versioned.Interface, service *corev1.Service, serviceBytesReceived *prometheus.GaugeVec, serviceBytesTransmitted *prometheus.GaugeVec) {

	// Create the Prometheus labels.
	labels := prometheus.Labels{
		"namespace": service.Namespace,
		"service":   service.Name,
	}

	// Get the total number of bytes received by the service.
	bytesReceived, err := getServiceBytesReceived(clientset, metricsClientset, service)
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		fmt.Printf("Error getting service bytes received for service %s/%s: %s\n", service.Namespace, service.Name, err.Error())
		return
	}

	// Eport to prometheus
	serviceBytesReceived.With(labels).Set(bytesReceived)
	fmt.Println(bytesReceived)

	// Get the total number of bytes transmitted by the service.
	bytesTransmitted, err := getServiceBytesTransmitted(clientset, metricsClientset, service)
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		fmt.Printf("Error getting service bytes transmitted for service %s/%s: %s\n", service.Namespace, service.Name, err.Error())
		return
	}

	// Eport to prometheus
	serviceBytesTransmitted.With(labels).Set(bytesTransmitted)
	fmt.Println(bytesTransmitted)
}

//==============================---Get-Service-Bytes-Received---============================

// TO DO: find a way to get pod rx and tx traffic metrics

func getServiceBytesReceived(clientset kubernetes.Interface, metricsClientset versioned.Interface, service *corev1.Service) (float64, error) {
	Endpoints, error := clientset.CoreV1().Endpoints(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
	if error != nil {
		return 0, error
	}

	for _, subset := range Endpoints.Subsets {
		for _, Addresses := range subset.Addresses {
			fmt.Println(Addresses.IP)
		}
	}

	return 1, nil
}

//==============================---Get-Service-Bytes-Transmitted---============================

func getServiceBytesTransmitted(clientset kubernetes.Interface, metricsClientset versioned.Interface, service *corev1.Service) (float64, error) {
	return 1, nil
}
