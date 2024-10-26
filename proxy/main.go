package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
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
	ID             string `json:"ID"`
	Source         string `json:"source"`
	MaxReaders     int64  `json:"maxReaders"`
	Fallback       string `json:"fallback,omitempty"`
	SourceOnDemand bool   `json:"source_on_demand"`
}

type ReplicaSetStatus struct {
	Replicas int `json:"replicas"`
}

type ReplicaSet struct {
	Status ReplicaSetStatus `json:"status"`
}

func addCamera(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	var data CameraEndpoint
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	totalInstances, err := getDeploymentInstances()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	hostname, err := os.ReadFile("/etc/hostname")
	if err != nil {
		http.Error(w, fmt.Sprintf("could not read hostname: %v", err), http.StatusBadRequest)
		return
	}

	err = createSts(data.Source)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ordinalIdx := string(hostname)[bytes.LastIndexByte(hostname, '-')+1:]

	// create a path config on the current instance
	_ = ordinalIdx

	// TODO: have a button in the UI when adding a camera for enabling Adaptive Bitrate Streaming for HLS
	//  as it is computationally expensive, do not include it by default
	/*
		runOnPublish: |
			ffmpeg -i rtsp://localhost:8554/{path} -map 0:v -map 0:a \
			-b:v:0 500k -maxrate:v:0 500k -bufsize:v:0 1000k -s:v:0 640x360 -c:v:0 libx264 \
			-b:v:1 1000k -maxrate:v:1 1000k -bufsize:v:1 2000k -s:v:1 1280x720 -c:v:1 libx264 \
			-b:v:2 2000k -maxrate:v:2 2000k -bufsize:v:2 4000k -s:v:2 1920x1080 -c:v:2 libx264 \
			-c:a aac -b:a 128k -f hls -hls_time 4 -hls_list_size 5 \
			-var_stream_map "v:0,a:0 v:1,a:1 v:2,a:2" \
			-master_pl_name master.m3u8 \
			-hls_segment_filename /{path}/stream_%v/segment_%d.ts \
			/{path}/stream_%v/playlist.m3u8
		runOnPublishRestart: yes
	*/
	// TODO: support only [rtsp, rtmp, hls, webrtc]
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
	addr, _ := url.ParseRequestURI("http://localhost:9997/v3/paths")

	proxy := httputil.NewSingleHostReverseProxy(addr)

	proxy.ServeHTTP(writer, request)
}

func getDeploymentInstances() (int32, error) {
	deploymentName := "mediamtx"
	config, err := rest.InClusterConfig()
	if err != nil {
		return -1, fmt.Errorf("failed to load in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return -1, fmt.Errorf("failed to create k8s client: %v", err)
	}

	deploymentsClient := clientset.AppsV1().Deployments("hub")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	deployment, err := deploymentsClient.Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return -1, fmt.Errorf("could not get Deployment %s: %v", deploymentName, err)
	}

	return deployment.Status.Replicas, nil
}

func createSts(cameraUrl string) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to load in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %v", err)
	}

	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ml-pipeline-{Camera_ID}",
			Namespace: "ml-analysis",
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
							// TODO: update name and image
							Name:  "nginx",
							Image: "nginx:1.19",
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 8080,
								},
							},
							Args: []string{
								cameraUrl,
								"behaviour",
								// the camera ID should be part of the notification webhook URL
								"notif-service.k8s.internal:8080/camera/1",
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

	statefulSetsClient := clientset.AppsV1().StatefulSets("ml-analysis")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := statefulSetsClient.Create(ctx, statefulSet, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create StatefulSet: %v", err)
	}

	fmt.Printf("Created StatefulSet %q\n", result.GetObjectMeta().GetName())
	return nil
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

	router.Post("/endpoints", addCamera)
	router.Get("/endpoints", getEndpoints)

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
