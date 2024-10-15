package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type CameraEndpoint struct {
	Name           string `json:"name"`
	Source         string
	MaxReaders     int64 `json:"maxReaders"`
	Fallback       string
	SourceOnDemand bool
}

type ReplicaSetStatus struct {
	Replicas int `json:"replicas"`
}

type ReplicaSet struct {
	Status ReplicaSetStatus `json:"status"`
}

func addCamera(w http.ResponseWriter, r *http.Request) {
	totalInstances, err := getReplicaSetInstances()
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}

	hostname, err := os.ReadFile("/etc/hostname")
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}

	// should this be here or in a different service?
	createSts("<camera RTSP url>")

	ordinalIdx := string(hostname)[bytes.LastIndexByte(hostname, '-')+1:]

	// create a path config on the current instance
	_ = ordinalIdx
	cameraPayload := strings.NewReader("{\"a\":\"test\"}")
	res, err := http.Post("http://localhost:9997/v3/config/paths/add/{path}", "application/json", cameraPayload)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	defer res.Body.Close()

	if res.StatusCode/100 != 2 {
		w.WriteHeader(res.StatusCode)
		return
	}

	// TODO: create redirect paths in the rest of the instaces
	_ = totalInstances

	w.WriteHeader(200)
}

func getEndpoints(writer http.ResponseWriter, request *http.Request) {
	res, err := http.Get("http://localhost:9997/v3/paths")
	if err != nil {
		http.Error(writer, "server error", 500)
		return
	}

	defer res.Body.Close()

	_, err = io.ReadAll(res.Body)
	if err != nil {
		http.Error(writer, "server error", 500)
		return
	}

	writer.WriteHeader(200)
}

func getEndpointEncodings(writer http.ResponseWriter, request *http.Request) {
	path := chi.URLParam(request, "path")

	res, err := http.Get("http://localhost:9997/v3/paths/get/" + path)
	if err != nil {
		http.Error(writer, "server error", 500)
		return
	}

	defer res.Body.Close()

	writer.WriteHeader(200)
}

func getReplicaSetInstances() (int, error) {
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		// TODO: wrap all errors so we know where they come from
		return -1, err
	}

	caCertPath := "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		return -1, err
	}

	namespace, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return -1, err
	}

	replicaSetName := "<replica-set-name>"

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: caCertPool,
		},
	}

	client := &http.Client{Transport: transport}

	// TODO: ensure the ServiceAccount has RBAC roles for getting and listing ReplcaSets
	apiURL := fmt.Sprintf(
		"https://kubernetes.default.svc/apis/apps/v1/namespaces/%s/replicasets/%s",
		string(namespace), replicaSetName,
	)

	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", string(token)))

	resp, err := client.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return -1, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return -1, err
	}

	var replicaSet ReplicaSet
	if err := json.Unmarshal(body, &replicaSet); err != nil {
		return -1, err
	}

	return replicaSet.Status.Replicas, nil
}

func createSts(cameraUrl string) {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to load in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example-statefulset",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    int32Ptr(1),
			ServiceName: "analyser-service",
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "analyser",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "analyser",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "nginx:1.19",
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 80,
								},
							},
							Args: []string{
								cameraUrl,
								"behaviour",
								// the camera ID should be part of the notification webhook URL
								"notif-service.k8s.internal:9090/camera/1",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "ACTIVITY_WHITELIST",
									Value: "1-30",
								},
								{
									Name:  "PYTHONUNBUFFERED",
									Value: "1",
								},
								{
									Name:  "LOG_LEVEL",
									Value: "DEBUG",
								},
							},
						},
					},
				},
			},
		},
	}

	statefulSetsClient := clientset.AppsV1().StatefulSets("default")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := statefulSetsClient.Create(ctx, statefulSet, metav1.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create StatefulSet: %v", err)
	}

	fmt.Printf("Created StatefulSet %q.\n", result.GetObjectMeta().GetName())
}

func int32Ptr(i int32) *int32 { return &i }

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix | log.LUTC)
	log.SetPrefix("[debug] ")

	log.Println("configuring server...")

	router := chi.NewRouter()

	router.Use(middleware.Recoverer)
	router.Use(middleware.CleanPath)
	router.Use(middleware.Heartbeat("/public/ping"))

	router.Post("/cameras", addCamera)
	router.Get("/cameras", getEndpoints)
	router.Get("/cameras/{path}", getEndpointEncodings)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           router,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	server.SetKeepAlivesEnabled(false)

	go func() {
		log.Println("starting server...")
		// TODO: make an self-signed SSL certificate
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, err.Error())
		}
	}()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	<-signalChan

	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()

	log.Println("shutting down server...")
	err := server.Shutdown(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
	}
}
