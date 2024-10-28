package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
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

type CameraEndpointRequest struct {
	ID                string `json:"ID"`
	AnalysisMode      string `json:"analysisMode"`
	Source            string `json:"source"`
	EnableTranscoding bool   `json:"enableTranscoding,omitempty"`
	MaxReaders        int64  `json:"maxReaders,omitempty"`
}

type CameraEndpointConfig struct {
	Path                string `json:"name"`
	Source              string `json:"source,omitempty"`
	SourceRedirect      string `json:"sourceRedirect,omitempty"`
	MaxReaders          int64  `json:"maxReaders,omitempty"`
	RunOnPublish        string `json:"runOnPublish,omitempty"`
	RunOnPublishRestart bool   `json:"runOnPublishRestart,omitempty"`
}

func addCamera(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()

	var data CameraEndpointRequest
	err := json.NewDecoder(request.Body).Decode(&data)
	if err != nil {
		http.Error(writer, "Invalid JSON", http.StatusBadRequest)
		return
	}

	parsedSourceURL, err := url.Parse(data.Source)
	if err != nil {
		http.Error(writer, fmt.Sprintf("Invalid Source URL %q: %v", data.Source, err), http.StatusBadRequest)
		return
	}

	// TODO: ignore 409 Conflict, as the camera endpoint may already be created
	err = createMlPipelineDeployment(data.Source, data.AnalysisMode, data.ID)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	totalInstances, err := getStatefulSetInstances()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	hostname := os.Getenv("HOSTNAME")

	currentInstanceIdxStr := hostname[strings.LastIndexByte(hostname, '-')+1:]
	currentInstanceIdx, _ := strconv.Atoi(currentInstanceIdxStr)

	var config CameraEndpointConfig
	config.Path = "camera-" + url.PathEscape(data.ID)
	config.MaxReaders = data.MaxReaders
	config.Source = data.Source
	if data.EnableTranscoding {
		config.RunOnPublishRestart = true
		config.RunOnPublish = "ffmpeg -i rtsp://localhost:8554/" + config.Path + " -map 0:v -map 0:a " +
			"-b:v:0 500k -maxrate:v:0 500k -bufsize:v:0 1000k -s:v:0 640x360 -c:v:0 libx264 " +
			"-b:v:1 1000k -maxrate:v:1 1000k -bufsize:v:1 2000k -s:v:1 1280x720 -c:v:1 libx264 " +
			"-b:v:2 2000k -maxrate:v:2 2000k -bufsize:v:2 4000k -s:v:2 1920x1080 -c:v:2 libx264 " +
			"-c:a aac -b:a 128k -f hls -hls_time 4 -hls_list_size 5 " +
			"-var_stream_map \"v:0,a:0 v:1,a:1 v:2,a:2\" " +
			"-master_pl_name master.m3u8 " +
			"-hls_segment_filename /" + config.Path + "/stream_%v/segment_%d.ts " +
			"/" + config.Path + "/stream_%v/playlist.m3u8"
	}

	err = sendConfigRequest(config, "localhost")
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	for i := 0; i < int(totalInstances); i++ {
		if i != currentInstanceIdx {
			config.Source = ""
			config.RunOnPublishRestart = false
			config.RunOnPublish = ""
			config.SourceRedirect = parsedSourceURL.Scheme + "://" + string(hostname) + ":" + parsedSourceURL.Port() + "/" + config.Path

			err = sendConfigRequest(config, changeHostnameSuffix(string(hostname), i))
			if err != nil {
				// should we stop or continue here?
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	writer.WriteHeader(http.StatusOK)
}

func sendConfigRequest(config CameraEndpointConfig, host string) error {
	cameraPayload, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("could not serialize config request: %v", err)
	}

	res, err := http.Post("http://"+host+":9997/v3/config/paths/add/"+config.Path, "application/json", bytes.NewReader(cameraPayload))
	if err != nil {
		return fmt.Errorf("could not send config request: %v", err)
	}
	defer res.Body.Close()

	// TODO: handle the case where the camera endpoint is already created
	if res.StatusCode/100 != 2 {
		bodyBytes, err := io.ReadAll(res.Body)
		if err != nil {
			return fmt.Errorf("could not read config response body: %v", err)
		}
		return fmt.Errorf("config response error: %s", string(bodyBytes))
	}
	return nil
}

func changeHostnameSuffix(hostname string, newSuffix int) string {
	re := regexp.MustCompile(`(\D+)(\d+)$`)
	matches := re.FindStringSubmatch(hostname)

	newHostname := matches[1] + strconv.Itoa(newSuffix)
	return fmt.Sprintf("%s.mediamtx-service.hub.svc.cluster.local", newHostname)
}

func getEndpoints(writer http.ResponseWriter, request *http.Request) {
	// this will not be the full config, just a summary
	addr, _ := url.ParseRequestURI("http://localhost:9997/v3/paths/list")

	proxy := httputil.NewSingleHostReverseProxy(addr)

	proxy.ServeHTTP(writer, request)
}

func getStatefulSetInstances() (int32, error) {
	statefulSet := "mediamtx"
	config, err := rest.InClusterConfig()
	if err != nil {
		return -1, fmt.Errorf("failed to load in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return -1, fmt.Errorf("failed to create k8s client: %v", err)
	}

	statefulSetsClient := clientset.AppsV1().StatefulSets("hub")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sts, err := statefulSetsClient.Get(ctx, statefulSet, metav1.GetOptions{})
	if err != nil {
		return -1, fmt.Errorf("could not get StatefulSet %s: %v", statefulSet, err)
	}

	return sts.Status.Replicas, nil
}

func createMlPipelineDeployment(cameraUrl, analysisMode, cameraID string) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to load in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %v", err)
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ml-pipeline-" + cameraID,
			Namespace: "ml-analysis",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
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
							Name:  "ml-pipeline",
							Image: "python:3.12-bookworm", // TODO: build a custom image
							Args: []string{
								cameraUrl,
								analysisMode,
								"notif-service.k8s.internal:8080/alert/" + cameraID,
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

	deplomentsClient := clientset.AppsV1().Deployments("ml-analysis")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := deplomentsClient.Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create Deployment: %v", err)
	}

	fmt.Printf("Created Deployment %q\n", result.GetObjectMeta().GetName())
	return nil
}

func int32Ptr(i int32) *int32 { return &i }

func deleteCameraEndpoint(writer http.ResponseWriter, request *http.Request) {
	cameraPath := chi.URLParam(request, "path")

	req, _ := http.NewRequest(http.MethodDelete, "http://localhost:9997/v3/config/paths/delete/"+cameraPath, nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		http.Error(writer, fmt.Sprintf("camera path %q not found", cameraPath), http.StatusNotFound)
		return
	}

	if resp.StatusCode == http.StatusInternalServerError {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(writer, fmt.Sprintf("could not read config response body: %v", err), http.StatusInternalServerError)
			return
		}
		http.Error(writer, fmt.Sprintf("config response error: %s", string(bodyBytes)), http.StatusInternalServerError)
		return
	}

	totalInstances, err := getStatefulSetInstances()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	hostname := os.Getenv("HOSTNAME")

	currentInstanceIdxStr := hostname[strings.LastIndexByte(hostname, '-')+1:]
	currentInstanceIdx, _ := strconv.Atoi(currentInstanceIdxStr)

	for i := 0; i < int(totalInstances); i++ {
		if i != currentInstanceIdx {
			req, _ = http.NewRequest(http.MethodDelete, "http://"+changeHostnameSuffix(string(hostname), i)+":9997/v3/config/paths/delete/"+cameraPath, nil)

			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusInternalServerError {
				bodyBytes, err := io.ReadAll(resp.Body)
				if err != nil {
					http.Error(writer, fmt.Sprintf("could not read config response body: %v", err), http.StatusInternalServerError)
					return
				}
				http.Error(writer, fmt.Sprintf("config response error: %s", string(bodyBytes)), http.StatusInternalServerError)
				return
			}
		}
	}

	writer.WriteHeader(http.StatusNoContent)
}

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
	router.Delete("/endpoints/{path}", deleteCameraEndpoint)

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
