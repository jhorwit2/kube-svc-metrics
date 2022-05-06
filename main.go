package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func main() {

	var err error
	var config *rest.Config

	if os.Getenv("USE_LOCAL") != "" {
		var kubeconfig *string
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
		} else {
			kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
		}
		flag.Parse()

		// use the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			panic(err.Error())
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	serviceListWatch := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"services",
		v1.NamespaceAll,
		fields.Everything(),
	)

	// never stop
	stop := make(chan struct{})
	defer close(stop)
	serviceIndexer, informer := cache.NewIndexerInformer(serviceListWatch, &v1.Service{}, 5*time.Minute, cache.ResourceEventHandlerFuncs{}, cache.Indexers{})
	go informer.Run(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		runtime.HandleError(errors.New("timed out waiting for caches to sync"))
		return
	}

	prometheus.MustRegister(newServiceCollector(serviceIndexer))

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":8080", nil)
}

type serviceCollector struct {
	serviceIndexer cache.Indexer
	serviceMetric  *prometheus.Desc
}

func newServiceCollector(serviceIndexer cache.Indexer) *serviceCollector {
	return &serviceCollector{
		serviceIndexer: serviceIndexer,
		serviceMetric: prometheus.NewDesc("kube_service_info_extended",
			"Extended information for services",
			[]string{"service", "namespace", "load_balancer_ip", "uid"}, nil,
		),
	}
}

func (c *serviceCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.serviceMetric
}

// Collect implements required collect function for all prometheus collectors
func (c *serviceCollector) Collect(ch chan<- prometheus.Metric) {
	for _, service := range c.serviceIndexer.List() {
		svc := service.(*v1.Service)
		if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
			continue
		}

		if len(svc.Status.LoadBalancer.Ingress) < 1 {
			// no ip so no log yet
			continue
		}

		ch <- prometheus.MustNewConstMetric(c.serviceMetric, prometheus.CounterValue, 1,
			svc.Name, svc.Namespace, svc.Status.LoadBalancer.Ingress[0].IP, string(svc.UID))
	}
}
