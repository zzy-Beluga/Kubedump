/*
Copyright © 2023 NAME HERE <EMAIL ADDRESS>
*/
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
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
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/scheme"
)

//===========================---global-var---========================

var (
	interval = time.Duration(20) * time.Second
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
	)
	serviceInformer := informerFactory.Core().V1().Services().Informer()

	// Create a stop channel for each service.
	stopChans := make(map[string]chan struct{})
	// Create a read-write lock for the stop channels map.
	stopChansLock := sync.RWMutex{}

	// Define the event handlers.
	serviceEventHandler := cache.ResourceEventHandlerFuncs{
		// when a service is added, start to collect metrics for that service
		AddFunc: func(obj interface{}) {

			stopChansLock.Lock()
			defer stopChansLock.Unlock()

			service := obj.(*corev1.Service)

			// Create a new stop channel for this service.
			stopCh := make(chan struct{})

			// Add the stop channel to the map.
			stopChans[service.Name] = stopCh

			go collectServiceNetworkMetrics(clientset, service, serviceBytesReceived, serviceBytesTransmitted, stopCh)
		},
		// when a service is updated, start to collect for the service with new status
		UpdateFunc: func(oldObj, newObj interface{}) {

			stopChansLock.Lock()
			defer stopChansLock.Unlock()

			service := newObj.(*corev1.Service)

			// Get the existing stop channel for this service.
			stopCh, ok := stopChans[service.Name]
			if ok {
				// Stop the existing goroutine by closing the stop channel.
				close(stopCh)
			}

			// Create a new stop channel for this service.
			stopCh = make(chan struct{})
			stopChans[service.Name] = stopCh

			go collectServiceNetworkMetrics(clientset, service, serviceBytesReceived, serviceBytesTransmitted, stopCh)
		},
		// delete exsiting goroutine on service deletion
		DeleteFunc: func(obj interface{}) {

			stopChansLock.Lock()
			defer stopChansLock.Unlock()

			service := obj.(*corev1.Service)
			// Get the existing stop channel for this service.
			stopCh, ok := stopChans[service.Name]
			if !ok {
				// If the stop channel doesn't exist, do nothing.
				return
			}

			// Stop the existing goroutine by closing the stop channel.
			close(stopCh)

			// Remove the stop channel from the map.
			delete(stopChans, service.Name)
		},
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
	http.ListenAndServe(":8081", nil)

}

//==============================---Collect-Service-Network-Metrics---============================

// TO DO: make the function a neverstop loop, fetch and calulate metrics periodically and stop when the service does not exist anymore
func collectServiceNetworkMetrics(clientset kubernetes.Interface, service *corev1.Service, serviceBytesReceived *prometheus.GaugeVec, serviceBytesTransmitted *prometheus.GaugeVec, stopCh <-chan struct{}) {

	// Create the Prometheus labels.
	labels := prometheus.Labels{
		"namespace": service.Namespace,
		"service":   service.Name,
	}

	// create ticker to make collection happens every 5 seconds
	ticker := time.NewTicker(5 * time.Second)

	// Get the total number of bytes received by the service.
	for {
		select {
		// stop collecting when service deleted
		case <-stopCh:
			log.Printf("Stopping Metrics collection for service %v in namespace %v", service.Name, service.Namespace)
		default:
			cmd := "cat /proc/net/dev | awk '{if (NR>2){sum+=$2}} END {printf(\"%d\", sum)}'"
			bytesReceived, err := getServiceBytes(clientset, service, cmd)
			if err != nil {
				if errors.IsNotFound(err) {
					return
				}
				fmt.Printf("Error getting service bytes received for service %s/%s: %s\n", service.Namespace, service.Name, err.Error())
				return
			}

			// Export to prometheus
			serviceBytesReceived.With(labels).Set(bytesReceived)
			fmt.Println(bytesReceived)

			// Get the total number of bytes transmitted by the service.
			cmd = "cat /proc/net/dev | awk '{if (NR>2){sum+=$10}} END {printf(\"%d\", sum)}'"
			bytesTransmitted, err := getServiceBytes(clientset, service, cmd)
			if err != nil {
				if errors.IsNotFound(err) {
					return
				}
				fmt.Printf("Error getting service bytes transmitted for service %s/%s: %s\n", service.Namespace, service.Name, err.Error())
				return
			}

			// Export to prometheus
			serviceBytesTransmitted.With(labels).Set(bytesTransmitted)
			fmt.Println(bytesTransmitted)

			// Waiting for next period
			<-ticker.C

		}
	}
}

//==============================---Get-Service-Bytes---============================

// TO DO: find a way to get pod rx and tx traffic metrics

func getServiceBytes(clientset kubernetes.Interface, service *corev1.Service, cmd string) (float64, error) {
	pods, err := getPodsbyService(clientset, service)
	if err != nil {
		return 0, err
	}
	var totalBytes float64
	for _, pod := range pods {
		Bytes, _ := readFromPod(clientset, &pod, cmd)
		totalBytes += Bytes
	}

	return totalBytes, nil
}

//===============================---utils---=================================

func getPodsbyService(clientset kubernetes.Interface, service *corev1.Service) ([]corev1.Pod, error) {
	Endpoints, _ := clientset.CoreV1().Endpoints(service.Name).Get(context.Background(), service.Name, metav1.GetOptions{})
	var ipset []string
	for _, subset := range Endpoints.Subsets {
		for _, Addresses := range subset.Addresses {
			ipset = append(ipset, Addresses.IP)
		}
	}

	var podLists []corev1.Pod

	// get pod list
	pods, err := clientset.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	// check pod ip
	for _, ip := range ipset {
		for _, pod := range pods.Items {
			if pod.Status.PodIP == ip {
				podLists = append(podLists, pod)
			}
		}
	}

	return podLists, nil
}

func readFromPod(clientset kubernetes.Interface, pod *corev1.Pod, cmd string) (float64, error) {

	// Load the Kubernetes configuration file
	path := os.Getenv("HOME") + "/.kube/config"
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		panic(err.Error())
	}

	podName := pod.Name
	namespace := pod.Namespace
	var totalBytes float64
	for _, container := range pod.Spec.Containers {
		containerName := container.Name
		// Create a stream to the container
		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Name(podName).
			Namespace(namespace).
			SubResource("exec").
			Param("container", containerName)

		req.VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   []string{"sh", "-c", cmd},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

		exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
		if err != nil {
			fmt.Printf("Error creating executor: %s\n", err)
			return 0, err
		}

		output := bytes.NewBuffer(nil)

		// Create a stream to the container
		err = exec.StreamWithContext(context.TODO(), remotecommand.StreamOptions{
			Stdout: output,
			Stderr: os.Stderr,
			Tty:    false,
		})
		if err != nil {
			fmt.Printf("Error streaming: %s\n", err)
			return 0, err
		}
		r, _ := strconv.ParseFloat(output.String(), 64)
		totalBytes += r
	}

	return totalBytes, nil
}
